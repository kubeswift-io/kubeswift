#!/usr/bin/env bash
#
# Proxied-egress test: what a sandbox in network mode `proxied` (built-in
# broker) can and cannot reach.
#
# In mode proxied nothing the guest sends is forwarded: every TCP connection
# is redirected to the broker swiftletd runs in the launcher pod, which allows
# it by address (cidr and service rules) or by the TLS server name / HTTP Host
# (host rules, and only to an address the name resolves to). DNS answers only
# allowed names. The broker runs under its own uid, and the kernel applies the
# restricted rules to that uid, so even an allow rule naming a node or the API
# server address cannot reach it.
#
# A TCP connect always succeeds (the broker accepts it), so the probes are
# HTTP(S) requests and DNS lookups, not `nc -z`.
#
# Subcommands:
#   ./egress-proxied.sh validate   # run the checks
#   ./egress-proxied.sh cleanup    # delete the test namespace
#
# What "validate" does:
#   1. Starts an HTTP pod ("target") behind Service "app", and reads the node
#      addresses, the API server endpoints, the kubernetes ClusterIP and the
#      cluster DNS address.
#   2. Sandbox "proxied" allows host PUBLIC_HOST:443, host *.PUBLIC_HOST:443
#      and service app:8080, and probes from the guest:
#        reach: DNS for both names, https to both, http://app:8080
#        block: DNS for OTHER_HOST, DNS straight to the cluster resolver,
#               http to PUBLIC_HOST on port 80, https to OTHER_HOST at its own
#               address, https with PUBLIC_HOST's name at OTHER_HOST's address
#               (server name spoof), the metadata endpoint, every node's
#               kubelet, every API server endpoint, the kubernetes ClusterIP,
#               the target pod's own address
#      Its egress audit (kubectl logs -c egress-audit) is JSON lines with
#      the DNS queries, the blocked name and allow and deny decisions.
#   3. Sandbox "kernel" allows, as cidr rules, every node address, every API
#      server endpoint, the kubernetes ClusterIP and the target pod. Only the
#      target pod becomes reachable: the broker allows the rest by address and
#      the kernel drops its connections.
#   4. Pool "warm" (same allowlist as 2, one warm slot) and sandbox "checkout"
#      claiming it (same shape, so it must land on the warm slot): https to PUBLIC_HOST works, OTHER_HOST does not, from a
#      warm slot.
#   5. Asserts the launcher of "proxied" carries the platform deny list.
#
# Prerequisites:
#   - a node labeled kubeswift.io/kernel-node=true with KVM
#   - internet egress from pods; PUBLIC_HOST (default example.com, must also
#     answer for www.PUBLIC_HOST) and OTHER_HOST (default github.com) resolve
#     from where this script runs and from the cluster
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-sandbox-egress-proxied}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.22}"
IMAGE="${IMAGE:-docker.io/curlimages/curl:8.11.1}"
TARGET_IMAGE="${TARGET_IMAGE:-docker.io/library/busybox:1.36}"
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

# probe_script EXPECTATIONS: a guest shell script. Lines are
#   dns  NAME WANT HOST [SERVER]
#   http NAME WANT URL [curl args...]
# with WANT reach|block. Exits with the failure count.
probe_script() {
	cat <<'EOF'
f=0
res() { if [ "$1" = "$2" ]; then echo "PROBE-OK   $3 $1"; else echo "PROBE-FAIL $3 got $1, want $2"; f=$((f+1)); fi; }
dns() { n=$1; w=$2; shift 2; if nslookup "$@" >/dev/null 2>&1; then g=reach; else g=block; fi; res $g $w "$n"; }
http() { n=$1; w=$2; shift 2; if curl -sS -m 8 -o /dev/null "$@" 2>/dev/null; then g=reach; else g=block; fi; res $g $w "$n"; }
EOF
	printf '%s\n' "$1" | sed '/^$/d'
	echo 'exit $f'
}

# sandbox NAME SPEC_YAML SCRIPT (SPEC_YAML: lines under spec, 2-space indent)
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
	# A warm checkout runs in the slot's pod, named after the slot.
	pod=$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox="$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
	out=$(kubectl -n "$NS" logs "${pod:-$1}" -c console 2>&1 | grep -E '^PROBE-' || true)
	printf '%s\n' "$out" | sed "s/^/   [$1] /"
	[ -n "$out" ] || die "sandbox $1 $phase with no probe output: $(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.message}')"
	if printf '%s\n' "$out" | grep -q '^PROBE-FAIL' || [ "$phase" != Completed ]; then
		die "sandbox $1: egress not as expected (phase $phase)"
	fi
}

network() { # network ALLOW_LINES: a proxied network block with these allow entries
	printf '  network:\n    mode: proxied\n    egress:\n      allow:\n%s\n' "$(printf '%s\n' "$1" | sed '/^$/d; s/^/      - /')"
}

