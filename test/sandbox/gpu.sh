#!/usr/bin/env bash
#
# GPU sandbox test: a memory snapshot of a VM with a passed-through GPU is
# refused by the launcher, and (optionally) CUDA runs in a GPU sandbox.
#
# Guards the launcher's VFIO snapshot refusal (#945). Cloud Hypervisor
# snapshots a VFIO device without refusing, and for a device without VFIO
# migration support (every GPU on vfio-pci) saves no device state: a guest
# restored from it can hang in its driver. swiftletd now reads the VM's
# devices from Cloud Hypervisor before it pauses and refuses the capture.
# The test sends the capture action straight to the launcher, the path the
# admission webhook cannot see (a DRA claim, or the webhook off).
#
# Why this needs a real cluster with a GPU: the refusal reads Cloud
# Hypervisor's own device list, which only a VM with a real VFIO device has.
# Unit tests cover the decision on recorded vm.info output.
#
# Subcommands:
#   ./gpu.sh validate   # GPU sandbox, capture action, optional CUDA run
#   ./gpu.sh cleanup    # delete the test namespace
#
# What "validate" does:
#   1. Starts a GPU sandbox (SwiftGPUProfile, tier pcie, one GPU) that sleeps.
#   2. Writes a capture action on its launcher pod and waits for the
#      launcher's answer: status "failed", detail naming the device and
#      "memory snapshot refused". The VM must still be running afterwards.
#   3. When GPU_IMAGE is set, runs a sandbox from it with its own command and
#      expects Completed, exit 0. The image must load the NVIDIA modules
#      built for KERNEL_IMAGE and run a CUDA check (see
#      docs/sandbox/gpu-sandboxes.md, "Example: a Pascal image").
#
# Prerequisites:
#   - a node labeled kubeswift.io/gpu-node=true and kernel-node=true with a
#     free GPU in its SwiftGPUNode inventory
#   - KERNEL_IMAGE (default the current gpu-sandbox kernel)
#
# Acceptance: exit 0 on success. Non-zero on any check failing.

set -euo pipefail

NS="${NS:-sandbox-gpu}"
KERNEL_IMAGE="${KERNEL_IMAGE:-ghcr.io/kubeswift-io/kubeswift/kernels/gpu-sandbox:6.6.13}"
IMAGE="${IMAGE:-docker.io/library/busybox:1.36}"
GPU_IMAGE="${GPU_IMAGE:-}"
TIMEOUT="${TIMEOUT:-300}"

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null
	log "deleted namespace $NS"
}

field() { kubectl -n "$NS" get swiftsandbox "$1" -o jsonpath="$2"; }

# sandbox NAME IMAGE COMMAND_YAML
sandbox() {
	kubectl -n "$NS" delete swiftsandbox "$1" --ignore-not-found --wait=true >/dev/null
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata: {name: $1, namespace: $NS}
spec:
  image: $2
  kernelProfileRef: {name: gpu-sandbox}
  cpu: 2
  memory: 4Gi
  gpuProfileRef: {name: one-gpu}
  network: {mode: restricted}
$3
EOF
}

# phase_in NAME PHASES...: waits until the sandbox is in one of PHASES.
phase_in() {
	local name=$1 deadline=$((SECONDS + TIMEOUT)) phase
	shift
	while :; do
		phase=$(field "$name" '{.status.phase}')
		for p in "$@"; do [ "$phase" = "$p" ] && { echo "$phase"; return; }; done
		case "$phase" in Failed|Completed)
			die "sandbox $name $phase: $(field "$name" '{.status.message}')" ;;
		esac
		[ "$SECONDS" -lt "$deadline" ] || die "sandbox $name still $phase after ${TIMEOUT}s: $(field "$name" '{.status.message}')"
		sleep 1
	done
}

launcher_pod() {
	kubectl -n "$NS" get pods -l "sandbox.kubeswift.io/sandbox=$1" -o jsonpath='{.items[0].metadata.name}'
}

ann() { kubectl -n "$NS" get pod "$1" -o jsonpath="{.metadata.annotations.kubeswift\.io/$2}"; }

validate() {
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" delete swiftsandbox --all --wait=true >/dev/null
	cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: {name: gpu-sandbox, namespace: $NS}
spec: {profile: gpu-sandbox, kernelCmdline: console=ttyS0, ociRef: {image: "$KERNEL_IMAGE"}}
---
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
metadata: {name: one-gpu, namespace: $NS}
spec: {count: 1, tier: pcie, partitionMode: isolated}
EOF
	kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/gpu-sandbox --timeout=300s >/dev/null ||
		die "SwiftKernel gpu-sandbox not Ready in $NS"

	sandbox holder "$IMAGE" "  command: [sleep, '600']"
	phase_in holder Running >/dev/null
	local pod id detail status
	pod=$(launcher_pod holder)
	# Running is reported once the VM has started; give the guest a moment so
	# the capture meets a fully running VM, as a real snapshot would.
	sleep 5
	id="gpu-test-$(date +%s)"
	kubectl -n "$NS" annotate pod "$pod" --overwrite \
		"kubeswift.io/snapshot-action=capture" \
		"kubeswift.io/snapshot-action-id=$id" \
		"kubeswift.io/snapshot-action-args={\"destination_url\":\"file:///var/lib/kubeswift/snapshots/$NS-gpu-test/\"}" >/dev/null
	local deadline=$((SECONDS + 60))
	until [ "$(ann "$pod" snapshot-status-id)" = "$id" ]; do
		[ "$SECONDS" -lt "$deadline" ] || die "launcher $pod did not answer capture $id within 60s"
		sleep 0.5
	done
	status=$(ann "$pod" snapshot-status)
	detail=$(ann "$pod" snapshot-status-detail)
	log "launcher answered: $status: $detail"
	[ "$status" = failed ] || die "capture with a GPU attached: status $status, want failed"
	grep -q 'memory snapshot refused' <<<"$detail" || die "capture failed for another reason: $detail"
	grep -q 'vfio' <<<"$detail" || die "refusal does not name the device's driver: $detail"
	[ "$(field holder '{.status.phase}')" = Running ] || die "sandbox holder left Running after the refused capture"
	local state
	state=$(kubectl -n "$NS" exec "$pod" -c launcher -- curl -sS --unix-socket "/var/lib/kubeswift/run/$NS-holder/ch.sock" http://localhost/api/v1/vm.info 2>/dev/null |
		python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["state"], len(d["config"].get("devices") or []))')
	[ "${state%% *}" = Running ] || die "VM state after the refused capture: $state, want Running"
	log "capture refused before pausing; VM $state device(s) attached"
	kubectl -n "$NS" delete swiftsandbox holder --wait=true >/dev/null

	if [ -n "$GPU_IMAGE" ]; then
		sandbox cuda "$GPU_IMAGE" ""
		phase_in cuda Completed >/dev/null
		[ "$(field cuda '{.status.exitCode}')" = 0 ] || die "CUDA sandbox exit $(field cuda '{.status.exitCode}')"
		log "CUDA sandbox from $GPU_IMAGE Completed, exit 0"
	else
		log "GPU_IMAGE not set: CUDA run skipped"
	fi
	log "PASS: GPU sandbox checks"
}

case "${1:-}" in
validate) validate ;;
cleanup) cleanup ;;
*) echo "usage: $0 validate|cleanup" >&2; exit 2 ;;
esac
