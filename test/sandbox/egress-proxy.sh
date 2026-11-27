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
#      "upstream-svc": the same through a Service upstream (its ClusterIP,
#      reported in status.network.proxyUpstreams). A Service upstream in
#      front of the API server keeps the sandbox Pending
#      (EgressServiceReachesNode).
#   4. "http-listener": listeners.http: the guest's proxy variables point
#      at the proxy's HTTP listener; clients that honour them go through it,
#      one that ignores them hits the transparent listener; no other pod
#      reaches the HTTP port.
#   5. "intent": a proxy image in config Intent without the
#      io.kubeswift.egress-intent.versions label is refused
#      (EgressIntentUnsupported) before any launcher starts.
#   6. An image outside the allowlist is refused.
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
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.25}"
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

# envoy_http_config: an Envoy whose transparent listener forwards nothing and
# whose explicit HTTP listener on 15002 is a forward proxy (CONNECT and
# absolute URIs) to any name.
envoy_http_config() {
	cat <<'EOF'
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
---
apiVersion: v1
kind: Service
metadata: {name: target, namespace: $NS}
spec: {selector: {app: target}, ports: [{port: 8080, protocol: TCP}]}
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/sandbox --timeout=300s >/dev/null ||
		die "SwiftKernel sandbox not Ready in $NS"
	kubectl -n "$NS" wait --for=condition=Ready pod/target --timeout=120s >/dev/null || die "target pod not Ready"

	local target target_svc nodes apis cluster_ip other_ip
	target=$(kubectl -n "$NS" get pod target -o jsonpath='{.status.podIP}')
	target_svc=$(kubectl -n "$NS" get service target -o jsonpath='{.spec.clusterIP}')
	nodes=$(kubectl get nodes -o jsonpath='{range .items[*]}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{range .status.addresses[?(@.type=="ExternalIP")]}{.address}{"\n"}{end}{end}' | grep -E '^[0-9.]+$' | sort -u)
	apis=$(kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]}{"\n"}{end}' | sort -u)
	cluster_ip=$(kubectl get service kubernetes -n default -o jsonpath='{.spec.clusterIP}')
	other_ip=$(getent ahostsv4 "$OTHER_HOST" | awk 'NR==1{print $1}')
	[ -n "$other_ip" ] || die "cannot resolve $OTHER_HOST here"

	local guarded="http metadata block http://169.254.169.254/
http apiserver-clusterip block -k https://$cluster_ip/
http target-pod block http://$target:8080/
http target-service block http://$target_svc:8080/"
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

	local cond
	cond=$(kubectl -n "$NS" get swiftsandbox strict -o jsonpath='{range .status.conditions[?(@.type=="EgressProxyReady")]}{.status} {.reason}: {.message}{end}')
	case "$cond" in "True Listening: the egress proxy container listens"*) log "strict: EgressProxyReady $cond" ;; *) die "strict: EgressProxyReady is '$cond'" ;; esac

	run open "$hosts
$(envoy_proxy "")" "http https-other reach --resolve $OTHER_HOST:443:$other_ip https://$OTHER_HOST/
$guarded"

	run upstream "$hosts
$(envoy_proxy "" "        upstreams: [{cidr: $target/32, ports: [{port: 8080}]}]")" "http target-pod-upstream reach http://$target:8080/
http metadata block http://169.254.169.254/"

	run upstream-svc "$hosts
$(envoy_proxy "" "        upstreams: [{service: {name: target}}]")" "http target-service-upstream reach http://$target_svc:8080/
http target-pod block http://$target:8080/
http metadata block http://169.254.169.254/"
	kubectl -n "$NS" get swiftsandbox upstream-svc -o jsonpath='{.status.network.proxyUpstreams}' | grep -q "$target_svc/32" ||
		die "status.network.proxyUpstreams does not show the Service address: $(kubectl -n "$NS" get swiftsandbox upstream-svc -o jsonpath='{.status.network}')"
	log "upstream-svc: status.network.proxyUpstreams has $target_svc"

	kubectl -n "$NS" delete swiftsandbox upstream-api --ignore-not-found --wait=true >/dev/null
	sandbox upstream-api "$hosts
