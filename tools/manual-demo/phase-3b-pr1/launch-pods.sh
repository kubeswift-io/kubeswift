#!/usr/bin/env bash
# Phase 3b PR 1 manual demo — bring up source SwiftGuest + raw
# destination pod for live migration handoff.
#
# Source: Phase 3a controller-built launcher pod (the well-trodden
# disk-boot RWX+Block path; no live-mode wiring needed because we'll
# hand-trigger the send action via annotations).
#
# Destination: hand-crafted from `kubectl get pod <src> -o yaml`,
# with metadata reset + nodeName=boba + KUBESWIFT_MIGRATION_ROLE=
# receiver env added. This mirrors what `newDstPod` does at runtime
# (Phase 3a dst_pod.go::newDstPod, LBA-1) — we replicate the spec
# by hand for PR 1's controller-less manual demo, then PR 2 wires
# the controller path that uses `newDstPod` directly.
set -euo pipefail

NS="${NS:-phase-3b-pr1-demo}"
GUEST_NAME="${GUEST_NAME:-pr1-guest}"
SRC_NODE="${SRC_NODE:-miles}"
DST_NODE="${DST_NODE:-boba}"
DST_POD_NAME="${GUEST_NAME}-dst"

step() { printf '\n[%s] %s\n' "$1" "$2"; }

# ─── 1/5 Namespace ─────────────────────────────────────────────────
step 1/5 "namespace ${NS}"
kubectl create ns "${NS}" --dry-run=client -o yaml | kubectl apply -f -

# ─── 2/5 SwiftImage ────────────────────────────────────────────────
step 2/5 "SwiftImage ubuntu-noble"
cat <<EOF | kubectl apply -f -
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: ubuntu-noble
  namespace: ${NS}
spec:
  cloneStrategy: copy
  format: qcow2
  rootDisk:
    size: 10Gi
  source:
    http:
      url: https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
