#!/usr/bin/env bash
#
# Egress proxy conformance: what a SwiftSandbox in network mode `proxied`
# can and cannot reach, run against the built-in broker or a proxy preset.
# Every proxy KubeSwift runs for a sandbox (the broker, an Envoy, agentgateway
# or OpenShell preset, an explicit image) must pass it.
#
# The image ghcr.io/kubeswift-io/kubeswift/egress-conformance:<version> holds
# this driver and is also the guest the sandboxes boot (alpine, root, with
# curl, dig, nc, ping, ntpd and /conformance/probe-lib.sh), so the probes run
# as root inside the microVM, as a hostile workload would.
#
# Subcommands:
#   egress-conformance.sh validate   # run the checks
#   egress-conformance.sh cleanup    # delete the test namespace
#
# Environment:
#   PROXY        broker (default) or the name of a preset
#                (chart sandboxMaterialize.egressProxyPresets)
#   CREDENTIALS  1: also run the credentials suite (needs a preset)
#   SPIFFE       1: also run the SPIFFE suite (needs SPIRE and the chart's
#                sandboxMaterialize.spiffe)
#   IMAGE        the guest image (default: this image, CONFORMANCE_IMAGE)
#   PUBLIC_HOST  an allowed name answering on 80 and 443, also as www.<name>
#                (default example.com)
#   OTHER_HOST   a name never allowed, with SSH on 22 (default github.com)
#   NTP_HOST     an NTP server (default time.cloudflare.com)
#
# What "validate" checks:
#   1. Sandbox "battery" allows host PUBLIC_HOST (443 and 80), host
#      *.PUBLIC_HOST (443) and service app:8080:
#        reach: DNS for both names; https (by server name) and http (by Host)
#               to them; http://app:8080
#        block: DNS for OTHER_HOST, through the cluster resolver, through a
#               public resolver over UDP and TCP, and a TXT lookup (DNS
#               tunnel); https to OTHER_HOST's address with and without its
#               name; http with OTHER_HOST's Host; SSH to OTHER_HOST:22 (another
#               port); ICMP; NTP (UDP); IPv6; the metadata endpoint; every
#               node's kubelet; every API server endpoint; the kubernetes
#               ClusterIP; a Service fronting a node; the target pod's address
#        spoof: PUBLIC_HOST's name sent to OTHER_HOST's address (server name,
#               and Host) is refused or lands on PUBLIC_HOST's own address
#   2. Sandbox "kernel" allows, as cidr rules (and, for a preset, as
#      proxy.upstreams), every node address, every API server endpoint, the
#      kubernetes ClusterIP and the target pod: only the target pod becomes
#      reachable. The proxy forwards by address and the kernel drops its
#      connections (the proxy uid chain).
#   3. A sandbox allowing the Service that fronts a node does not start
#      (EgressServiceReachesNode).
#   4. EgressProxyReady is True; the egress audit carries the DNS queries (and,
#      for the broker, its allow and deny decisions).
#   5. Pool "warm" and a checkout on its warm slot: the allowlist holds there.
#   6. With CREDENTIALS=1 / SPIFFE=1, the suites in /conformance/suites.
#
# Prerequisites: a node labeled kubeswift.io/kernel-node=true with KVM;
# internet egress from pods; for a preset, the preset defined in the chart.
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
PROXY="${PROXY:-broker}"
NS="${NS:-egress-conformance}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.22}"
IMAGE="${IMAGE:-${CONFORMANCE_IMAGE:-}}"
TARGET_IMAGE="${TARGET_IMAGE:-docker.io/library/busybox:1.36}"
PUBLIC_HOST="${PUBLIC_HOST:-example.com}"
OTHER_HOST="${OTHER_HOST:-github.com}"
NTP_HOST="${NTP_HOST:-time.cloudflare.com}"
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

