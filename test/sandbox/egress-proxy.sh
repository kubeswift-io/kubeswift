#!/usr/bin/env bash
#
# Egress proxy test: a proxied sandbox with its own proxy container
# (spec.network.egress.proxy.image) instead of the built-in broker.
#
# Guest TCP is redirected to the proxy's transparent listener. The proxy is an
# unprivileged container in the launcher pod under the proxy uid, and the
# kernel holds the proxy's own connections to the restricted rules plus
# proxy.upstreams. So a proxy decides what the guest reaches, but no proxy,
# however it is configured, reaches a node, the API server, the metadata
# endpoint or a private address outside its upstreams.
#
# Subcommands:
#   ./egress-proxy.sh validate   # run the checks
#   ./egress-proxy.sh cleanup    # delete the test namespace
#
# What "validate" does (Envoy, unmodified, config Self via --config-yaml):
#   1. "strict": Envoy forwards only TLS connections whose server name is
#      PUBLIC_HOST or *.PUBLIC_HOST. https to PUBLIC_HOST works, OTHER_HOST
#      does not, nor does any node, API server or metadata address.
#   2. "open": Envoy forwards every connection to its original destination.
#      OTHER_HOST is now reachable (the guest's policy is the proxy's), but
#      the kernel still blocks every node's kubelet, the API server, the
#      kubernetes ClusterIP, the metadata endpoint and the target pod.
#   3. "upstream": "open" plus proxy.upstreams = the target pod: now reachable.
#   4. "intent": a stand-in proxy in config Intent prints the
#      egress-intent.kubeswift.io/v1 file it was given; the test checks it.
#   5. An image outside the allowlist is refused.
#
# Prerequisites:
#   - a node labeled kubeswift.io/kernel-node=true with KVM
#   - the controller allows PROXY_IMAGE and STANDIN_IMAGE
#     (chart sandboxMaterialize.egressProxyImages)
#   - internet egress from pods; PUBLIC_HOST and OTHER_HOST resolve here and
#     in the cluster
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-sandbox-egress-proxy}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.22}"
IMAGE="${IMAGE:-docker.io/curlimages/curl:8.11.1}"
TARGET_IMAGE="${TARGET_IMAGE:-docker.io/library/busybox:1.36}"
PROXY_IMAGE="${PROXY_IMAGE:-docker.io/envoyproxy/envoy:v1.36.2}"
STANDIN_IMAGE="${STANDIN_IMAGE:-docker.io/library/busybox:1.36}"
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
res() { if [ "$1" = "$2" ]; then echo "PROBE-OK   $3 $1"; else echo "PROBE-FAIL $3 got $1, want $2"; f=$((f+1)); fi; }
http() { n=$1; w=$2; shift 2; if curl -sS -m 8 -o /dev/null "$@" 2>/dev/null; then g=reach; else g=block; fi; res $g $w "$n"; }
EOF
	printf '%s\n' "$1" | sed '/^$/d'
	echo 'exit $f'
}

# envoy_config CHAIN_MATCH: an Envoy bootstrap with one transparent listener
# on 15006 forwarding to the original destination. CHAIN_MATCH is the
# filter_chain_match (empty: every connection).
envoy_config() {
	cat <<EOF
static_resources:
  listeners:
  - name: transparent
    address: {socket_address: {address: 0.0.0.0, port_value: 15006}}
    listener_filters:
    - name: envoy.filters.listener.original_dst
      typed_config: {"@type": type.googleapis.com/envoy.extensions.filters.listener.original_dst.v3.OriginalDst}
    - name: envoy.filters.listener.tls_inspector
      typed_config: {"@type": type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector}
    filter_chains:
    - filter_chain_match: {$1}
      filters:
      - name: envoy.filters.network.tcp_proxy
        typed_config: {"@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy, stat_prefix: out, cluster: original_dst}
  clusters:
  - name: original_dst
    type: ORIGINAL_DST
    lb_policy: CLUSTER_PROVIDED
    connect_timeout: 5s
EOF
}

# sandbox NAME PROXY_YAML SCRIPT (PROXY_YAML: lines under egress, 6-space indent)
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
  network:
    mode: proxied
    egress:
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
	local phase out
	phase=$(finished "$1")
	out=$(kubectl -n "$NS" logs "$1" -c console 2>&1 | grep -E '^PROBE-' || true)
	printf '%s\n' "$out" | sed "s/^/   [$1] /"
	[ -n "$out" ] || die "sandbox $1 $phase with no probe output: $(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.reason}: {.status.message}')"
	if printf '%s\n' "$out" | grep -q '^PROBE-FAIL' || [ "$phase" != Completed ]; then
		die "sandbox $1: egress not as expected (phase $phase)"
	fi
}