EOF
echo "  waiting for SwiftImage to reach phase=Ready ..."
for i in $(seq 1 60); do
  phase=$(kubectl -n "${NS}" get swiftimage ubuntu-noble -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [ "${phase}" = "Ready" ]; then
    echo "  Ready (t+${i}0s)"
    break
  fi
  sleep 10
done

# ─── 3/5 SwiftSeedProfile + SwiftGuestClass ────────────────────────
step 3/5 "SwiftSeedProfile + SwiftGuestClass"
cat <<EOF | kubectl apply -f -
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: default
  namespace: ${NS}
spec:
  datasource: NoCloud
  metaData: |
    instance-id: ${GUEST_NAME}
    local-hostname: ${GUEST_NAME}
  userData: |
    #cloud-config
    hostname: ${GUEST_NAME}
    users:
      - name: kubeswift
        passwd: \$6\$kubeswift\$/2TeJTNbX7JcApzW0gTbSFzCAN4eFcOdnQsI0lVANFpXhHmjqLy7npyhHxvT65kpUd0YKVMBAbRt3MUfV6eCJ.
        sudo: ALL=(ALL) NOPASSWD:ALL
        lock_passwd: false
    runcmd:
      - systemctl enable --now getty@ttyS0.service
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: pr1-class
  namespace: ${NS}
spec:
  cpu: "2"
  memory: 4Gi
  rootDisk:
    format: raw
    size: 20Gi
  storage:
    accessMode: ReadWriteMany
    volumeMode: Block
    storageClassName: longhorn-migratable
EOF

# ─── 4/5 SwiftGuest pinned to source node ──────────────────────────
step 4/5 "SwiftGuest ${GUEST_NAME} pinned to ${SRC_NODE}"
cat <<EOF | kubectl apply -f -
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: ${GUEST_NAME}
  namespace: ${NS}
spec:
  imageRef:
    name: ubuntu-noble
  seedProfileRef:
    name: default
  guestClassRef:
    name: pr1-class
  nodeName: ${SRC_NODE}
  runPolicy: Running
EOF
echo "  waiting for SwiftGuest to reach phase=Running + IP populated ..."
for i in $(seq 1 60); do
  phase=$(kubectl -n "${NS}" get sg "${GUEST_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  ip=$(kubectl -n "${NS}" get sg "${GUEST_NAME}" -o jsonpath='{.status.network.primaryIP}' 2>/dev/null || true)
  if [ "${phase}" = "Running" ] && [ -n "${ip}" ]; then
    echo "  Running (t+${i}0s); IP=${ip}"
    break
  fi
  sleep 10
done

SRC_POD=$(kubectl -n "${NS}" get sg "${GUEST_NAME}" -o jsonpath='{.status.podRef.name}')
GUEST_IP=$(kubectl -n "${NS}" get sg "${GUEST_NAME}" -o jsonpath='{.status.network.primaryIP}')
if [ -z "${SRC_POD}" ] || [ -z "${GUEST_IP}" ]; then
  echo "ERROR: source pod or guest IP missing; cannot proceed." >&2
  exit 1
fi
echo "  src pod = ${SRC_POD}"
echo "  guest IP = ${GUEST_IP}"

# ─── 5/5 Hand-crafted destination pod ──────────────────────────────
step 5/5 "destination pod ${DST_POD_NAME} on ${DST_NODE}"
# Pull the live src pod spec, then mutate it to fit a receiver-mode
# dst pod. Mirrors what Phase 3a dst_pod.go::newDstPod does at
# runtime (LBA-1 — image is inherited from src by clone semantics);
# we replicate by hand for the controller-less PR 1 demo.
DST_POD_NAME="${DST_POD_NAME}" DST_NODE="${DST_NODE}" \
kubectl -n "${NS}" get pod "${SRC_POD}" -o yaml \
  | DST_POD_NAME="${DST_POD_NAME}" DST_NODE="${DST_NODE}" python3 -c '
import sys, yaml
p = yaml.safe_load(sys.stdin)
import os
dst = os.environ["DST_POD_NAME"]
node = os.environ["DST_NODE"]
# Reset metadata
p["metadata"] = {
    "name": dst,
    "namespace": p["metadata"]["namespace"],
    "labels": {
        **{k: v for k, v in p["metadata"].get("labels", {}).items() if k != "swift.kubeswift.io/canonical-pod"},
        "kubeswift.io/migration-role": "destination",
    },
    "annotations": {
        "kubeswift.io/migration-phase2-unsafe-plaintext": "ack",
    },
}
# Reset status, owner refs, etc.
p.pop("status", None)
# nodeName override
p["spec"]["nodeName"] = node
# Add receiver-mode env to the launcher container
for c in p["spec"]["containers"]:
    if c["name"] == "launcher":
        env = c.get("env", [])
        env = [e for e in env if e.get("name") != "KUBESWIFT_MIGRATION_ROLE"]
        env.append({"name": "KUBESWIFT_MIGRATION_ROLE", "value": "receiver"})
        c["env"] = env
yaml.safe_dump(p, sys.stdout)
' | kubectl apply -f -

echo "  waiting for destination pod to reach phase=Running ..."
for i in $(seq 1 60); do
  phase=$(kubectl -n "${NS}" get pod "${DST_POD_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [ "${phase}" = "Running" ]; then
    echo "  Running (t+${i}0s)"
    break
  fi
  sleep 10
done

echo
echo "READY for trigger-migration.sh"
echo "  source pod:      ${SRC_POD}        (node=${SRC_NODE})"
echo "  destination pod: ${DST_POD_NAME}   (node=${DST_NODE})"
echo "  guest IP:        ${GUEST_IP}"
echo
echo "Export these for trigger-migration.sh:"
echo "  export NS='${NS}' SRC_POD='${SRC_POD}' DST_POD='${DST_POD_NAME}' GUEST_IP='${GUEST_IP}'"
