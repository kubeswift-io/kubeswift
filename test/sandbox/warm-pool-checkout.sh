#!/usr/bin/env bash
#
# Warm-pool checkout test: a checkout made as soon as a SwiftSandboxPool
# reports a warm slot must run on that slot.
#
# Guards the warm-slot readiness fix. A slot used to count as warm, and be
# claimable, as soon as its launcher container was running; its microVM then
# still had to boot, and the guest agent the checkout sends its workload to
# listened seconds later. A checkout in that window failed with ExecFailed
# (GuestAgentUnreachable). A newly created pool hit it every time: it reported
# warmReplicas 1 at launcher start, and a caller waiting for that checked out
# at once. So did a refill slot checked out right after it appeared.
#
# Why this needs a real cluster with KVM: the window is the guest's boot
# (kernel, DHCP, bridge, agent), which only a real microVM has. Unit tests
# cover the eligibility rule; this proves the launcher's report matches the
# guest.
#
# Subcommands:
#   ./warm-pool-checkout.sh validate   # pools + checkouts, assert warm hits
#   ./warm-pool-checkout.sh cleanup    # delete the test namespace
#
# What "validate" does, ROUNDS times (default 3), on one kernel node:
#   1. Creates a pool (minWarm 1), waits until status.warmReplicas is 1, and
#      at once creates a SwiftSandbox checking out from it. Asserts the
#      sandbox Completed with exit code 0 on a claimed slot (CheckedOut).
#   2. The same for the refill slot, checked out as soon as it is warm.
#   3. Creates a SwiftSandbox while the pool's slot is still booting.
#      Asserts it Completed (cold, PoolColdFallback, or on the slot once
#      ready) and never failed.
#
# Prerequisites:
#   - a node labeled kubeswift.io/kernel-node=true with KVM
#   - a SwiftKernel (SWIFTKERNEL, default "sandbox") Ready in NS, or
#     KERNEL_IMAGE to create it
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-sandbox-warm-checkout}"
SWIFTKERNEL="${SWIFTKERNEL:-sandbox}"
KERNEL_IMAGE="${KERNEL_IMAGE:-}"
IMAGE="${IMAGE:-docker.io/library/busybox:1.36}"
ROUNDS="${ROUNDS:-3}"
TIMEOUT="${TIMEOUT:-300}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null
	log "deleted namespace $NS"
}

pool() { # name node
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata: {name: $1, namespace: $NS}
spec:
  image: $IMAGE
  kernelProfileRef: {name: $SWIFTKERNEL}
  cpu: 1
  memory: 256Mi
  network: {mode: restricted}
  nodeSelector: {kubernetes.io/hostname: $2}
  minWarm: 1
EOF
}

checkout() { # name pool node
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata: {name: $1, namespace: $NS}
spec:
  image: $IMAGE
  kernelProfileRef: {name: $SWIFTKERNEL}
  poolRef: {name: $2}
  cpu: 1
  memory: 256Mi
  network: {mode: restricted}
  nodeSelector: {kubernetes.io/hostname: $3}
  command: [sh, -c]
  args: ["echo checked-out workload ran"]
EOF
}

warm() { kubectl -n "$NS" get swiftsandboxpool "$1" -o jsonpath='{.status.warmReplicas}' 2>/dev/null; }

# ready_slot POOL: a slot the controller would hand out (slotReady): labelled
# warm, its launcher running, and, when the controller stamped it to wait for
# the guest agent, the agent reported. The pool's status alone can be read
# before the pool has recounted after the previous checkout's claim, and then
# counts a slot already taken; the claim relabels the pod in the same write.
# A slot from a controller that does not stamp (v0.16.3) is ready when its
# launcher runs, so this test still reproduces that release's defect.
ready_slot() {
	kubectl -n "$NS" get pods -l "sandbox.kubeswift.io/pool=$1,sandbox.kubeswift.io/slot-state=warm" \
		-o jsonpath='{range .items[*]}{.metadata.annotations.sandbox\.kubeswift\.io/slot-readiness}|{.metadata.annotations.kubeswift\.io/guest-agent-ready}|{.status.containerStatuses[?(@.name=="launcher")].state.running.startedAt}{"\n"}{end}' 2>/dev/null |
		awk -F'|' '$3 != "" && ($1 != "guest-agent" || $2 != "") {found=1} END {exit !found}'
}

wait_warm() { # pool
	local deadline=$((SECONDS + TIMEOUT))
	until [ "$(warm "$1")" = 1 ] && ready_slot "$1"; do
		[ "$SECONDS" -lt "$deadline" ] || die "pool $1 has no warm slot after ${TIMEOUT}s: $(kubectl -n "$NS" get swiftsandboxpool "$1" -o jsonpath='{.status.message}')"
		sleep 0.2
	done
}

# finished NAME: waits for the sandbox to end; dies unless Completed, exit 0.
finished() {
	local deadline=$((SECONDS + TIMEOUT)) phase code msg
	while :; do
		phase=$(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.phase}')
		case "$phase" in Completed|Failed) break ;; esac
		[ "$SECONDS" -lt "$deadline" ] || die "sandbox $1 still $phase after ${TIMEOUT}s"
		sleep 0.5
	done
	code=$(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.exitCode}')
	msg=$(kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath='{.status.message}')
	[ "$phase" = Completed ] && [ "$code" = 0 ] ||
		die "sandbox $1 $phase (exit ${code:-none}): $msg. ExecFailed/GuestAgentUnreachable means a slot was handed out before its guest agent answered"
}

event() { # sandbox reason
	kubectl -n "$NS" get events.events.k8s.io --field-selector "regarding.name=$1,reason=$2" -o name | grep -q .
}

# hit NAME POOL NODE: a checkout made the moment the pool has a warm slot.
hit() {
	wait_warm "$2"
	local t0=$SECONDS
	checkout "$1" "$2" "$3"
	finished "$1"
	event "$1" CheckedOut || die "sandbox $1 did not check out a warm slot (booted cold): the pool reported a slot no checkout could take"
	log "$1: ran on a warm slot ($((SECONDS - t0))s to Completed)"
}

validate() {
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# A sandbox left by an earlier run would be re-applied unchanged and its
	# old phase read back: start clean.
	kubectl -n "$NS" delete swiftsandbox,swiftsandboxpool --all --wait=true >/dev/null
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
	log "node $node; image $IMAGE; $ROUNDS rounds"

	for r in $(seq 1 "$ROUNDS"); do
		p="wp$r"
		pool "$p" "$node"
		hit "first$r" "$p" "$node"
		hit "refill$r" "$p" "$node"
		kubectl -n "$NS" delete swiftsandboxpool "$p" --wait=true >/dev/null

		# A checkout while the only slot is still booting: it must not
		# fail. Today it boots cold; it never gets the booting slot.
		p="wb$r"
		pool "$p" "$node"
		checkout "booting$r" "$p" "$node"
		finished "booting$r"
		if event "booting$r" PoolColdFallback; then
			log "booting$r: no ready slot yet, booted cold"
		else
			log "booting$r: ran on a warm slot"
		fi
		kubectl -n "$NS" delete swiftsandboxpool "$p" --wait=true >/dev/null
		kubectl -n "$NS" delete swiftsandbox --all --wait=false >/dev/null
	done
	log "PASS: every checkout made as soon as a slot was warm ran on that slot"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