# envoy_proxy CHAIN_MATCH [UPSTREAMS_YAML]: a proxy block running Envoy.
envoy_proxy() {
	printf '      proxy:\n        image: %s\n        listeners: {transparent: 15006}\n        args:\n        - --disable-hot-restart\n        - --log-level\n        - warn\n        - --config-yaml\n        - |\n%s\n%s\n' \
		"$PROXY_IMAGE" "$(envoy_config "$1" | sed 's/^/          /')" "${2:-}"
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
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/sandbox --timeout=300s >/dev/null ||
		die "SwiftKernel sandbox not Ready in $NS"
	kubectl -n "$NS" wait --for=condition=Ready pod/target --timeout=120s >/dev/null || die "target pod not Ready"

	local target nodes apis cluster_ip other_ip
	target=$(kubectl -n "$NS" get pod target -o jsonpath='{.status.podIP}')
	nodes=$(kubectl get nodes -o jsonpath='{range .items[*]}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{range .status.addresses[?(@.type=="ExternalIP")]}{.address}{"\n"}{end}{end}' | grep -E '^[0-9.]+$' | sort -u)
	apis=$(kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]}{"\n"}{end}' | sort -u)
	cluster_ip=$(kubectl get service kubernetes -n default -o jsonpath='{.spec.clusterIP}')
	other_ip=$(getent ahostsv4 "$OTHER_HOST" | awk 'NR==1{print $1}')
	[ -n "$other_ip" ] || die "cannot resolve $OTHER_HOST here"

	local guarded="http metadata block http://169.254.169.254/
http apiserver-clusterip block -k https://$cluster_ip/
http target-pod block http://$target:8080/"
	for n in $nodes; do guarded+="
http node-kubelet-$n block -k https://$n:10250/"; done
	for a in $apis; do guarded+="
http apiserver-$a block -k https://$a:6443/"; done
	local hosts="      allow:
      - {host: $PUBLIC_HOST, ports: [{port: 443}]}
      - {host: $OTHER_HOST, ports: [{port: 443}]}"

	run strict "$hosts
$(envoy_proxy "server_names: [\"$PUBLIC_HOST\", \"*.$PUBLIC_HOST\"]")" "http https-allowed reach https://$PUBLIC_HOST/
http https-other block --resolve $OTHER_HOST:443:$other_ip https://$OTHER_HOST/
$guarded"

	run open "$hosts
$(envoy_proxy "")" "http https-other reach --resolve $OTHER_HOST:443:$other_ip https://$OTHER_HOST/
$guarded"

	run upstream "$hosts
$(envoy_proxy "" "        upstreams: [{cidr: $target/32, ports: [{port: 8080}]}]")" "http target-pod-upstream reach http://$target:8080/
http metadata block http://169.254.169.254/"

	kubectl -n "$NS" delete swiftsandbox intent --ignore-not-found --wait=true >/dev/null
	sandbox intent "$hosts
      proxy:
        image: $STANDIN_IMAGE
        config: Intent
        listeners: {transparent: 15006}
        args: [sh, -c, 'cat \"\$KUBESWIFT_EGRESS_INTENT\"; nc -lk -p 15006 -e true || while :; do nc -l -p 15006 </dev/null; done']" "echo intent-ran"
	local pod logs
	finished intent >/dev/null
	pod=$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox=intent -o jsonpath='{.items[0].metadata.name}')
	logs=$(kubectl -n "$NS" logs "$pod" -c egress-proxy 2>&1 || true)
	for want in '"apiVersion": "egress-intent.kubeswift.io/v1"' "\"host\": \"$PUBLIC_HOST\"" '"transparent": 15006' "\"name\": \"intent\""; do
		grep -qF "$want" <<<"$logs" || die "intent file lacks $want: $logs"
	done
	log "config Intent: the proxy read the egress-intent file"

	if kubectl apply -f - >/dev/null 2>"$NS.err" <<EOF; then
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata: {name: not-allowed, namespace: $NS}
spec:
  image: $IMAGE
  network: {mode: proxied, egress: {proxy: {image: "docker.io/library/nginx:1.27", listeners: {transparent: 15006}}}}
EOF
		local phase
		phase=$(finished not-allowed)
		[ "$phase" = Failed ] || die "an image outside the allowlist ran (phase $phase)"
	fi
	rm -f "$NS.err"
	log "an image outside the allowlist is refused"
	log "PASS: the proxy decides the guest's egress; the kernel keeps every proxy away from nodes, the API server and private addresses"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