# network ALLOW_LINES [UPSTREAM_LINES]: a proxied network block with these
# allow entries. A preset's proxy reaches private addresses only through
# proxy.upstreams (its uid chain), so in-cluster destinations are listed there
# too; the built-in broker needs no upstreams.
network() {
	printf '  network:\n    mode: proxied\n    egress:\n      allow:\n%s\n' "$(printf '%s\n' "$1" | sed '/^$/d; s/^/      - /')"
	[ "$PROXY" = broker ] && return
	printf '      proxy:\n        preset: %s\n' "$PROXY"
	[ -z "${2:-}" ] || printf '        upstreams:\n%s\n' "$(printf '%s\n' "$2" | sed '/^$/d; s/^/        - /')"
}

# sandbox NAME SPEC_LINES PROBES (PROBES: probe-lib.sh calls, one per line)
sandbox() {
	local script
	script=$(printf '. /conformance/probe-lib.sh\n%s\necho "PROBE-END $f"\nexit $f\n' "$(printf '%s\n' "$3" | sed '/^$/d')" | sed 's/^/      /')
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

launcher() { kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox="$1" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true; }

# run NAME SPEC_LINES PROBES: boots the sandbox and checks its probes. The
# console is read while the launcher runs: a warm slot's pod goes away once
# its checkout completes. RAN_POOL is the pool label of the pod it ran in.
RAN_POOL=""
run() {
	kubectl -n "$NS" delete swiftsandbox "$1" --ignore-not-found --wait=true >/dev/null
	sandbox "$1" "$2" "$3"
	local deadline=$((SECONDS + TIMEOUT)) phase="" out="" pod=""
	RAN_POOL=""
	while :; do
		[ -n "$pod" ] || pod=$(launcher "$1")
		if [ -n "$pod" ]; then
			[ -n "$RAN_POOL" ] || RAN_POOL=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.metadata.labels.sandbox\.kubeswift\.io/pool}' 2>/dev/null || true)
			out=$(kubectl -n "$NS" logs "$pod" -c console 2>/dev/null | grep -E '^PROBE-' || true)
			grep -q '^PROBE-END' <<<"$out" && break
		fi
		phase=$(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.phase}')
		case "$phase" in Completed|Failed) break ;; esac
		[ "$SECONDS" -lt "$deadline" ] || die "sandbox $1 still $phase after ${TIMEOUT}s"
		sleep 2
	done
	phase=$(finished "$1")
	if ! grep -q '^PROBE-END' <<<"$out"; then
		[ -n "$pod" ] || pod=$(launcher "$1")
		[ -z "$pod" ] || out=$(kubectl -n "$NS" logs "$pod" -c console 2>/dev/null | grep -E '^PROBE-' || true)
	fi
	printf '%s\n' "$out" | grep -v '^PROBE-END' | sed "s/^/   [$1] /"
	grep -q '^PROBE-END' <<<"$out" || die "sandbox $1 $phase without finishing its probes: $(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.message}')"
	if grep -q '^PROBE-FAIL' <<<"$out" || [ "$phase" != Completed ]; then
		die "sandbox $1: egress not as expected (phase $phase)"
	fi
}

proxy_ready() {
	local cond
	cond=$(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{range .status.conditions[?(@.type=="EgressProxyReady")]}{.status} {.reason}: {.message}{end}')
	case "$cond" in True\ *) log "$1: EgressProxyReady $cond" ;; *) die "$1: EgressProxyReady is '$cond'" ;; esac
}

suite() { # suite NAME: a feature suite shipped next to this driver
	local s="$HERE/suites/$1.sh"
	[ -x "$s" ] || s="$HERE/../../sandbox/$1.sh"
	[ -x "$s" ] || die "suite $1 not found"
	log "suite $1 (PRESET=$PROXY)"
	PRESET="$PROXY" "$s" validate
}

