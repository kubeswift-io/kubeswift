#!/usr/bin/env bash
#
# Egress credentials test: spec.network.egress.credentials keeps a secret
# value out of the guest. The guest holds a placeholder; the preset's proxy
# terminates TLS for the destination (with a leaf certificate from a
# per-launcher CA the guest trusts) and sets the header from a Secret.
#
# The controller must define a preset PRESET (default envoy) with a
# translator that supports credentials and a readiness probe (chart
# sandboxMaterialize.egressProxyPresets), for example Envoy with the
# kubeswift-egress-adapters envoy-translator v0.2.0 or later.
#
# Subcommands:
#   ./credentials.sh validate   # run the checks
#   ./credentials.sh cleanup    # delete the test namespace
#
# What "validate" checks:
#   1. Cold sandbox "cred": from the guest, DEST's basic-auth endpoint accepts
#      the placeholder (the proxy sent the real value); the same request with
#      another Host gets 403 from the proxy; an allowed host without a
#      credential (OTHER) still verifies against the image's own CA bundle;
#      the image bundle now ends with the sandbox's CA.
#   2. While it runs, the guest's whole RAM (the hypervisor's memfd, read
#      from the launcher) holds the placeholder (positive control) and ZERO
#      copies of the secret value or of the proxy's leaf private key.
#   3. Pool "warm" and a checkout on its slot: the credential works there
#      too (fresh material at checkout).
#   4. A credential whose Secret does not exist keeps the sandbox Pending
#      (Resolved=False, SecretNotFound) with no launcher.
#
# Prerequisites: a KVM node; internet egress; the preset above; DEST answers
# GET /basic-auth with 200 for "postman:password" and does not echo it.
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-sandbox-credentials}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.23}"
IMAGE="${IMAGE:-docker.io/curlimages/curl:8.11.1}"
PRESET="${PRESET:-envoy}"
DEST="${DEST:-postman-echo.com}"
OTHER="${OTHER:-example.com}"
TIMEOUT="${TIMEOUT:-300}"
# The value the proxy injects (base64 of DEST's fixed test account). It must
# never appear in guest memory.
VALUE="$(printf 'postman:password' | base64)"

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null
	log "deleted namespace $NS"
}

phase() { kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }

finished() {
	local deadline=$((SECONDS + TIMEOUT)) p
	while :; do
		p=$(phase "$1")
		case "$p" in Completed|Failed) echo "$p"; return ;; esac
		[ "$SECONDS" -lt "$deadline" ] || die "sandbox $1 still $p after ${TIMEOUT}s"
		sleep 1
	done
}

