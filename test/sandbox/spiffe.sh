#!/usr/bin/env bash
#
# SPIFFE identity test: a proxied sandbox's egress proxy proves the sandbox's
# SPIFFE ID to an upstream over mTLS, with an SVID from the SPIFFE Workload
# API that the guest never sees.
#
# The guest sends plaintext HTTP to the target's ClusterIP. The sandbox's
# Envoy (config Self) originates mTLS with the SVID it reads over SDS from
# the SPIRE agent socket KubeSwift mounts into the proxy. The target is an
# Envoy that requires a client certificate from the trust domain and lets
# through only the IDs in its RBAC rule; it answers 200, or 403 for any other
# SPIFFE ID.
#
# Subcommands:
#   ./spiffe.sh validate   # run the checks
#   ./spiffe.sh cleanup    # delete the test namespace and ClusterSPIFFEID
#
# What "validate" does:
#   1. Cold sandbox "allowed": 200; status.identity.spiffeID is
#      spiffe://<td>/ns/<ns>/sandbox/allowed.
#   2. Cold sandbox "other": the target sees a different ID and answers 403.
#   3. Pool "agents" with identity, sandbox "checkout" claiming a warm slot:
#      200 with the slot's ID (spiffe://<td>/ns/<ns>/pool/agents/slot/<slot>,
#      in status), on the warm slot.
#   4. Sandbox "anonymous": the same proxy without spec.identity has no
#      socket and no SVID; the guest's request fails.
#   5. Sandbox "broker": no proxy; the built-in broker originates the mTLS
#      for an allow rule with tls: {originate: SPIFFE}: 200, and the egress
#      audit records tls=spiffe with the SVID. "broker-other": 403.
#   6. allow[].tls with a Self proxy is refused.
#   7. With PRESET (a preset whose translator lists tls-originate, such as
#      envoy-translator v0.3.0): sandbox "preset" claims nothing, its proxy
#      reads rule.tls from the egress-intent file and originates the mTLS:
#      200; "preset-other": 403. With OLD_PRESET (a translator without the
#      label): the sandbox fails with EgressIntentUnsupported.
#
# Prerequisites:
#   - a node labeled kubeswift.io/kernel-node=true with KVM
#   - SPIRE with its controller manager and the spiffe-csi-driver
#   - the controller with chart sandboxMaterialize.spiffe enabled (TRUST_DOMAIN)
#     and the chart's ClusterSPIFFEIDs; PROXY_IMAGE in
#     sandboxMaterialize.egressProxyImages
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-sandbox-spiffe}"
TRUST_DOMAIN="${TRUST_DOMAIN:?set TRUST_DOMAIN to the SPIRE trust domain}"
CLASS_NAME="${CLASS_NAME:-spire-server-spire}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.23}"
IMAGE="${IMAGE:-docker.io/curlimages/curl:8.11.1}"
PROXY_IMAGE="${PROXY_IMAGE:-docker.io/envoyproxy/envoy:v1.36.2}"
PRESET="${PRESET:-}"
OLD_PRESET="${OLD_PRESET:-}"
SOCKET="${SOCKET:-/run/spiffe/spire-agent.sock}"
TIMEOUT="${TIMEOUT:-300}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete clusterspiffeid "kst-$NS-target" --ignore-not-found >/dev/null
	kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null
	log "deleted namespace $NS"
}

# sds NAME: an SDS reference to the SPIRE agent ("default" is the
# workload's SVID, "ROOTCA" the trust bundle).
sds() {
	printf '{name: %s, sds_config: {resource_api_version: V3, api_config_source: {api_type: GRPC, transport_api_version: V3, grpc_services: [{envoy_grpc: {cluster_name: spire_agent}}]}}}' "$1"
}

