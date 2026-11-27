#!/usr/bin/env bash
#
# Egress proxy preset test: spec.network.egress.proxy.preset runs an
# administrator-defined proxy, with a translator that turns the
# egress-intent file into the proxy's configuration (the Envoy translator
# from github.com/kubeswift-io/kubeswift-egress-adapters, unmodified Envoy).
#
# The controller must define two presets (chart
# sandboxMaterialize.egressProxyPresets):
#   PRESET (default envoy): Envoy + the Envoy translator, listener 15006
#   BAD_PRESET (default unlabelled): a translator image without the
#     io.kubeswift.egress-intent.versions label
#
# Subcommands:
#   ./egress-preset.sh validate   # run the checks
#   ./egress-preset.sh cleanup    # delete the test namespace
#
# What "validate" checks, from the guest:
#   1. Sandbox "preset" allows host PUBLIC_HOST (every port, so TLS 443 and
#      HTTP 80): https and http to it work; https to OTHER_HOST at its own
#      address and http with OTHER_HOST's Host are refused; a TLS connection
#      to OTHER_HOST's address naming PUBLIC_HOST lands on PUBLIC_HOST's own
#      address (same response), never on OTHER_HOST; every node's kubelet,
#      the API server and the metadata endpoint are blocked.
#   2. Pool "warm" with the same preset, and a checkout on its warm slot.
#   3. A sandbox naming BAD_PRESET fails with EgressIntentUnsupported.
#
# Prerequisites: a KVM node; internet egress; the two presets above.
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-sandbox-egress-preset}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.25}"
IMAGE="${IMAGE:-docker.io/curlimages/curl:8.11.1}"
PRESET="${PRESET:-envoy}"
BAD_PRESET="${BAD_PRESET:-unlabelled}"
PUBLIC_HOST="${PUBLIC_HOST:-example.com}"
OTHER_HOST="${OTHER_HOST:-github.com}"
TIMEOUT="${TIMEOUT:-300}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null
	log "deleted namespace $NS"
}

finished() {
	local deadline=$((SECONDS + TIMEOUT)) phase
	while :; do
		phase=$(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.phase}')
		case "$phase" in Completed|Failed) echo "$phase"; return ;; esac
		[ "$SECONDS" -lt "$deadline" ] || die "sandbox $1 still $phase after ${TIMEOUT}s"
		sleep 1
	done
}

probe_script() {
	cat <<'EOF'
f=0
empty=$(printf '' | md5sum)
res() { if [ "$1" = "$2" ]; then echo "PROBE-OK   $3 $1"; else echo "PROBE-FAIL $3 got $1, want $2"; f=$((f+1)); fi; }
# code NAME WANT URL: the HTTP status (000: no response).
code() { g=$(curl -s -m 8 -o /dev/null -w '%{http_code}' "$3"); res "$g" "$2" "$1"; }
http() { n=$1; w=$2; shift 2; if curl -sS -m 8 -o /dev/null "$@" 2>/dev/null; then g=reach; else g=block; fi; res $g $w "$n"; }
# spoof NAME URL HOST IP: the response for URL is the same whether HOST
# resolves normally or is forced to IP, so the proxy went to HOST's address.
spoof() { a=$(curl -s -m 8 "$2" | md5sum); b=$(curl -sk -m 8 --resolve "$3:443:$4" "$2" | md5sum)
  if [ "$a" != "$empty" ] && [ "$a" = "$b" ]; then echo "PROBE-OK   $1 went to the name's address"; else echo "PROBE-FAIL $1 reached another address"; f=$((f+1)); fi; }
EOF
	printf '%s\n' "$1" | sed '/^$/d'
	echo 'exit $f'
}

network() { # network PRESET
	cat <<EOF
  network:
    mode: proxied
    egress:
      allow:
      - {host: $PUBLIC_HOST}
      proxy: {preset: $1}
EOF
}

