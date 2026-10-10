#!/usr/bin/env bash
#
# Sandbox TCP receive test: repeated downloads into a SwiftSandbox must not
# lose a single frame in the guest.
#
# Guards #766. The sandbox guest kernel (Linux 6.6.44/6.6.45) rejected valid
# GSO packets in virtio-net ("eth0: bad gso", rx_frame_errors) whenever
# skb->len % gso_size was small, and every drop cost the transfer a TCP
# retransmission timeout (about 205 ms). Whether a packet hits it depends on
# how the sender's data falls into segments, so only volume finds it.
#
# Why this needs a real cluster with KVM: the failure is in the guest kernel's
# receive path and needs real GSO frames, built by the host stack (sender TSO
# on a veth, or GRO on the CNI overlay), handed through tap0 and Cloud
# Hypervisor's virtio-net with GUEST_TSO4 negotiated. Unit tests and envtest
# have no guest and no offloads.
#
# Subcommands:
#   ./sandbox-tcp-receive.sh validate   # servers + sandbox, assert zero drops
#   ./sandbox-tcp-receive.sh cleanup    # delete the test namespace
#
# What "validate" does:
#   1. An nginx pod serving 256 KiB, 1 MiB and 4 MiB random files on the
#      sandbox's node, and one on a second kernel node when there is one
#      (same-node and cross-node paths reach the guest
#      differently: sender TSO versus GRO on the overlay).
#   2. A SwiftSandbox (network mode open) downloads every file from each
#      server ROUNDS times (default 40: 240 transfers with two servers) and
#      exits non-zero if the guest's rx_frame_errors moved or "bad gso" was
#      logged.
#   3. Asserts the sandbox Completed with exit code 0.
#
# Prerequisites:
#   - a node labeled kubeswift.io/kernel-node=true with KVM
#   - a SwiftKernel (SWIFTKERNEL, default "sandbox") Ready in NS
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-sandbox-tcp-receive}"
SWIFTKERNEL="${SWIFTKERNEL:-sandbox}"
KERNEL_IMAGE="${KERNEL_IMAGE:-}"
ROUNDS="${ROUNDS:-40}"
CLIENT_IMAGE="${CLIENT_IMAGE:-curlimages/curl:8.10.1}"
SERVER_IMAGE="${SERVER_IMAGE:-nginx:1.27-alpine}"
TIMEOUT="${TIMEOUT:-900}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null
	log "deleted namespace $NS"
}

server() { # name node
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS, labels: {app: sandbox-tcp-receive}}
spec:
  nodeSelector: {kubernetes.io/hostname: $2}
  initContainers:
  - name: files
    image: $SERVER_IMAGE
    command: [sh, -c, "for s in 256 1024 4096; do dd if=/dev/urandom of=/data/f\$s bs=1024 count=\$s 2>/dev/null; done"]
    volumeMounts: [{name: data, mountPath: /data}]
  containers:
  - name: nginx
    image: $SERVER_IMAGE
    volumeMounts: [{name: data, mountPath: /usr/share/nginx/html}]
  volumes: [{name: data, emptyDir: {}}]
EOF
}

validate() {
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	if [ -n "$KERNEL_IMAGE" ]; then
		cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: {name: $SWIFTKERNEL, namespace: $NS}
spec: {profile: sandbox, kernelCmdline: console=ttyS0, ociRef: {image: "$KERNEL_IMAGE"}}
EOF
	fi
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready "swiftkernel/$SWIFTKERNEL" --timeout=300s >/dev/null ||
		die "SwiftKernel $SWIFTKERNEL not Ready in $NS"

	node=$(kubectl get nodes -l kubeswift.io/kernel-node=true -o jsonpath='{.items[0].metadata.name}')
	[ -n "$node" ] || die "no node labeled kubeswift.io/kernel-node=true"
	# The cross-node server goes on another kernel node: a worker the test
	# already relies on, so no taint keeps the pod off it.
	other=$(kubectl get nodes -l kubeswift.io/kernel-node=true \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep -vx "$node" | head -1 || true)
	server srv-same "$node"
	servers="srv-same"
	if [ -n "$other" ]; then
		server srv-cross "$other"
		servers="srv-same srv-cross"
	else
		log "one kernel node: same-node path only"
	fi
	for s in $servers; do
		kubectl -n "$NS" wait --for=condition=Ready "pod/$s" --timeout=300s >/dev/null || die "server $s not Ready"
	done
	ips=$(for s in $servers; do kubectl -n "$NS" get pod "$s" -o jsonpath='{.status.podIP}'; echo; done | xargs)
	log "sandbox node $node; servers $servers ($ips); $ROUNDS rounds"

	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata: {name: receiver, namespace: $NS}
spec:
  image: $CLIENT_IMAGE
  kernelProfileRef: {name: $SWIFTKERNEL}
  cpu: 1
  memory: 512Mi
  network: {mode: open}
  nodeSelector: {kubernetes.io/hostname: $node}
  command: [/bin/sh, -c]
  args:
  - |
    E=/sys/class/net/eth0/statistics/rx_frame_errors
    start=\$(cat \$E); slow=0; n=0
    for r in \$(seq 1 $ROUNDS); do for ip in $ips; do for f in f256 f1024 f4096; do
      t=\$(curl -sf -o /dev/null -w '%{time_total}' --max-time 30 http://\$ip/\$f) || { echo "transfer failed: \$ip/\$f"; exit 3; }
      n=\$((n+1)); case \$t in 0.0*|0.1[0-4]*) ;; *) slow=\$((slow+1)); echo "slow: \$ip/\$f \${t}s";; esac
    done; done; done
    drops=\$((\$(cat \$E)-start)); bad=\$(dmesg 2>/dev/null | grep -c 'bad gso' || true)
    echo "transfers=\$n rx_frame_errors=\$drops bad_gso=\$bad over_150ms=\$slow"
    [ "\$drops" -eq 0 ] && [ "\$bad" -eq 0 ]
EOF

	deadline=$((SECONDS + TIMEOUT))
	while :; do
		phase=$(kubectl -n "$NS" get swiftsandbox receiver -o jsonpath='{.status.phase}')
		case "$phase" in Completed|Failed) break ;; esac
		[ "$SECONDS" -lt "$deadline" ] || die "sandbox still $phase after ${TIMEOUT}s"
		sleep 5
	done
	code=$(kubectl -n "$NS" get swiftsandbox receiver -o jsonpath='{.status.exitCode}')
	msg=$(kubectl -n "$NS" get swiftsandbox receiver -o jsonpath='{.status.message}')
	if [ "$phase" != Completed ] || [ "$code" != 0 ]; then
		die "receiver $phase (exit ${code:-none}): $msg. Exit 1 means the guest dropped frames (#766); see 'swiftctl -n $NS sandbox logs receiver'"
	fi
	log "PASS: every transfer arrived without a dropped frame"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