validate() {
	[ -n "$IMAGE" ] || die "IMAGE (or CONFORMANCE_IMAGE) must name the egress-conformance image"
	log "proxy: $PROXY, guest image: $IMAGE"
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" delete swiftsandbox --all --wait=true >/dev/null
	kubectl -n "$NS" delete swiftsandboxpool --all --wait=true >/dev/null

	local nodes node1 apis cluster_ip dns other_ip ntp_ip target
	nodes=$(kubectl get nodes -o jsonpath='{range .items[*]}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{range .status.addresses[?(@.type=="ExternalIP")]}{.address}{"\n"}{end}{end}' | grep -E '^[0-9.]+$' | sort -u)
	node1=$(head -n1 <<<"$nodes")
	apis=$(kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]}{"\n"}{end}' | sort -u)
	cluster_ip=$(kubectl get service kubernetes -n default -o jsonpath='{.spec.clusterIP}')
	dns=$(kubectl -n kube-system get service -l k8s-app=kube-dns -o jsonpath='{.items[0].spec.clusterIP}')
	other_ip=$(getent ahostsv4 "$OTHER_HOST" | awk 'NR==1{print $1}')
	ntp_ip=$(getent ahostsv4 "$NTP_HOST" | awk 'NR==1{print $1}')
	[ -n "$other_ip" ] && [ -n "$ntp_ip" ] || die "cannot resolve $OTHER_HOST or $NTP_HOST here"

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
---
# A Service with no selector whose endpoint is a node's kubelet.
apiVersion: v1
kind: Service
metadata: {name: node-front, namespace: $NS}
spec: {ports: [{port: 10250, protocol: TCP}]}
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata: {name: node-front, namespace: $NS, labels: {kubernetes.io/service-name: node-front}}
addressType: IPv4
endpoints: [{addresses: ["$node1"]}]
ports: [{port: 10250, protocol: TCP}]
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/sandbox --timeout=300s >/dev/null ||
		die "SwiftKernel sandbox not Ready in $NS"
	kubectl -n "$NS" wait --for=condition=Ready pod/target --timeout=120s >/dev/null || die "target pod not Ready"
	target=$(kubectl -n "$NS" get pod target -o jsonpath='{.status.podIP}')
	local front_ip
	front_ip=$(kubectl -n "$NS" get service node-front -o jsonpath='{.spec.clusterIP}')
	log "$(wc -w <<<"$nodes") node address(es), $(wc -w <<<"$apis") API server endpoint(s), PUBLIC_HOST=$PUBLIC_HOST OTHER_HOST=$OTHER_HOST"

	# 1. The battery.
	local guarded="http metadata block -f http://169.254.169.254/
http apiserver-clusterip block -k https://$cluster_ip/
http node-front-service block -k https://$front_ip:10250/"
	for n in $nodes; do guarded+="
http node-kubelet-$n block -k https://$n:10250/"; done
	for a in $apis; do guarded+="