# sandbox NAME SPEC_LINES SCRIPT
sandbox() {
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

run() {
	kubectl -n "$NS" delete swiftsandbox "$1" --ignore-not-found --wait=true >/dev/null
	sandbox "$1" "$2" "$(probe_script "$3")"
	local phase out pod
	phase=$(finished "$1")
	pod=$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox="$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
	out=$(kubectl -n "$NS" logs "${pod:-$1}" -c console 2>&1 | grep -E '^PROBE-' || true)
	printf '%s\n' "$out" | sed "s/^/   [$1] /"
	[ -n "$out" ] || die "sandbox $1 $phase with no probe output: $(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.message}')"
	if printf '%s\n' "$out" | grep -q '^PROBE-FAIL' || [ "$phase" != Completed ]; then
		die "sandbox $1: egress not as expected (phase $phase)"
	fi
}

validate() {
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" delete swiftsandbox --all --wait=true >/dev/null
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: {name: sandbox, namespace: $NS}
spec: {profile: sandbox, kernelCmdline: console=ttyS0, ociRef: {image: "$KERNEL_IMAGE"}}
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/sandbox --timeout=300s >/dev/null ||
		die "SwiftKernel sandbox not Ready in $NS"

	local nodes apis cluster_ip other_ip guarded
	nodes=$(kubectl get nodes -o jsonpath='{range .items[*]}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{range .status.addresses[?(@.type=="ExternalIP")]}{.address}{"\n"}{end}{end}' | grep -E '^[0-9.]+$' | sort -u)
	apis=$(kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]}{"\n"}{end}' | sort -u)
	cluster_ip=$(kubectl get service kubernetes -n default -o jsonpath='{.spec.clusterIP}')
	other_ip=$(getent ahostsv4 "$OTHER_HOST" | awk 'NR==1{print $1}')
	[ -n "$other_ip" ] || die "cannot resolve $OTHER_HOST here"
	# -f: on port 80 Envoy itself answers a Host it does not allow with 403.
	guarded="http metadata block -f http://169.254.169.254/
code metadata-envoy-403 403 http://169.254.169.254/
http apiserver-clusterip block -k https://$cluster_ip/"
	for n in $nodes; do guarded+="
http node-kubelet-$n block -k https://$n:10250/"; done
	for a in $apis; do guarded+="
http apiserver-$a block -k https://$a:6443/"; done

	run preset "$(network "$PRESET")" "http https-allowed reach https://$PUBLIC_HOST/
http http-allowed reach http://$PUBLIC_HOST/
http https-other block --resolve $OTHER_HOST:443:$other_ip https://$OTHER_HOST/
http http-other-host block -f --resolve $OTHER_HOST:80:$other_ip http://$OTHER_HOST/
spoof sni-spoof https://$PUBLIC_HOST/ $PUBLIC_HOST $other_ip
$guarded"
	local pod
	pod=$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox=preset -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
	if [ -n "$pod" ]; then
		kubectl -n "$NS" logs "$pod" -c egress-translator 2>&1 | sed 's/^/   [translator] /' | head -3
	fi

	kubectl -n "$NS" delete swiftsandbox checkout --ignore-not-found --wait=true >/dev/null
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
$(network "$PRESET")
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftsandboxpool/warm --timeout="${TIMEOUT}s" >/dev/null ||
		die "pool warm not Ready: $(kubectl -n "$NS" get swiftsandboxpool warm -o jsonpath='{.status.conditions}')"
	run checkout "$(network "$PRESET")
  poolRef: {name: warm}" "http https-allowed reach https://$PUBLIC_HOST/
http https-other block --resolve $OTHER_HOST:443:$other_ip https://$OTHER_HOST/"
	local claimed
	claimed=$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox=checkout -o jsonpath='{.items[0].metadata.labels.sandbox\.kubeswift\.io/pool}' 2>/dev/null || true)
	[ "$claimed" = warm ] || die "checkout did not run on a warm slot"
	log "checkout ran on a warm slot of pool warm"

	kubectl -n "$NS" delete swiftsandbox unlabelled --ignore-not-found --wait=true >/dev/null
	sandbox unlabelled "$(network "$BAD_PRESET")" "true"
	local phase status
	phase=$(finished unlabelled)
	status=$(kubectl -n "$NS" get swiftsandbox unlabelled -o jsonpath='{.status}')
	[ "$phase" = Failed ] && grep -q EgressIntentUnsupported <<<"$status" ||
		die "a translator without the version label: phase $phase, status $status"
	log "a translator without io.kubeswift.egress-intent.versions fails the sandbox (EgressIntentUnsupported)"
	log "PASS: the preset's translator configures an unmodified proxy from the egress-intent file"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