spire_cluster() {
	cat <<EOF
  - name: spire_agent
    type: STATIC
    connect_timeout: 1s
    typed_extension_protocol_options: {envoy.extensions.upstreams.http.v3.HttpProtocolOptions: {"@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions, explicit_http_config: {http2_protocol_options: {}}}}
    load_assignment: {cluster_name: spire_agent, endpoints: [{lb_endpoints: [{endpoint: {address: {pipe: {path: $1}}}}]}]}
EOF
}

# sandbox_envoy TARGET_IP: every guest connection goes to the target over
# mTLS with the sandbox's SVID; the target must present its own ID.
sandbox_envoy() {
	cat <<EOF
node: {id: sandbox, cluster: sandbox}
admin: {address: {socket_address: {address: 127.0.0.1, port_value: 15000}}}
static_resources:
  listeners:
  - name: transparent
    address: {socket_address: {address: 0.0.0.0, port_value: 15006}}
    filter_chains:
    - filters:
      - name: envoy.filters.network.tcp_proxy
        typed_config: {"@type": type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy, stat_prefix: out, cluster: target}
  clusters:
  - name: target
    type: STATIC
    connect_timeout: 5s
    load_assignment: {cluster_name: target, endpoints: [{lb_endpoints: [{endpoint: {address: {socket_address: {address: $1, port_value: 8443}}}}]}]}
    transport_socket:
      name: envoy.transport_sockets.tls
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
        common_tls_context:
          tls_certificate_sds_secret_configs: [$(sds default)]
          combined_validation_context:
            default_validation_context: {match_typed_subject_alt_names: [{san_type: URI, matcher: {exact: "spiffe://$TRUST_DOMAIN/ns/$NS/target"}}]}
            validation_context_sds_secret_config: $(sds ROOTCA)
$(spire_cluster "$SOCKET")
EOF
}

# target_envoy: mTLS on 8443, client certificates from the trust domain, RBAC
# on the client's SPIFFE ID.
target_envoy() {
	cat <<EOF
node: {id: target, cluster: target}
static_resources:
  listeners:
  - name: mtls
    address: {socket_address: {address: 0.0.0.0, port_value: 8443}}
    filter_chains:
    - transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
          require_client_certificate: true
          common_tls_context:
            tls_certificate_sds_secret_configs: [$(sds default)]
            validation_context_sds_secret_config: $(sds ROOTCA)
      filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: in
          route_config:
            virtual_hosts: [{name: all, domains: ["*"], routes: [{match: {prefix: /}, direct_response: {status: 200, body: {inline_string: "spiffe-ok\n"}}}]}]
          http_filters:
          - name: envoy.filters.http.rbac
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.rbac.v3.RBAC
              rules:
                policies:
                  sandboxes:
                    permissions: [{any: true}]
                    principals:
                    - {authenticated: {principal_name: {exact: "spiffe://$TRUST_DOMAIN/ns/$NS/sandbox/allowed"}}}
                    - {authenticated: {principal_name: {exact: "spiffe://$TRUST_DOMAIN/ns/$NS/sandbox/broker"}}}
                    - {authenticated: {principal_name: {exact: "spiffe://$TRUST_DOMAIN/ns/$NS/sandbox/preset"}}}
                    - {authenticated: {principal_name: {prefix: "spiffe://$TRUST_DOMAIN/ns/$NS/pool/agents/slot/"}}}
          - name: envoy.filters.http.router
            typed_config: {"@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router}
  clusters:
$(spire_cluster /spiffe-workload-api/spire-agent.sock)
EOF
}

# network TARGET_IP SVC: the proxied network with the Envoy above.
network() {
	cat <<EOF
  network:
    mode: proxied
    egress:
      proxy:
        image: $PROXY_IMAGE
        listeners: {transparent: 15006}
        upstreams: [{service: {name: $2}}]
        args:
        - --disable-hot-restart
        - --log-level
        - warn
        - --config-yaml
        - |
$(sandbox_envoy "$1" | sed 's/^/          /')
EOF
}

# broker_network: no proxy; the broker originates TLS to the target Service.
broker_network() {
	cat <<EOF
  network:
    mode: proxied
    egress:
      allow:
      - service: {name: target}
        tls: {originate: SPIFFE}
EOF
}

# preset_network PRESET: a config-Intent preset proxy that reads rule.tls.
preset_network() {
	cat <<EOF
  network:
    mode: proxied
    egress:
      allow:
      - service: {name: target}
        tls: {originate: SPIFFE}
      proxy:
        preset: $1
        upstreams: [{service: {name: target}}]
EOF
}

# sandbox NAME IDENTITY EXTRA TARGET_IP [NETWORK]: a sandbox whose workload prints the
# HTTP status the target answered (000: no answer).
sandbox() {
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
$3
${5:-$(network "$4" target)}
  command: [sh, -c]
  args: ['for i in 1 2 3 4 5; do c=\$(curl -s -m 5 -o /dev/null -w "%{http_code}" http://$4:8443/) || true; [ "\$c" = 000 ] || break; sleep 2; done; echo "SPIFFE-STATUS \$c"; sleep 300']
EOF
}

# run NAME WANT IDENTITY EXTRA TARGET_IP [NETWORK]. The output is read while the
# workload still runs: a warm checkout's slot pod is deleted once it ends.
run() {
	kubectl -n "$NS" delete swiftsandbox "$1" --ignore-not-found --wait=true >/dev/null
	sandbox "$1" "$3" "$4" "$5" "${6:-}"
	local deadline=$((SECONDS + TIMEOUT)) pod got="" phase
	while [ -z "$got" ]; do
		phase=$(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.phase}')
		case "$phase" in Failed|Completed) die "sandbox $1 $phase before any output: $(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.reason}: {.status.message}')" ;; esac
		[ "$SECONDS" -lt "$deadline" ] || die "sandbox $1 printed nothing after ${TIMEOUT}s ($phase)"
		pod=$(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.podRef}')
		[ -z "$pod" ] || got=$(kubectl -n "$NS" logs "$pod" -c console 2>/dev/null | sed -n 's/.*SPIFFE-STATUS \([0-9]*\).*/\1/p' | tail -1 || true)
		[ -n "$got" ] || sleep 1
	done
	[ "$got" = "$2" ] || die "sandbox $1: the target answered $got, want $2"
	log "$1: the target answered $got (launcher $pod)"
}

spiffe_id() { kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.identity.spiffeID}'; }

validate() {
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" delete swiftsandbox --all --wait=true >/dev/null
	kubectl -n "$NS" delete swiftsandboxpool --all --wait=true >/dev/null
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: {name: sandbox, namespace: $NS}
spec: {profile: sandbox, kernelCmdline: console=ttyS0, ociRef: {image: "$KERNEL_IMAGE"}}
---
apiVersion: spire.spiffe.io/v1alpha1
kind: ClusterSPIFFEID
metadata: {name: kst-$NS-target}
spec:
  className: $CLASS_NAME
  spiffeIDTemplate: 'spiffe://{{ .TrustDomain }}/ns/{{ .PodMeta.Namespace }}/target'
  namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: $NS}}
  podSelector: {matchLabels: {app: spiffe-target}}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: target-envoy, namespace: $NS}
