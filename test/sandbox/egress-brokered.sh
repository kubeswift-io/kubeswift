#!/usr/bin/env bash
#
# Brokered-egress test: what a sandbox in network mode `brokered` can and
# cannot reach.
#
# In mode brokered the guest has no network device. Its workload's
# HTTP_PROXY / HTTPS_PROXY point at the guest agent's forwarder on the
# guest loopback, which carries each connection over vsock to the launcher,
# where the built-in broker (or the egress proxy) parses the HTTP proxy
# request and allows it by the same rules as mode proxied. The broker
# resolves names itself, only names a host rule or an allowed Service gives,
# and runs under the proxy uid, so the kernel keeps it away from nodes, the
# API server and private ranges the allowlist does not name.
#
# Subcommands:
#   ./egress-brokered.sh validate   # run the checks
#   ./egress-brokered.sh cleanup    # delete the test namespace
#
# What "validate" does:
#   1. Starts an HTTP pod ("target") behind Service "app", and reads the node
#      addresses, the API server endpoints and the kubernetes ClusterIP.
#   2. Sandbox "brokered" allows host PUBLIC_HOST:443, host *.PUBLIC_HOST:443
#      and service app:8080, and checks from the guest:
#        the guest has no interface but lo (and the kernel's unconfigured
#        tunnel devices), no DNS, and the proxy variables are set;
#        reach: https to both names, http://app:8080 and http://app.NS:8080
#        block: https to OTHER_HOST, http to PUBLIC_HOST on port 80, the
#               target pod by address, the metadata endpoint, every node's
#               kubelet, every API server endpoint, the kubernetes ClusterIP,
#               a direct TCP connect bypassing the proxy
#      Its egress audit (kubectl logs -c egress-audit) records the allow and
#      deny decisions.
#   3. Sandbox "kernel" allows, as cidr rules, every node address, every API
#      server endpoint, the kubernetes ClusterIP and the target pod. Only the
#      target pod becomes reachable: the broker allows the rest by address
#      and the kernel drops its connections.
#   4. Pool "warm" (same allowlist as 2, one warm slot) and sandbox
#      "checkout" claiming it: https to PUBLIC_HOST works, OTHER_HOST does
#      not, from a warm slot.
#   5. EgressProxyReady is True on "brokered" and "checkout".
#   6. Sandbox "relay" runs an Envoy egress proxy (config Self, a CONNECT
#      forward proxy on listeners.http) instead of the broker: https to
#      PUBLIC_HOST works through it, a node's kubelet does not (the proxy
#      uid chain), and the guest has no NIC. Needs the controller to allow
#      PROXY_IMAGE (chart sandboxMaterialize.egressProxyImages); SKIP_RELAY=1
#      skips it.
#   7. With OLD_KERNEL_IMAGE set (a kernel whose bridge lacks egress-vsock,
#      such as kernels/sandbox:6.6.22), sandbox "old-kernel" fails with
#      KernelUnsupported.
#
# Prerequisites:
#   - a node labeled kubeswift.io/kernel-node=true with KVM
#   - a sandbox kernel whose bridge lists egress-vsock (KERNEL_IMAGE,
#     kernels/sandbox 6.6.23 or later)
#   - internet egress from pods; PUBLIC_HOST (default example.com, must also
#     answer for www.PUBLIC_HOST) and OTHER_HOST (default github.com)
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-sandbox-egress-brokered}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.23}"
IMAGE="${IMAGE:-docker.io/curlimages/curl:8.11.1}"
TARGET_IMAGE="${TARGET_IMAGE:-docker.io/library/busybox:1.36}"
PUBLIC_HOST="${PUBLIC_HOST:-example.com}"
OTHER_HOST="${OTHER_HOST:-github.com}"
TIMEOUT="${TIMEOUT:-300}"
# SKIP_KERNEL=1: the SwiftKernel "sandbox" in NS is already prepared.
SKIP_KERNEL="${SKIP_KERNEL:-}"
PROXY_IMAGE="${PROXY_IMAGE:-docker.io/envoyproxy/envoy:v1.36.2}"
SKIP_RELAY="${SKIP_RELAY:-}"
OLD_KERNEL_IMAGE="${OLD_KERNEL_IMAGE:-}"
# The SwiftKernel sandbox() boots.
KP=sandbox

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
#   http NAME WANT URL [curl args...]   (through the proxy variables)
#   chk  NAME WANT COMMAND...           (reach = the command succeeds)
# with WANT reach|block. Exits with the failure count.
probe_script() {
	cat <<'EOF'
f=0
res() { if [ "$1" = "$2" ]; then echo "PROBE-OK   $3 $1"; else echo "PROBE-FAIL $3 got $1, want $2"; f=$((f+1)); fi; }
http() { n=$1; w=$2; shift 2; if curl -sS -f -m 8 -o /dev/null "$@" 2>/dev/null; then g=reach; else g=block; fi; res $g $w "$n"; }
chk() { n=$1; w=$2; shift 2; if "$@" >/dev/null 2>&1; then g=reach; else g=block; fi; res $g $w "$n"; }
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
  kernelProfileRef: {name: $KP}
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

# envoy_connect: a brokered network block whose egress proxy is Envoy as a
# CONNECT / absolute-URI forward proxy to any name on listeners.http 15002
# (its transparent listener forwards nothing).
envoy_connect() {
	printf '  network:\n    mode: brokered\n    egress:\n      proxy:\n        image: %s\n        listeners: {transparent: 15006, http: 15002}\n        args:\n        - --disable-hot-restart\n        - --log-level\n        - warn\n        - --config-yaml\n        - |\n' "$PROXY_IMAGE"
	sed 's/^/          /' <<'EOF'
static_resources:
  listeners:
  - name: transparent
    address: {socket_address: {address: 0.0.0.0, port_value: 15006}}
    listener_filters:
    - name: envoy.filters.listener.tls_inspector
      typed_config: {"@type": type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector}
    filter_chains:
    - filter_chain_match: {server_names: ["nothing.invalid"]}
      filters:
      - name: envoy.filters.network.tcp_proxy
        typed_config: {"@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy, stat_prefix: none, cluster: dfp}
  - name: http
    address: {socket_address: {address: 0.0.0.0, port_value: 15002}}
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: fwd
          route_config:
            virtual_hosts:
            - name: all
              domains: ["*"]
              routes:
              - match: {connect_matcher: {}}
                route: {cluster: dfp, upgrade_configs: [{upgrade_type: CONNECT, connect_config: {}}]}
              - match: {prefix: "/"}
                route: {cluster: dfp}
          http_filters:
          - name: envoy.filters.http.dynamic_forward_proxy
            typed_config: {"@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_forward_proxy.v3.FilterConfig, dns_cache_config: {name: c, dns_lookup_family: V4_ONLY}}
          - name: envoy.filters.http.router
            typed_config: {"@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router}
  clusters:
  - name: dfp
    lb_policy: CLUSTER_PROVIDED
    connect_timeout: 5s
    cluster_type: {name: envoy.clusters.dynamic_forward_proxy, typed_config: {"@type": type.googleapis.com/envoy.extensions.clusters.dynamic_forward_proxy.v3.ClusterConfig, dns_cache_config: {name: c, dns_lookup_family: V4_ONLY}}}
EOF
}

network() { # network ALLOW_LINES: a brokered network block with these allow entries
	printf '  network:\n    mode: brokered\n    egress:\n      allow:\n%s\n' "$(printf '%s\n' "$1" | sed '/^$/d; s/^/      - /')"
}

ready_condition() {
	local cond
	cond=$(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{range .status.conditions[?(@.type=="EgressProxyReady")]}{.status} {.reason}{end}')
	[ "$cond" = "True Listening" ] || die "$1: EgressProxyReady is '$cond'"
	log "$1: EgressProxyReady $cond"
}

validate() {
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" delete swiftsandbox --all --wait=true >/dev/null
	if [ -z "$SKIP_KERNEL" ]; then
		cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: {name: sandbox, namespace: $NS}
spec: {profile: sandbox, kernelCmdline: console=ttyS0, ociRef: {image: "$KERNEL_IMAGE"}}
EOF
	fi
	cat <<EOF | kubectl apply -f - >/dev/null
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
		die "SwiftKernel sandbox not Ready"
	kubectl -n "$NS" wait --for=condition=Ready pod/target --timeout=120s >/dev/null || die "target pod not Ready"

	local target_ip api_ips node_ips k8s_ip
	target_ip=$(kubectl -n "$NS" get pod target -o jsonpath='{.status.podIP}')
	k8s_ip=$(kubectl -n default get svc kubernetes -o jsonpath='{.spec.clusterIP}')
	api_ips=$(kubectl -n default get endpointslices -l kubernetes.io/service-name=kubernetes -o jsonpath='{.items[*].endpoints[*].addresses[*]}')
	node_ips=$(kubectl get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}')
	log "target $target_ip, kubernetes $k8s_ip, API $api_ips, nodes $node_ips"

	local allow="{host: $PUBLIC_HOST, ports: [{port: 443}]}
{host: \"*.$PUBLIC_HOST\", ports: [{port: 443}]}
{service: {name: app}, ports: [{port: 8080}]}"

	local probes="chk no-eth0 block test -e /sys/class/net/eth0
chk only-lo reach sh -c '[ -z \"\$(ls /sys/class/net | grep -v -e ^lo\$ -e ^sit -e ^ip6tnl -e ^tunl)\" ]'
chk proxy-env reach sh -c '[ \"\$HTTPS_PROXY\" = http://127.0.0.1:3128 ]'
chk dns block nslookup $PUBLIC_HOST
http direct-bypass block --noproxy '*' http://1.1.1.1/
http https-public reach https://$PUBLIC_HOST/
http https-www reach https://www.$PUBLIC_HOST/
http http-app reach http://app:8080/
http http-app-ns reach http://app.$NS:8080/
http https-other block https://$OTHER_HOST/
http http-public-80 block http://$PUBLIC_HOST/
http target-pod-address block http://$target_ip:8080/
http metadata block http://169.254.169.254/
http kubernetes-clusterip block -k https://$k8s_ip/"
	for ip in $api_ips; do probes="$probes
http api-$ip block -k https://$ip:6443/"; done
	for ip in $node_ips; do probes="$probes
http kubelet-$ip block -k https://$ip:10250/"; done
	run brokered "$(network "$allow")" "$probes"
	ready_condition brokered
	local pod audit
	pod=$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox=brokered -o jsonpath='{.items[0].metadata.name}')
	audit=$(kubectl -n "$NS" logs "$pod" -c egress-audit 2>/dev/null || true)
	printf '%s\n' "$audit" | grep -q '"event":"allow"' || die "brokered: no allow decision in the egress audit"
	printf '%s\n' "$audit" | grep -q '"event":"deny"' || die "brokered: no deny decision in the egress audit"
	log "brokered: egress audit has $(printf '%s\n' "$audit" | grep -c . || true) lines with allow and deny decisions"

	# The kernel, not the broker, keeps nodes and the API server out.
	local kallow="{cidr: $target_ip/32, ports: [{port: 8080}]}
{cidr: $k8s_ip/32}" kprobes="http target-pod-address reach http://$target_ip:8080/
http kubernetes-clusterip block -k https://$k8s_ip/"
	for ip in $api_ips; do kallow="$kallow
{cidr: $ip/32}"; kprobes="$kprobes
http api-$ip block -k https://$ip:6443/"; done
	for ip in $node_ips; do kallow="$kallow
{cidr: $ip/32}"; kprobes="$kprobes
http kubelet-$ip block -k https://$ip:10250/"; done
	run kernel "$(network "$kallow")" "$kprobes"

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
	run checkout "$(network "$allow")
  poolRef: {name: warm}" "chk proxy-env reach sh -c '[ \"\$HTTPS_PROXY\" = http://127.0.0.1:3128 ]'
http https-public reach https://$PUBLIC_HOST/
http https-other block https://$OTHER_HOST/"
	local claimed
	claimed=$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox=checkout -o jsonpath='{.items[0].metadata.labels.sandbox\.kubeswift\.io/pool}' 2>/dev/null || true)
	[ "$claimed" = warm ] || die "checkout did not run on a warm slot"
	log "checkout ran on a warm slot of pool warm"
	ready_condition checkout

	if [ -z "$SKIP_RELAY" ]; then
		local rprobes="chk no-eth0 block test -e /sys/class/net/eth0
http https-public reach https://$PUBLIC_HOST/"
		for ip in $node_ips; do rprobes="$rprobes
http kubelet-$ip block -k https://$ip:10250/"; done
		run relay "$(envoy_connect)" "$rprobes"
		ready_condition relay
	fi

	if [ -n "$OLD_KERNEL_IMAGE" ]; then
		cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: {name: old, namespace: $NS}
spec: {profile: sandbox, kernelCmdline: console=ttyS0, ociRef: {image: "$OLD_KERNEL_IMAGE"}}
EOF
		kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/old --timeout=300s >/dev/null || die "SwiftKernel old not Ready"
		kubectl -n "$NS" delete swiftsandbox old-kernel --ignore-not-found --wait=true >/dev/null
		KP=old sandbox old-kernel "$(network "{host: $PUBLIC_HOST}")" "true"
		local phase reason
		phase=$(finished old-kernel)
		reason=$(kubectl -n "$NS" get swiftsandbox old-kernel -o jsonpath='{.status.conditions[?(@.type=="GuestRunning")].reason}')
		[ "$phase" = Failed ] && [ "$reason" = KernelUnsupported ] ||
			die "old-kernel: $phase $reason: $(kubectl -n "$NS" get swiftsandbox old-kernel -o jsonpath='{.status.message}')"
		log "old-kernel: Failed KernelUnsupported"
	fi

	log "PASS: brokered egress has no guest NIC and allows only the allowlist, never nodes or the API server"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
