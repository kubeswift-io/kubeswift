#!/usr/bin/env bash
#
# Restricted-egress test: what a `restricted` sandbox guest can and cannot
# reach, including what spec.network.egress.allow cannot open.
#
# Guards the restricted egress rules (#941). A guest in `restricted` mode
# reached the kubelet and the API server through the nodes' addresses, which
# the private-range drop did not cover on nodes with public addresses. Now the
# controller resolves every node InternalIP/ExternalIP and the API server's
# endpoints, publishes them on the launcher as sandbox.kubeswift.io/egress-deny,
# and network-init drops them before the allowlist.
#
# Also guards #953: the rules see a packet's destination before kube-proxy
# translates it on the node, so a Service in front of a node or the API server
# used to reach them. The controller now denies those Services' ClusterIPs,
# external IPs and load-balancer IPs, and a service rule naming one waits.
#
# Why this needs a real cluster with KVM: the rules live in the launcher pod's
# netns and only a real guest's forwarded traffic exercises them. Unit tests
# cover how the list is built.
#
# Subcommands:
#   ./egress-restricted.sh validate   # two sandboxes, probe from the guest
#   ./egress-restricted.sh cleanup    # delete the test namespace
#
# What "validate" does:
#   1. Starts a TCP echo pod (the "target") and reads the node addresses, the
#      API server endpoints and the kubernetes Service ClusterIP.
#   2. Sandbox "plain" (restricted, no allowlist) probes, from the guest:
#      blocked: metadata 169.254.169.254:80, every node address :10250, every
#      API server endpoint, the kubernetes ClusterIP, the target pod; allowed:
#      DNS through the cluster resolver and PUBLIC_TARGET.
#   3. The webhook refuses an allow entry for 169.254.169.254. A service
#      rule for the kubernetes Service, or for a selectorless Service the test
#      points at a node's kubelet, keeps the sandbox Pending
#      (EgressServiceReachesNode). Sandbox "allowing" allows the target pod,
#      every node and API server address and both ClusterIPs as CIDRs: only
#      the target pod becomes reachable.
#   4. Sandbox "flip" allows Service "app" while it points at the target pod,
#      then the test repoints its EndpointSlice at a node's kubelet: the
#      running guest must lose it within the refresh window.
#   5. Asserts the launcher's egress-deny annotation lists every node address
#      and API server endpoint address.
#
# Prerequisites:
#   - a node labeled kubeswift.io/kernel-node=true with KVM
#   - KERNEL_IMAGE (default the current sandbox kernel) pullable by the nodes
#   - PUBLIC_TARGET (host:port, default 1.1.1.1:443) reachable from pods;
#     set it empty on a cluster without internet egress
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-sandbox-egress-restricted}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.25}"
IMAGE="${IMAGE:-docker.io/library/busybox:1.36}"
PUBLIC_TARGET="${PUBLIC_TARGET-1.1.1.1:443}"
TIMEOUT="${TIMEOUT:-300}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null
	log "deleted namespace $NS"
}

# finished NAME: waits for the sandbox to end; prints its phase.
finished() {
	local deadline=$((SECONDS + TIMEOUT)) phase
	while :; do
		phase=$(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.phase}')
		case "$phase" in Completed|Failed) echo "$phase"; return ;; esac
		[ "$SECONDS" -lt "$deadline" ] || die "sandbox $1 still $phase after ${TIMEOUT}s"
		sleep 1
	done
}

# probe_script EXPECTATIONS: a guest shell script. Each expectation line is
# "name want host port" with want reach|block. Exits with the failure count.
probe_script() {
	cat <<'EOF'
f=0
chk() { if nc -z -w 3 "$3" "$4" </dev/null >/dev/null 2>&1; then got=reach; else got=block; fi
  if [ "$got" = "$2" ]; then echo "PROBE-OK   $1 $3:$4 $got"; else echo "PROBE-FAIL $1 $3:$4 got $got, want $2"; f=$((f+1)); fi; }
if nslookup kubernetes.default.svc.cluster.local >/dev/null 2>&1; then echo "PROBE-OK   dns cluster resolver"; else echo "PROBE-FAIL dns cluster resolver"; f=$((f+1)); fi
EOF
	while read -r name want host port; do
		[ -n "$name" ] && printf 'chk %s %s %s %s\n' "$name" "$want" "$host" "$port"
	done <<<"$1"
	echo 'exit $f'
}

# sandbox NAME ALLOW_YAML SCRIPT
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
    mode: restricted
$2
  command: [sh, -c]
  args:
    - |
$script
EOF
}