validate() {
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" delete swiftsandbox --all --wait=true >/dev/null
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: {name: sandbox, namespace: $NS}
spec: {profile: sandbox, kernelCmdline: console=ttyS0, ociRef: {image: "$KERNEL_IMAGE"}}
---
apiVersion: v1
kind: Pod
metadata: {name: target, namespace: $NS, labels: {app: target}}
spec:
  containers:
  - name: http
    image: $TARGET_IMAGE
    command: [sh, -c, "mkdir -p /tmp/www && echo ok > /tmp/www/index.html && exec httpd -f -p 8080 -h /tmp/www"]
    securityContext: {runAsNonRoot: true, runAsUser: 65534, allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
---
apiVersion: v1
kind: Service
metadata: {name: app, namespace: $NS}
spec: {selector: {app: target}, ports: [{port: 8080, protocol: TCP}]}
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/sandbox --timeout=300s >/dev/null ||
		die "SwiftKernel sandbox not Ready in $NS"
	kubectl -n "$NS" wait --for=condition=Ready pod/target --timeout=120s >/dev/null || die "target pod not Ready"

	local target nodes apis cluster_ip dns other_ip
	target=$(kubectl -n "$NS" get pod target -o jsonpath='{.status.podIP}')
	nodes=$(kubectl get nodes -o jsonpath='{range .items[*]}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{range .status.addresses[?(@.type=="ExternalIP")]}{.address}{"\n"}{end}{end}' | grep -E '^[0-9.]+$' | sort -u)
	apis=$(kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]}{"\n"}{end}' | sort -u)
	cluster_ip=$(kubectl get service kubernetes -n default -o jsonpath='{.spec.clusterIP}')
	dns=$(kubectl -n kube-system get service -l k8s-app=kube-dns -o jsonpath='{.items[0].spec.clusterIP}')
	other_ip=$(getent ahostsv4 "$OTHER_HOST" | awk 'NR==1{print $1}')
	[ -n "$other_ip" ] || die "cannot resolve $OTHER_HOST here"
	log "$(echo "$nodes" | wc -w) node address(es), $(echo "$apis" | wc -w) API server endpoint(s), PUBLIC_HOST=$PUBLIC_HOST OTHER_HOST=$OTHER_HOST"

	local guarded="http metadata block http://169.254.169.254/
http apiserver-clusterip block -k https://$cluster_ip/"
	for n in $nodes; do guarded+="
http node-kubelet-$n block -k https://$n:10250/"; done
	for a in $apis; do guarded+="
http apiserver-$a block -k https://$a:6443/"; done

	local allow="{host: $PUBLIC_HOST, ports: [{port: 443}]}
{host: \"*.$PUBLIC_HOST\", ports: [{port: 443}]}
{service: {name: app}, ports: [{port: 8080}]}"

	run proxied "$(network "$allow")" "dns dns-allowed reach $PUBLIC_HOST
dns dns-wildcard reach www.$PUBLIC_HOST
dns dns-other block $OTHER_HOST
dns dns-direct-to-cluster-resolver block $OTHER_HOST $dns
http https-allowed reach https://$PUBLIC_HOST/
http https-wildcard reach https://www.$PUBLIC_HOST/
http http-port-80 block http://$PUBLIC_HOST/
http https-other block --resolve $OTHER_HOST:443:$other_ip https://$OTHER_HOST/
http sni-spoof block -k --resolve $PUBLIC_HOST:443:$other_ip https://$PUBLIC_HOST/
http service-by-name reach http://app:8080/
http target-pod-address block http://$target:8080/
$guarded"

	local pod deny
	pod=$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox=proxied -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
	if [ -n "$pod" ]; then
		deny=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.metadata.annotations.sandbox\.kubeswift\.io/egress-deny}')
		for n in $nodes $apis; do
			grep -qwF "$n" <<<"$deny" || die "egress-deny annotation on $pod lacks $n"
		done
		log "launcher of 'proxied' carries the platform deny list"
		# The egress audit (kubectl logs -c egress-audit): JSON lines for the
		# guest's DNS queries and the broker's decisions.
		local audit want
		audit=$(kubectl -n "$NS" logs "$pod" -c egress-audit 2>&1 || true)
		for want in '"kind":"dns","event":"query"' '"kind":"dns","event":"blocked"' \
			"\"name\":\"$OTHER_HOST\"" '"kind":"connection","event":"allow"' '"kind":"connection","event":"deny"'; do
			grep -qF "$want" <<<"$audit" || die "egress audit of $pod lacks $want: $(head -c 2000 <<<"$audit")"
		done
		while IFS= read -r line; do
			[ -z "$line" ] || jq -e . >/dev/null 2>&1 <<<"$line" || die "egress audit line is not JSON: $line"
		done <<<"$audit"
		log "egress audit of 'proxied': $(grep -c . <<<"$audit") JSON lines (DNS queries, blocked names, allow and deny decisions)"
	fi

	local cidrs="{cidr: $target/32}
{cidr: $cluster_ip/32}"
	for n in $nodes $apis; do cidrs+="
{cidr: $n/32}"; done
	run kernel "$(network "$cidrs")" "http target-pod-by-cidr reach http://$target:8080/
$guarded"

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
$(network "$allow")
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftsandboxpool/warm --timeout="${TIMEOUT}s" >/dev/null ||
		die "pool warm has no ready slot: $(kubectl -n "$NS" get swiftsandboxpool warm -o jsonpath='{.status.conditions}')"
	# A checkout claims a slot only with the pool's whole shape, network included.
	run checkout "$(network "$allow")
  poolRef: {name: warm}" "http https-allowed reach https://$PUBLIC_HOST/
http https-other block --resolve $OTHER_HOST:443:$other_ip https://$OTHER_HOST/"
	local claimed
	claimed=$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox=checkout -o jsonpath='{.items[0].metadata.labels.sandbox\.kubeswift\.io/pool}' 2>/dev/null || true)
	[ "$claimed" = warm ] || die "checkout did not run on a warm slot: $(kubectl -n "$NS" get swiftsandbox checkout -o jsonpath='{.status.conditions}')"
	log "checkout ran on a warm slot of pool warm"

	log "PASS: proxied egress allows only the allowlist, by name and address, and never nodes or the API server"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
