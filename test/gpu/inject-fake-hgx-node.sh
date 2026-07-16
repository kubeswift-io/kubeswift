#!/usr/bin/env bash
set -euo pipefail

# inject-fake-hgx-node.sh -- inject (or remove) a hardware-faithful, fake 8-GPU
# HGX H100 SwiftGPUNode so the SwiftGPU allocation + GPU-pod/intent-build path
# can be exercised on a cluster that has NO HGX hardware.
#
# The node's status is taken verbatim from the NVIDIA HGX Shared NVSwitch GPU
# Passthrough Virtualization Integration Guide (WP-12736-002): 8 H100 SXM GPUs
# (BDFs 0f/10/41/44/86/87/b8/bb), 4 NVSwitches, and the real Fabric Manager
# partition table. CRITICALLY, the partition gpuIndices are in device-Index
# space -- exactly what gpu-discovery produces AFTER translating the FM
# physical/Module IDs (which do NOT follow lspci order) via `nvidia-smi -q`.
# See docs/gpu/hgx-hardware-free-validation.md.
#
# This is a TEST FIXTURE. It does not represent real hardware; a launcher pod
# pinned to this node stays Pending (the node name is not a real kubelet). The
# validation boundary is everything up to VFIO bind / NVLink / Fabric Manager,
# which is irreducibly hardware-gated.
#
# Usage:
#   ./inject-fake-hgx-node.sh          # create/refresh the fake node
#   ./inject-fake-hgx-node.sh remove   # delete it
#
# Honors $KUBECONFIG and $NODE_NAME (default: fake-hgx-0).

NODE_NAME="${NODE_NAME:-fake-hgx-0}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STATUS_JSON="${SCRIPT_DIR}/fake-hgx-h100-status.json"

if [ "${1:-inject}" = "remove" ]; then
  echo "Removing fake HGX SwiftGPUNode ${NODE_NAME}"
  kubectl delete swiftgpunode "${NODE_NAME}" --ignore-not-found
  exit 0
fi

echo "Injecting fake HGX SwiftGPUNode ${NODE_NAME}"

# 1. Create the object (metadata only -- status is a subresource, ignored by apply).
kubectl apply -f - <<EOF
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUNode
metadata:
  name: ${NODE_NAME}
  labels:
    kubeswift.io/gpu-node: "true"
    kubeswift.io/fake-hgx: "true"
EOF

# 2. Patch the status subresource with the fake HGX inventory.
kubectl patch swiftgpunode "${NODE_NAME}" \
  --subresource=status --type=merge --patch-file "${STATUS_JSON}"

echo
echo "Injected. Current state:"
kubectl get swiftgpunode "${NODE_NAME}" \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,GPUS:.status.gpuCount,FREE:.status.freeGPUs,MODEL:.status.gpuModel,VFIO:.status.vfioReady
echo
echo "Fabric Manager partitions (id -> device indices):"
kubectl get swiftgpunode "${NODE_NAME}" -o jsonpath='{range .status.fabricManager.partitions[*]}{"  partition "}{.id}{" -> "}{.gpuIndices}{"\n"}{end}'