data:
  envoy.yaml: |
$(target_envoy | sed 's/^/    /')
---
apiVersion: v1
kind: Pod
metadata: {name: target, namespace: $NS, labels: {app: spiffe-target}}
spec:
  containers:
  - name: envoy
    image: $PROXY_IMAGE
    args: [-c, /etc/envoy/envoy.yaml, --disable-hot-restart, --log-level, warn]
    securityContext: {runAsNonRoot: true, runAsUser: 65534, allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    volumeMounts:
    - {name: config, mountPath: /etc/envoy}
    - {name: spiffe, mountPath: /spiffe-workload-api, readOnly: true}
  volumes:
  - {name: config, configMap: {name: target-envoy}}
  - {name: spiffe, csi: {driver: csi.spiffe.io, readOnly: true}}
---
apiVersion: v1
kind: Service
metadata: {name: target, namespace: $NS}
spec: {selector: {app: spiffe-target}, ports: [{port: 8443, protocol: TCP}]}
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/sandbox --timeout=300s >/dev/null ||
		die "SwiftKernel sandbox not Ready in $NS"
	kubectl -n "$NS" wait --for=condition=Ready pod/target --timeout=120s >/dev/null || die "target pod not Ready"
	local ip
	ip=$(kubectl -n "$NS" get service target -o jsonpath='{.spec.clusterIP}')
	local on="  identity: {spiffe: Enabled}"

	# 6 first: no launcher needed.
	if out=$(cat <<EOF | kubectl apply --dry-run=server -f - 2>&1
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata: {name: tls-with-proxy, namespace: $NS}
spec:
  image: $IMAGE
  identity: {spiffe: Enabled}
  network:
    mode: proxied
    egress:
      allow: [{service: {name: target}, tls: {originate: SPIFFE}}]
      proxy: {image: $PROXY_IMAGE, listeners: {transparent: 15006}}
  command: ["true"]
EOF
	); then die "allow[].tls with a Self proxy was accepted"; fi
	case "$out" in *"built-in broker"*) log "allow[].tls with a Self proxy is refused" ;; *) die "unexpected refusal: $out" ;; esac

	run allowed 200 "$on" "" "$ip"
	[ "$(spiffe_id allowed)" = "spiffe://$TRUST_DOMAIN/ns/$NS/sandbox/allowed" ] || die "allowed: status.identity.spiffeID is '$(spiffe_id allowed)'"
	log "allowed: status.identity.spiffeID $(spiffe_id allowed)"
	run other 403 "$on" "" "$ip"
	log "other: the target saw $(spiffe_id other) and refused it"
	run anonymous 000 "" "" "$ip"
	log "anonymous: no identity, no SVID, no mTLS"

	run broker 200 "$on" "" "$ip" "$(broker_network)"
	local audit
	audit=$(kubectl -n "$NS" logs broker -c egress-audit 2>/dev/null | grep '"tls":"spiffe"' | head -1 || true)
	case "$audit" in *"\"svid\":\"spiffe://$TRUST_DOMAIN/ns/$NS/sandbox/broker\""*) log "broker: egress audit $audit" ;;
	*) die "broker: no tls=spiffe audit line with the sandbox's SVID: '$audit'" ;; esac
	run broker-other 403 "$on" "" "$ip" "$(broker_network)"
	log "broker-other: the target saw $(spiffe_id broker-other) and refused it"

	if [ -n "$PRESET" ]; then
		run preset 200 "$on" "" "$ip" "$(preset_network "$PRESET")"
		run preset-other 403 "$on" "" "$ip" "$(preset_network "$PRESET")"
		log "preset $PRESET: the proxy originated the mTLS from rule.tls"
	fi
	if [ -n "$OLD_PRESET" ]; then
		kubectl -n "$NS" delete swiftsandbox old-preset --ignore-not-found --wait=true >/dev/null
		sandbox old-preset "$on" "" "$ip" "$(preset_network "$OLD_PRESET")"
		local deadline=$((SECONDS + 120)) reason=""
		while [ "$reason" != EgressIntentUnsupported ]; do
			[ "$SECONDS" -lt "$deadline" ] || die "old-preset: reason '$reason', want EgressIntentUnsupported"
			sleep 2
			reason=$(kubectl -n "$NS" get swiftsandbox old-preset -o jsonpath='{.status.conditions[?(@.type=="GuestRunning")].reason}')
		done
		log "old-preset: $(kubectl -n "$NS" get swiftsandbox old-preset -o jsonpath='{.status.phase} {.status.conditions[?(@.type=="GuestRunning")].reason}: {.status.message}')"
	fi

	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata: {name: agents, namespace: $NS}
