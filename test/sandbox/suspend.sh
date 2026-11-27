#!/usr/bin/env bash
#
# Sandbox suspend and resume test (spec.operatingMode).
#
# Checks, on a cluster with KVM:
#   - a running sandbox set to Suspended reaches phase Suspended, its launcher
#     pod goes, status.suspendedState records the node and size;
#   - set back to Running it reaches Running again on the same node: its
#     workload carries on where it was (a counter in memory keeps counting
#     from its last value), its clock is the host's again (resume-sync), its
#     readiness probe passes, the Suspended condition says Resumed and the
#     saved state is gone (finalizer removed);
#   - the same with a virtio-fs rootfs;
#   - deleting a suspended sandbox removes it (the cleanup Job on the node);
#   - a sandbox created Suspended is not started;
#   - admission refuses Suspended for a sandbox with spec.poolRef.
#
# Not covered here: two restores of one snapshot differ in RNG output (a
# snapshot is restored once; the kernel's VMGENID reseed was measured by hand,
# docs/sandbox/suspend.md), and spec.timeout skipping suspended time (unit
# tests).
#
# Subcommands:
#   ./suspend.sh validate
#   ./suspend.sh cleanup
#
# Prerequisites: a node labeled kubeswift.io/kernel-node=true with KVM;
# KERNEL_IMAGE pullable by the nodes (bridge feature resume-sync: sandbox
# 6.6.25 or later).

set -euo pipefail

NS="${NS:-sandbox-suspend}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.25}"
IMAGE="${IMAGE:-docker.io/library/busybox:1.37.0}"
TIMEOUT="${TIMEOUT:-180}"
# How long a sandbox stays suspended before it is resumed: long enough that
# a guest clock left alone would be visibly behind.
AWAY="${AWAY:-20}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null
	log "deleted namespace $NS"
}

field() { kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath="$2"; }

# wait_phase NAME PHASE
wait_phase() {
	local deadline=$((SECONDS + TIMEOUT)) p
	while :; do
		p=$(field "$1" '{.status.phase}' 2>/dev/null || true)
		[ "$p" = "$2" ] && return 0
		case "$p" in Failed | Completed) [ "$2" = "$p" ] || die "$1 $p: $(field "$1" '{.status.message}')" ;; esac
		[ $SECONDS -lt $deadline ] || die "$1 not $2 after ${TIMEOUT}s (phase $p: $(field "$1" '{.status.message}'))"
		sleep 1
	done
}

# sandbox NAME OPERATING_MODE EXTRA_SPEC_YAML
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
  operatingMode: $2
  network:
    mode: restricted
    ports: [{port: 8000}]
  readinessProbe: {tcpSocket: {port: 8000}, periodSeconds: 1}
$3
  command: [sh, -c]
  args:
    - |
      mkdir -p /www && httpd -p 8000 -h /www
      i=0; while :; do i=\$((i+1)); echo "TICK \$i \$(date +%s)"; sleep 1; done
EOF
}

# last_tick NAME: "<count> <guest epoch>" of the console's last TICK line.
last_tick() {
	kubectl -n "$NS" logs "$1" -c console 2>/dev/null | awk '/^TICK /{c=$2; t=$3} END{print c, t}'
}

workload_ready() {
	field "$1" '{.status.conditions[?(@.type=="WorkloadReady")].status}'
}