pod_of() { kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox="$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true; }

console() { kubectl -n "$NS" logs "$(pod_of "$1")" -c console 2>/dev/null || true; }

network() {
	cat <<EOF
  network:
    mode: proxied
    egress:
      allow:
      - {host: $DEST}
      - {host: $OTHER}
      proxy: {preset: $PRESET}
      credentials:
      - name: PM_AUTH
        secretKeyRef: {name: ${1:-pm}, key: auth}
        destinations: [$DEST]
        valueTemplate: "Basic {}"
EOF
}

# The guest-side checks; HOLD=1 keeps the workload alive for the RAM grep.
probes() {
	cat <<EOF
f=0
ok() { echo "PROBE-OK   \$1"; }
bad() { echo "PROBE-FAIL \$1"; f=\$((f+1)); }
body=\$(curl -s -m 10 https://$DEST/basic-auth -H "Authorization: Basic \$PM_AUTH")
case "\$body" in *'"authenticated":true'*) ok "credential accepted (placeholder swapped)";; *) bad "credential: \$body";; esac
code=\$(curl -s -o /dev/null -w '%{http_code}' -m 10 https://$DEST/basic-auth -H "Authorization: Basic \$PM_AUTH" -H "Host: $OTHER")
[ "\$code" = 403 ] && ok "another Host refused (403)" || bad "another Host: \$code"
curl -s -o /dev/null -m 10 https://$OTHER/ && ok "passthrough host verifies with the image bundle" || bad "passthrough host"
last=\$(grep -v -e '-----' /etc/kubeswift/egress/ca.crt | tail -1)
if [ -n "\$last" ] && tail -c 2048 /etc/ssl/certs/ca-certificates.crt | grep -qF "\$last"; then ok "image bundle ends with the sandbox CA"; else bad "image bundle"; fi
echo "PROBES-DONE \$f"
EOF
	if [ "${HOLD:-0}" = 1 ]; then echo 'sleep 600'; fi
	echo 'exit $f'
}

sandbox() { # sandbox NAME SPEC_LINES SCRIPT
	local script
	script=$(printf '%s' "$3" | sed 's/^/      /')
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata: {name: $1, namespace: $NS}
spec:
  image: $IMAGE
  kernelProfileRef: {name: sandbox}
  cpu: 1
  memory: 256Mi
$2
  command: [sh, -c]
  args:
    - |
$script
EOF
}

# count NEEDLE: occurrences in the guest RAM of sandbox "cred".
count() {
	kubectl -n "$NS" exec "$(pod_of cred)" -c launcher -- sh -c '
pid=$(pgrep -f cloud-hypervisor | head -1)
for fd in /proc/$pid/fd/*; do
	case "$(readlink "$fd")" in *memfd:ch_ram*) grep -a -o -F -- "$1" "$fd" | wc -l; exit 0 ;; esac
done
echo "no guest RAM memfd" >&2; exit 1' sh "$1"
}

validate() {
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" delete swiftsandbox --all --wait=true >/dev/null
	kubectl -n "$NS" create secret generic pm --from-literal=auth="$VALUE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: {name: sandbox, namespace: $NS}
spec: {profile: sandbox, kernelCmdline: console=ttyS0, ociRef: {image: "$KERNEL_IMAGE"}}
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/sandbox --timeout=300s >/dev/null ||
		die "SwiftKernel sandbox not Ready in $NS"

	# 1 + 2: cold sandbox, held for the RAM grep.
	sandbox cred "$(network)" "$(HOLD=1 probes)"
	local deadline=$((SECONDS + TIMEOUT)) out
	until out=$(console cred | grep -E '^PROBE'); printf '%s' "$out" | grep -q PROBES-DONE; do
		[ "$(phase cred)" != Failed ] || die "sandbox cred failed: $(kubectl -n "$NS" get swiftsandbox cred -o jsonpath='{.status.message}')"
		[ "$SECONDS" -lt "$deadline" ] || die "sandbox cred: no probe output after ${TIMEOUT}s"
		sleep 2
	done
	printf '%s\n' "$out" | sed 's/^/   [cred] /'
	printf '%s\n' "$out" | grep -q '^PROBE-FAIL' && die "sandbox cred: a guest check failed"

	local placeholder keyline n
	placeholder=$(kubectl -n "$NS" get configmap cred-runtime-intent -o jsonpath='{.data.egress-intent\.json}' |
		sed -n 's/.*"placeholder": "\([^"]*\)".*/\1/p' | head -1)
	[ -n "$placeholder" ] || die "no placeholder in the egress-intent file"
	keyline=$(kubectl -n "$NS" exec "$(pod_of cred)" -c launcher -- sh -c 'grep -v -e "-----" /run/kubeswift/egress/secrets/cert-0.key | head -1')
	[ -n "$keyline" ] || die "cannot read the proxy's leaf key"
	n=$(count "$placeholder"); log "guest RAM: placeholder $n (positive control)"
	[ "$n" -gt 0 ] || die "the placeholder is not in guest RAM: the grep proves nothing"
	n=$(count "$VALUE"); log "guest RAM: secret value $n"
	[ "$n" -eq 0 ] || die "the secret value is in guest RAM ($n)"
	n=$(count "$keyline"); log "guest RAM: proxy leaf key $n"
	[ "$n" -eq 0 ] || die "the proxy's private key is in guest RAM ($n)"
	local audit
	audit=$(kubectl -n "$NS" logs "$(pod_of cred)" -c egress-audit 2>&1 || true)
	[ -n "$(jq -c 'select(.kind=="credentials" and .event=="delivered" and .names==["PM_AUTH"])' <<<"$audit")" ] ||
		die "no credential delivery in the egress audit: $audit"
	! grep -qF -- "$VALUE" <<<"$audit" || die "the secret value is in the egress audit"
	log "egress audit records the delivery (names, hosts, serials), not the value"
	kubectl -n "$NS" delete swiftsandbox cred --wait=false >/dev/null

	# 3: warm pool and checkout.
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata: {name: warm, namespace: $NS}
spec:
  image: $IMAGE
  kernelProfileRef: {name: sandbox}
  cpu: 1
  memory: 256Mi
  minWarm: 1
$(network)
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftsandboxpool/warm --timeout="${TIMEOUT}s" >/dev/null ||
		die "pool warm not Ready: $(kubectl -n "$NS" get swiftsandboxpool warm -o jsonpath='{.status.conditions}')"
	sandbox checkout "$(network)
  poolRef: {name: warm}" "$(probes)"
	local p
	p=$(finished checkout)
	out=$(console checkout | grep -E '^PROBE-' || true)
	printf '%s\n' "$out" | sed 's/^/   [checkout] /'
	[ "$p" = Completed ] && ! printf '%s\n' "$out" | grep -q '^PROBE-FAIL' ||
		die "checkout: phase $p, $(kubectl -n "$NS" get swiftsandbox checkout -o jsonpath='{.status.message}')"
	[ "$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox=checkout -o jsonpath='{.items[0].metadata.labels.sandbox\.kubeswift\.io/pool}')" = warm ] ||
		die "checkout did not run on a warm slot"
	log "checkout on a warm slot: credential accepted"

	# 4: a missing Secret keeps the sandbox Pending, named (it starts once
	# the Secret exists), before any launcher is created.
	sandbox nosecret "$(network missing)" "true"
	kubectl -n "$NS" wait swiftsandbox/nosecret --timeout=60s \
		--for=jsonpath='{.status.conditions[?(@.type=="Resolved")].reason}'=SecretNotFound >/dev/null ||
		die "missing Secret: $(kubectl -n "$NS" get swiftsandbox nosecret -o jsonpath='{.status}')"
	[ "$(phase nosecret)" = Pending ] && [ -z "$(pod_of nosecret)" ] || die "missing Secret: phase $(phase nosecret), pod $(pod_of nosecret)"
	log "a missing credential Secret keeps the sandbox Pending (SecretNotFound), no launcher"
	log "PASS: the guest holds only the placeholder; the proxy sends the real value"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
