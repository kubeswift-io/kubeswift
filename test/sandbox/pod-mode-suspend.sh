#!/usr/bin/env bash
#
# Pod mode with agent-sandbox: a Sandbox suspended and resumed keeps its
# microVM (docs/sandbox/pod-mode.md#agent-sandbox-suspend-and-resume).
#
# Checks, on a cluster with pod mode enabled and agent-sandbox installed:
#   - a Sandbox naming the kubeswift-microvm RuntimeClass runs in a microVM;
#   - set to operatingMode Suspended (what the agent-sandbox SDK's suspend
#     does), its Pod goes, agent-sandbox reports it Suspended, and the
#     microVM is a Suspended SwiftSandbox, not deleted;
#   - set back to Running, the new Pod resumes the same SwiftSandbox on the
#     same node: a file written in the guest before the suspend is still
#     there and a counter in memory carries on from where it was;
#   - deleting a suspended Sandbox deletes its microVM.
#
# Subcommands:
#   ./pod-mode-suspend.sh validate
#   ./pod-mode-suspend.sh cleanup
#
# Prerequisites: the chart with sandboxMaterialize.podMode.enabled; agent-
# sandbox (CRD sandboxes.agents.x-k8s.io, v1beta1); the pod-mode namespace's
# SwiftKernel "sandbox" on a kernel with bridge feature resume-sync
# (kernels/sandbox:6.6.25 or later), else the microVM is not suspended.

set -euo pipefail

NS="${NS:-pod-mode-suspend}"
PM_NS="${PM_NS:-kubeswift-sandboxes}"
IMAGE="${IMAGE:-docker.io/library/busybox:1.37.0}"
TIMEOUT="${TIMEOUT:-240}"
AWAY="${AWAY:-15}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null
	log "deleted namespace $NS"
}

# until_ok SECONDS DESCRIPTION CMD...
until_ok() {
	local deadline=$((SECONDS + $1)) what=$2
	shift 2
	until "$@" >/dev/null 2>&1; do
		[ "$SECONDS" -lt "$deadline" ] || die "timed out: $what"
		sleep 1
	done
}

sandbox() {
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata: {name: $1, namespace: $NS}
spec:
  operatingMode: Running
  podTemplate:
    spec:
      runtimeClassName: kubeswift-microvm
      containers:
        - name: work
          image: $IMAGE
          command: [sh, -c]
          args:
            - |
              i=0; while :; do i=\$((i+1)); echo "TICK \$i"; sleep 1; done
          resources: {limits: {cpu: "1", memory: 256Mi}}
EOF
}

mode() { kubectl -n "$NS" patch sandbox "$1" --type=merge -p "{\"spec\":{\"operatingMode\":\"$2\"}}" >/dev/null; }
pod_ready() { [ "$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ]; }
pod_gone() { ! kubectl -n "$NS" get pod "$1"; }
microvm() { kubectl -n "$NS" get pod "$1" -o jsonpath='{.metadata.labels.podmode\.kubeswift\.io/sandbox}'; }
sb() { kubectl -n "$PM_NS" get swiftsandbox "$1" -o jsonpath="$2"; }
sb_phase() { [ "$(sb "$1" '{.status.phase}')" = "$2" ]; }
sb_gone() { ! kubectl -n "$PM_NS" get swiftsandbox "$1"; }
as_suspended() { [ "$(kubectl -n "$NS" get sandbox "$1" -o jsonpath='{.status.conditions[?(@.type=="Suspended")].status}')" = True ]; }
last_tick() { kubectl -n "$NS" logs "$1" 2>/dev/null | awk '/^TICK /{c=$2} END{print c}'; }

validate() {
	kubectl get crd sandboxes.agents.x-k8s.io >/dev/null 2>&1 || die "agent-sandbox is not installed (CRD sandboxes.agents.x-k8s.io)"
	kubectl create namespace "$NS" >/dev/null 2>&1 || true
	kubectl label namespace "$NS" kubeswift.io/pod-mode=enabled --overwrite >/dev/null

	log "a Sandbox runs in a microVM"
	sandbox a
	until_ok "$TIMEOUT" "pod a Ready" pod_ready a
	local vm node c1 c2
	vm=$(microvm a)
	[ -n "$vm" ] || die "stand-in a names no SwiftSandbox"
	node=$(sb "$vm" '{.status.nodeName}')
	kubectl -n "$NS" exec a -- sh -c 'echo kept > /tmp/marker' || die "exec into a"
	sleep 3
	c1=$(last_tick a)
	[ -n "$c1" ] || die "no TICK in the logs of a"
	log "a: microVM $vm on $node, counter $c1"

	log "operatingMode Suspended suspends the microVM"
	mode a Suspended
	until_ok "$TIMEOUT" "pod a gone" pod_gone a
	until_ok 60 "agent-sandbox reports a Suspended" as_suspended a
	sb_phase "$vm" Suspended || die "SwiftSandbox $vm is $(sb "$vm" '{.status.phase}'), not Suspended: $(sb "$vm" '{.status.message}')"
	log "a: Pod gone, SwiftSandbox $vm Suspended; waiting ${AWAY}s"
	sleep "$AWAY"

	log "operatingMode Running resumes the same microVM"
	local before=$SECONDS
	mode a Running
	until_ok "$TIMEOUT" "pod a Ready again" pod_ready a
	log "a: Ready again after $((SECONDS - before))s"
	[ "$(microvm a)" = "$vm" ] || die "the new Pod runs microVM $(microvm a), not $vm"
	[ "$(kubectl -n "$NS" get pod a -o jsonpath='{.spec.nodeName}')" = "$node" ] || die "the new Pod is not on $node"
	[ "$(sb "$vm" '{.status.conditions[?(@.type=="Suspended")].reason}')" = Resumed ] ||
		die "SwiftSandbox $vm condition $(sb "$vm" '{.status.conditions[?(@.type=="Suspended")].reason}')"
	[ "$(kubectl -n "$NS" exec a -- cat /tmp/marker)" = kept ] || die "the file written before the suspend is gone"
	sleep 3
	c2=$(last_tick a)
	[ -n "$c2" ] || die "no TICK in the logs of a after the resume"
	[ "$c2" -gt "$c1" ] && [ "$c2" -lt $((c1 + AWAY)) ] || die "counter $c2 after the resume, $c1 before (away ${AWAY}s)"
	log "a: file kept, counter $c1 -> $c2"

	log "deleting a suspended Sandbox deletes its microVM"
	mode a Suspended
	until_ok "$TIMEOUT" "pod a gone" pod_gone a
	until_ok 60 "SwiftSandbox $vm Suspended" sb_phase "$vm" Suspended
	kubectl -n "$NS" delete sandbox a --wait=true --timeout=60s >/dev/null
	until_ok 120 "SwiftSandbox $vm deleted" sb_gone "$vm"

	log "PASS: an agent-sandbox suspend keeps the microVM, and resume continues it"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*)
	echo "usage: $0 validate|cleanup" >&2
	exit 2
	;;
esac