$(envoy_proxy "" "        upstreams: [{service: {name: kubernetes, namespace: default}}]")" "true"
	kubectl -n "$NS" wait swiftsandbox/upstream-api --timeout=60s \
		--for=jsonpath='{.status.conditions[?(@.type=="Resolved")].reason}'=EgressServiceReachesNode >/dev/null ||
		die "an upstream Service in front of the API server: $(kubectl -n "$NS" get swiftsandbox upstream-api -o jsonpath='{.status}')"
	log "an upstream Service in front of the API server keeps the sandbox Pending (EgressServiceReachesNode)"
	kubectl -n "$NS" delete swiftsandbox upstream-api --wait=false >/dev/null

	# listeners.http: the guest's HTTP_PROXY/HTTPS_PROXY point at the proxy's
	# HTTP listener on the bridge, so a client that honours them goes there;
	# one that ignores them still lands on the transparent listener (which
	# forwards nothing here). The HTTP port is the guest's only: another pod
	# cannot reach it.
	kubectl -n "$NS" delete swiftsandbox http-listener --ignore-not-found --wait=true >/dev/null
	sandbox http-listener "$hosts
      proxy:
        image: $PROXY_IMAGE
        listeners: {transparent: 15006, http: 15002}
        args:
        - --disable-hot-restart
        - --log-level
        - warn
        - --config-yaml
        - |
$(envoy_http_config | sed 's/^/          /')" "$(probe_script "check() { if [ \"\$2\" = \"\$3\" ]; then echo \"PROBE-OK   \$1\"; else echo \"PROBE-FAIL \$1 got \$2\"; f=\$((f+1)); fi; }
check https-proxy-env \"\$HTTPS_PROXY\" http://192.168.99.1:15002
check no-proxy-env \"\$no_proxy\" localhost,127.0.0.1,::1
http https-via-http-listener reach https://$PUBLIC_HOST/
http http-via-http-listener reach http://$PUBLIC_HOST/
http https-ignoring-the-proxy block --noproxy '*' https://$PUBLIC_HOST/
echo PROBES-DONE; sleep 30")"
	local deadline=$((SECONDS + TIMEOUT)) out hpod hip
	until out=$(kubectl -n "$NS" logs http-listener -c console 2>/dev/null | grep -E '^PROBE'); grep -q PROBES-DONE <<<"$out"; do
		[ "$SECONDS" -lt "$deadline" ] || die "http-listener: no probe output: $(kubectl -n "$NS" get swiftsandbox http-listener -o jsonpath='{.status.message}')"
		sleep 2
	done
	printf '%s\n' "$out" | grep -v PROBES-DONE | sed 's/^/   [http-listener] /'
	grep -q '^PROBE-FAIL' <<<"$out" && die "http-listener: egress not as expected"
	hip=$(kubectl -n "$NS" get pod http-listener -o jsonpath='{.status.podIP}')
	# Positive control: nc from the target pod does reach an open port.
	kubectl -n "$NS" exec target -- nc -z -w 3 "$target_svc" 8080 || die "nc from the target pod cannot reach its own Service: the next check would prove nothing"
	if kubectl -n "$NS" exec target -- nc -z -w 3 "$hip" 15002 2>/dev/null; then
		die "http-listener: another pod reached the proxy's HTTP listener at $hip:15002"
	fi
	log "http-listener: another pod cannot reach the HTTP listener"
	kubectl -n "$NS" delete swiftsandbox http-listener --wait=false >/dev/null

	# config Intent: the image that reads the intent file must declare the
	# versions it reads (io.kubeswift.egress-intent.versions); an unlabelled
	# image is refused before any launcher starts. Delivery of the file to a
	# labelled reader is egress-preset.sh's.
	kubectl -n "$NS" delete swiftsandbox intent --ignore-not-found --wait=true >/dev/null
	sandbox intent "$hosts
      proxy:
        image: $STANDIN_IMAGE
        config: Intent
        listeners: {transparent: 15006}" "echo intent-ran"
	local phase
	phase=$(finished intent)
	[ "$phase" = Failed ] && kubectl -n "$NS" get swiftsandbox intent -o jsonpath='{.status}' | grep -q EgressIntentUnsupported ||
		die "an unlabelled config Intent proxy: phase $phase, $(kubectl -n "$NS" get swiftsandbox intent -o jsonpath='{.status.message}')"
	[ -z "$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox=intent -o name)" ] || die "an unlabelled config Intent proxy got a launcher"
	log "config Intent: an image without io.kubeswift.egress-intent.versions is refused (EgressIntentUnsupported)"

	if kubectl apply -f - >/dev/null 2>"$NS.err" <<EOF; then
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata: {name: not-allowed, namespace: $NS}
spec:
  image: $IMAGE
  network: {mode: proxied, egress: {proxy: {image: "docker.io/library/nginx:1.27", listeners: {transparent: 15006}}}}
EOF
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