suspend_resume() {
	local name=$1 node size before c1 t1 c2 t2 now reason
	wait_phase "$name" Running
	sleep 5
	node=$(field "$name" '{.status.nodeName}')
	read -r c1 t1 < <(last_tick "$name")
	[ -n "$c1" ] || die "$name: no TICK on the console"
	log "$name: running on $node, counter $c1"

	kubectl -n "$NS" patch swiftsandbox "$name" --type=merge -p '{"spec":{"operatingMode":"Suspended"}}' >/dev/null
	wait_phase "$name" Suspended
	size=$(field "$name" '{.status.suspendedState.sizeBytes}')
	[ "$(field "$name" '{.status.suspendedState.nodeName}')" = "$node" ] || die "$name: suspendedState.nodeName is not $node"
	[ "${size:-0}" -gt 0 ] || die "$name: suspendedState.sizeBytes is ${size:-unset}"
	[ "$(field "$name" '{.status.conditions[?(@.type=="Suspended")].status}')" = True ] || die "$name: Suspended condition not True"
	local deadline=$((SECONDS + 60))
	while kubectl -n "$NS" get pod "$name" >/dev/null 2>&1; do
		[ $SECONDS -lt $deadline ] || die "$name: launcher pod still there while suspended"
		sleep 1
	done
	log "$name: Suspended ($size bytes on $node), launcher gone; waiting ${AWAY}s"
	sleep "$AWAY"

	before=$SECONDS
	kubectl -n "$NS" patch swiftsandbox "$name" --type=merge -p '{"spec":{"operatingMode":"Running"}}' >/dev/null
	wait_phase "$name" Running
	log "$name: Running again after $((SECONDS - before))s"
	[ "$(field "$name" '{.status.nodeName}')" = "$node" ] || die "$name: resumed on another node"
	reason=$(field "$name" '{.status.conditions[?(@.type=="Suspended")].reason}')
	[ "$reason" = Resumed ] || die "$name: Suspended condition reason $reason: $(field "$name" '{.status.conditions[?(@.type=="Suspended")].message}')"
	[ -z "$(field "$name" '{.status.suspendedState}')" ] || die "$name: suspendedState left after the resume"
	case " $(field "$name" '{.metadata.finalizers}') " in *suspend-cleanup*) die "$name: suspend finalizer left after the resume" ;; esac

	sleep 4
	read -r c2 t2 < <(last_tick "$name")
	now=$(date +%s)
	[ -n "$c2" ] || die "$name: no TICK on the console after the resume"
	# Memory kept: the counter goes on from where it was, it did not restart.
	[ "$c2" -gt "$c1" ] || die "$name: counter $c2 after resume, $c1 before: the workload restarted"
	[ "$c2" -lt $((c1 + AWAY)) ] || die "$name: counter $c2 grew while suspended (before $c1, away ${AWAY}s)"
	# Clock reset: the last tick's guest time is the host's.
	[ $((now - t2)) -le 3 ] && [ $((t2 - now)) -le 3 ] || die "$name: guest clock $t2, host $now"
	log "$name: counter $c1 -> $c2, guest clock within 3s of the host"
	local deadline=$((SECONDS + 60))
	until [ "$(workload_ready "$name")" = True ]; do
		[ $SECONDS -lt $deadline ] || die "$name: WorkloadReady not True after the resume"
		sleep 1
	done
	log "$name: readiness probe passes after the resume"
}

validate() {
	kubectl create namespace "$NS" >/dev/null 2>&1 || true
	kubectl apply -f - >/dev/null <<EOF
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: {name: sandbox, namespace: $NS}
spec: {profile: sandbox, kernelCmdline: console=ttyS0, ociRef: {image: "$KERNEL_IMAGE"}}
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/sandbox --timeout=300s >/dev/null ||
		die "SwiftKernel sandbox not Ready in $NS"

	log "admission refuses Suspended with spec.poolRef"
	if sandbox refused Suspended "  poolRef: {name: none}" 2>/dev/null; then
		die "a Suspended sandbox with poolRef was admitted"
	fi

	log "created Suspended: not started"
	sandbox later Suspended ""
	wait_phase later Pending
	sleep 5
	kubectl -n "$NS" get pod later >/dev/null 2>&1 && die "a sandbox created Suspended got a launcher"

	sandbox block Running ""
	sandbox vfs Running "  rootfsMode: virtiofs"
	suspend_resume block
	suspend_resume vfs

	log "the sandbox created Suspended starts when set to Running"
	kubectl -n "$NS" patch swiftsandbox later --type=merge -p '{"spec":{"operatingMode":"Running"}}' >/dev/null
	wait_phase later Running

	log "deleting a suspended sandbox"
	kubectl -n "$NS" patch swiftsandbox block --type=merge -p '{"spec":{"operatingMode":"Suspended"}}' >/dev/null
	wait_phase block Suspended
	kubectl -n "$NS" delete swiftsandbox block --wait=true --timeout=120s >/dev/null || die "a suspended sandbox was not deleted"

	log "PASS: suspend and resume keep the workload's memory, reset its clock, and clean up"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*)
	echo "usage: $0 validate|cleanup" >&2
	exit 2
	;;
esac