http apiserver-$a block -k https://$a:6443/"; done

	local allow="{host: $PUBLIC_HOST, ports: [{port: 443}, {port: 80}]}
{host: \"*.$PUBLIC_HOST\", ports: [{port: 443}]}
{service: {name: app}, ports: [{port: 8080}]}"
	local upstreams="{service: {name: app}, ports: [{port: 8080}]}"

	run battery "$(network "$allow" "$upstreams")" "dns dns-allowed reach $PUBLIC_HOST
dns dns-wildcard reach www.$PUBLIC_HOST
http https-allowed reach -f https://$PUBLIC_HOST/
http https-wildcard reach -f https://www.$PUBLIC_HOST/
http http-allowed-by-host reach -f http://$PUBLIC_HOST/
http service-by-name reach -f http://app:8080/
dns dns-other block $OTHER_HOST
dns dns-via-cluster-resolver block $OTHER_HOST $dns
digq dns-public-resolver-udp block @1.1.1.1 $OTHER_HOST
digq dns-public-resolver-tcp block +tcp @1.1.1.1 $OTHER_HOST
digq dns-tunnel-txt block TXT conformance-\$\$.$OTHER_HOST
http https-other block --resolve $OTHER_HOST:443:$other_ip https://$OTHER_HOST/
http https-other-by-address block -k https://$other_ip/
http http-other-host block -f --resolve $OTHER_HOST:80:$other_ip http://$OTHER_HOST/
http http-allowed-name-port-8080 block -f http://$PUBLIC_HOST:8080/
banner ssh-other-port block $other_ip 22 SSH
icmp icmp block $other_ip
icmp icmp6 block 2606:4700:4700::1111 -6
ntp ntp-udp block $ntp_ip
http ipv6 block -k -g https://[2606:4700:4700::1111]/
spoof sni-spoof https $PUBLIC_HOST $other_ip
spoof host-spoof http $PUBLIC_HOST $other_ip
http target-pod-address block -f http://$target:8080/
$guarded"
	proxy_ready battery

	# 4. The egress audit of the battery's launcher.
	local pod audit sel sels
	pod=$(launcher battery)
	if [ -n "$pod" ]; then
		audit=$(kubectl -n "$NS" logs "$pod" -c egress-audit 2>&1 || true)
		while IFS= read -r line; do
			[ -z "$line" ] || jq -e . >/dev/null 2>&1 <<<"$line" || die "egress audit line is not JSON: $line"
		done <<<"$audit"
		sels=('.kind=="dns" and .event=="query"' ".kind==\"dns\" and .event==\"blocked\" and .name==\"$OTHER_HOST\"")
		[ "$PROXY" != broker ] || sels+=('.kind=="connection" and .event=="allow"' '.kind=="connection" and .event=="deny"')
		for sel in "${sels[@]}"; do
			[ -n "$(jq -c "select($sel)" <<<"$audit")" ] || die "egress audit of $pod has no event with $sel"
		done
		log "egress audit of battery: $(grep -c . <<<"$audit") JSON lines"
	else
		log "battery's launcher already gone: audit not checked"
	fi

	# 2. The proxy uid chain.
	local cidrs="{cidr: $target/32}
{cidr: $cluster_ip/32}"
	for n in $nodes $apis; do cidrs+="
{cidr: $n/32}"; done
	# For a preset the same addresses are also its upstreams: the deny list
	# comes first in the proxy's uid chain, so even an upstream cannot open
	# a node or the API server.
	run kernel "$(network "$cidrs" "$cidrs")" "http target-pod-address reach -f http://$target:8080/
$guarded"

	# 3. A Service fronting a node is refused before any launcher.
	kubectl -n "$NS" delete swiftsandbox svc-node --ignore-not-found --wait=true >/dev/null
	sandbox svc-node "$(network "{service: {name: node-front}, ports: [{port: 10250}]}")" "true"
	local deadline=$((SECONDS + 60)) why=""
	while [ "$SECONDS" -lt "$deadline" ]; do
		why=$(kubectl -n "$NS" get swiftsandbox svc-node -o jsonpath='{.status.conditions[*].reason}')
		grep -qw EgressServiceReachesNode <<<"$why" && break
		sleep 2
	done
	grep -qw EgressServiceReachesNode <<<"$why" || die "svc-node: no EgressServiceReachesNode (reasons: $why)"
	[ -z "$(launcher svc-node)" ] || die "svc-node has a launcher"
	kubectl -n "$NS" delete swiftsandbox svc-node --wait=false >/dev/null
	log "a Service fronting a node waits (EgressServiceReachesNode), no launcher"

	# 5. Warm pool and checkout.
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
$(network "$allow" "$upstreams")
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftsandboxpool/warm --timeout="${TIMEOUT}s" >/dev/null ||
		die "pool warm has no ready slot: $(kubectl -n "$NS" get swiftsandboxpool warm -o jsonpath='{.status.conditions}')"
	run checkout "$(network "$allow" "$upstreams")
  poolRef: {name: warm}" "http https-allowed reach -f https://$PUBLIC_HOST/
http https-other block --resolve $OTHER_HOST:443:$other_ip https://$OTHER_HOST/
http node-kubelet-$node1 block -k https://$node1:10250/"
	[ "$RAN_POOL" = warm ] || die "checkout did not run on a warm slot: $(kubectl -n "$NS" get swiftsandbox checkout -o jsonpath='{.status.conditions}')"
	proxy_ready checkout
	log "checkout ran on a warm slot of pool warm"

	# 6. Optional suites.
	if [ "${CREDENTIALS:-0}" = 1 ]; then
		[ "$PROXY" != broker ] || die "CREDENTIALS=1 needs a preset (PROXY)"
		suite credentials
	fi
	[ "${SPIFFE:-0}" != 1 ] || suite spiffe

	log "PASS: $PROXY conforms: only the allowlist, by name and address; never nodes, the API server, other ports, UDP, ICMP or IPv6"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