# run NAME ALLOW_YAML EXPECTATIONS: runs the probes; dies on any failure.
run() {
	kubectl -n "$NS" delete swiftsandbox "$1" --ignore-not-found --wait=true >/dev/null
	sandbox "$1" "$2" "$(probe_script "$3")"
	local phase out
	phase=$(finished "$1")
	out=$(kubectl -n "$NS" logs "$1" -c console 2>&1 | grep -E '^PROBE-' || true)
	printf '%s\n' "$out" | sed "s/^/   [$1] /"
	[ -n "$out" ] || die "sandbox $1 $phase with no probe output: $(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.message}')"
	if printf '%s\n' "$out" | grep -q '^PROBE-FAIL' || [ "$phase" != Completed ]; then
		die "sandbox $1: egress not as expected (phase $phase)"
	fi
}

# flip TARGET NODE: a sandbox allows Service "app" while its endpoint is the
# target pod; the test then points the Service's EndpointSlice at NODE's
# kubelet. The guest must lose the Service within the refresh window
# (controller every 30 s, kubelet's downward-API refresh, swiftletd's 10 s
# poll).
flip() {
	cat <<EOF2 | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Service
metadata: {name: app, namespace: $NS}
spec: {ports: [{name: p, port: 8080, protocol: TCP}]}
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata: {name: app-1, namespace: $NS, labels: {kubernetes.io/service-name: app}}
addressType: IPv4
ports: [{name: p, port: 8080, protocol: TCP}]
endpoints: [{addresses: ["$1"]}]
EOF2
	local ip
	ip=$(kubectl -n "$NS" get service app -o jsonpath='{.spec.clusterIP}')
	kubectl -n "$NS" delete swiftsandbox flip --ignore-not-found --wait=true >/dev/null
	sandbox flip "    egress:
      allow:
      - service: {name: app}" "seen=0; i=0
while [ \$i -lt 100 ]; do
  if nc -z -w 2 $ip 8080 </dev/null >/dev/null 2>&1; then
    [ \$seen = 0 ] && echo FLIP-REACH; seen=1
  elif [ \$seen = 1 ]; then echo \"FLIP-BLOCKED after \$((i*3))s of probing\"; exit 0; fi
  i=\$((i+1)); sleep 3
done
echo FLIP-TIMEOUT; exit 1"
	local deadline=$((SECONDS + TIMEOUT))
	until kubectl -n "$NS" logs flip -c console 2>/dev/null | grep -q '^FLIP-REACH'; do
		[ "$SECONDS" -lt "$deadline" ] || die "flip: guest never reached Service app through its pod endpoint"
		sleep 1
	done
	local t0=$SECONDS
	kubectl -n "$NS" patch endpointslice app-1 --type=json -p "[{\"op\":\"replace\",\"path\":\"/endpoints/0/addresses/0\",\"value\":\"$2\"},{\"op\":\"replace\",\"path\":\"/ports/0/port\",\"value\":10250}]" >/dev/null
	log "flip: guest reaches Service app; pointed its EndpointSlice at a node's kubelet"
	local phase
	phase=$(finished flip)
	kubectl -n "$NS" logs flip -c console 2>/dev/null | grep '^FLIP-' | sed 's/^/   [flip] /'
	[ "$phase" = Completed ] || die "flip: the guest kept reaching the Service after it was pointed at a node"
	log "flip: Service blocked $((SECONDS - t0))s after the EndpointSlice change (sandbox end included)"
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
metadata: {name: target, namespace: $NS}
spec:
  containers:
  - name: echo
    image: $IMAGE
    command: [sh, -c, "nc -lk -p 8080 -e cat"]
    securityContext: {runAsNonRoot: true, runAsUser: 65534, allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/sandbox --timeout=300s >/dev/null ||
		die "SwiftKernel sandbox not Ready in $NS"
	kubectl -n "$NS" wait --for=condition=Ready pod/target --timeout=120s >/dev/null || die "target pod not Ready"

	local target nodes apis api_ip api_port cluster_ip
	target=$(kubectl -n "$NS" get pod target -o jsonpath='{.status.podIP}')
	nodes=$(kubectl get nodes -o jsonpath='{range .items[*].status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{range .items[*].status.addresses[?(@.type=="ExternalIP")]}{.address}{"\n"}{end}' | grep -E '^[0-9.]+$' | sort -u)
	api_port=$(kubectl -n default get endpointslice kubernetes -o jsonpath='{.ports[0].port}')
	apis=$(kubectl -n default get endpointslice kubernetes -o jsonpath='{range .endpoints[*]}{.addresses[0]}{"\n"}{end}' | grep -E '^[0-9.]+$' | sort -u)
	cluster_ip=$(kubectl -n default get service kubernetes -o jsonpath='{.spec.clusterIP}')
	# A Service the tenant points at a node's kubelet: selectorless, with an
	# EndpointSlice it writes itself. Allowing it must not open the node.
	local first_node fwd_ip
	first_node=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
	cat <<EOF2 | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Service
metadata: {name: kubelet-fwd, namespace: $NS}
spec: {ports: [{name: k, port: 10250, protocol: TCP}]}
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata: {name: kubelet-fwd-1, namespace: $NS, labels: {kubernetes.io/service-name: kubelet-fwd}}
addressType: IPv4
ports: [{name: k, port: 10250, protocol: TCP}]
endpoints: [{addresses: ["$first_node"]}]
EOF2
	fwd_ip=$(kubectl -n "$NS" get service kubelet-fwd -o jsonpath='{.spec.clusterIP}')
	[ -n "$nodes" ] && [ -n "$apis" ] && [ -n "$target" ] || die "could not read node, API server or target addresses"
	log "$(wc -l <<<"$nodes") node address(es), $(wc -l <<<"$apis") API server endpoint(s), target pod, PUBLIC_TARGET=${PUBLIC_TARGET:-none}"

	local blocked="" n
	blocked+="metadata block 169.254.169.254 80"$'\n'
	for n in $nodes; do blocked+="node-kubelet block $n 10250"$'\n'; done
	for api_ip in $apis; do blocked+="apiserver-endpoint block $api_ip $api_port"$'\n'; done
	blocked+="apiserver-clusterip block $cluster_ip 443"$'\n'
	blocked+="service-to-node block $fwd_ip 10250"$'\n'
	local public=""
	[ -n "$PUBLIC_TARGET" ] && public="public reach ${PUBLIC_TARGET%:*} ${PUBLIC_TARGET##*:}"

	run plain "" "${blocked}target-pod block $target 8080
$public"

	# The webhook refuses an allow entry for the metadata range outright.
	local refused
	refused=$(sandbox metadata "    egress:
      allow:
      - cidr: 169.254.169.254/32" "true" 2>&1 || true)
	grep -q 'never allowed' <<<"$refused" || die "an allow entry for 169.254.169.254 was not refused: $refused"
	log "webhook refuses an allow entry for the metadata range"

	# A service rule naming a Service in front of the API server or a node
	# keeps the sandbox Pending.
	local svc
	for svc in "kubernetes default" "kubelet-fwd $NS"; do
		set -- $svc
		kubectl -n "$NS" delete swiftsandbox pend --ignore-not-found --wait=true >/dev/null
		sandbox pend "    egress:
      allow:
      - service: {name: $1, namespace: $2}" "true"
		local deadline=$((SECONDS + 60)) reason=""
		until [ "$reason" = EgressServiceReachesNode ]; do
			[ "$SECONDS" -lt "$deadline" ] || die "allowing Service $2/$1: condition reason ${reason:-none}, want EgressServiceReachesNode"
			sleep 1
			reason=$(kubectl -n "$NS" get swiftsandbox pend -o jsonpath='{.status.conditions[?(@.type=="Resolved")].reason}')
		done
		[ "$(kubectl -n "$NS" get swiftsandbox pend -o jsonpath='{.status.phase}')" = Pending ] || die "allowing Service $2/$1: not Pending"
		log "service rule for $2/$1 holds the sandbox Pending (EgressServiceReachesNode)"
	done
	kubectl -n "$NS" delete swiftsandbox pend --wait=false >/dev/null

	# By CIDR, the same addresses stay blocked.
	local allow="    egress:
      allow:
      - cidr: $target/32
      - cidr: $cluster_ip/32
      - cidr: $fwd_ip/32"
	for n in $nodes $apis; do allow+="
      - cidr: $n/32"; done
	run allowing "$allow" "${blocked}target-pod reach $target 8080
$public"

	flip "$target" "$first_node"

	local pod deny
	pod=$(kubectl -n "$NS" get pods -l sandbox.kubeswift.io/sandbox=allowing -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
	if [ -n "$pod" ]; then
		deny=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.metadata.annotations.sandbox\.kubeswift\.io/egress-deny}')
		for n in $nodes $apis; do
			grep -qwF "$n" <<<"$deny" || die "egress-deny annotation on $pod lacks $n: $deny"
		done
		log "egress-deny annotation lists every node and API server address"
	else
		log "launcher pod of 'allowing' already gone; annotation check skipped"
	fi
	log "PASS: restricted egress blocks metadata, nodes and the API server whatever the allowlist says"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