spec:
  image: $IMAGE
  kernelProfileRef: {name: sandbox}
  cpu: 1
  memory: 256Mi
  minWarm: 1
$on
$(network "$ip" target)
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftsandboxpool/agents --timeout="${TIMEOUT}s" >/dev/null ||
		die "pool agents has no ready slot: $(kubectl -n "$NS" get swiftsandboxpool agents -o jsonpath='{.status.conditions}')"
	local t0 took slot
	t0=$SECONDS
	run checkout 200 "$on" "  poolRef: {name: agents}" "$ip"
	took=$((SECONDS - t0))
	slot=$(kubectl -n "$NS" get swiftsandbox checkout -o jsonpath='{.status.podRef}')
	case "$slot" in agents-*) ;; *) die "checkout did not run on a warm slot (launcher '$slot'): $(kubectl -n "$NS" get swiftsandbox checkout -o jsonpath='{.status.conditions}')" ;; esac
	[ "$(spiffe_id checkout)" = "spiffe://$TRUST_DOMAIN/ns/$NS/pool/agents/slot/$slot" ] || die "checkout: status.identity.spiffeID is '$(spiffe_id checkout)'"
	log "checkout: warm slot $slot, status.identity.spiffeID $(spiffe_id checkout), create to answer ${took}s"
	kubectl -n "$NS" delete swiftsandbox --all --wait=false >/dev/null

	log "PASS: the egress proxy proves the sandbox's SPIFFE ID to the upstream; the guest never holds it"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
