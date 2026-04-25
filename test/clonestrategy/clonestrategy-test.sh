#!/usr/bin/env bash
# KubeSwift clone-strategy side-by-side acceptance test.
#
# Provisions one SwiftImage with cloneStrategy=copy and one with
# cloneStrategy=snapshot from the same source URL, then boots N guests
# from each and times the wall-clock from SwiftGuest creation to
# GuestRunning=True for each pair. The acceptance criterion (per the
# Phase 1 design) is: snapshot mean must be at least MIN_SPEEDUP_X faster
# than copy mean.
#
# Defaults: N=2 guests per strategy, MIN_SPEEDUP_X=3.
#
# Requires:
#   - kubectl, KubeSwift cluster with snapshot CRDs and controllers.
#   - Snapshot-capable CSI driver and a default VolumeSnapshotClass.

set -euo pipefail

NAMESPACE="${NAMESPACE:-default}"
N="${N:-2}"
MIN_SPEEDUP_X="${MIN_SPEEDUP_X:-3}"
GUEST_TIMEOUT_M="${GUEST_TIMEOUT_M:-10}"
VSCLASS=""
NO_CLEANUP=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --vsclass)        VSCLASS="$2"; shift 2 ;;
    --replicas)       N="$2"; shift 2 ;;
    --min-speedup)    MIN_SPEEDUP_X="$2"; shift 2 ;;
    --no-cleanup)     NO_CLEANUP=true; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "$VSCLASS" ]]; then
  VSCLASS=$(kubectl get volumesnapshotclass -o jsonpath='{range .items[?(@.metadata.annotations.snapshot\.storage\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1)
fi
if [[ -z "$VSCLASS" ]]; then
  echo "ERROR: no default VolumeSnapshotClass; pass --vsclass" >&2
  exit 2
fi

echo "=== KubeSwift clone-strategy side-by-side test ==="
echo "Namespace: $NAMESPACE  N=$N  MIN_SPEEDUP=${MIN_SPEEDUP_X}x  VSClass=$VSCLASS"
echo ""

cleanup() {
  if [[ "$NO_CLEANUP" == "true" ]]; then
    echo "(skipping cleanup; --no-cleanup)"
    return
  fi
  echo ""
  echo "--- Cleanup ---"
  for i in $(seq 1 "$N"); do
    kubectl delete -n "$NAMESPACE" --ignore-not-found \
      "swiftguest/cs-copy-${i}" "swiftguest/cs-snap-${i}" >/dev/null 2>&1 || true
  done
  kubectl delete -n "$NAMESPACE" --ignore-not-found \
    swiftimage/cs-source-copy swiftimage/cs-source-snap \
    swiftguestclass/cs-class swiftseedprofile/cs-seed >/dev/null 2>&1 || true
}
trap cleanup EXIT

kubectl apply -n "$NAMESPACE" -f - >/dev/null <<'EOF'
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: cs-class
spec:
  cpu: 2
  memory: "2Gi"
  rootDisk:
    size: "10Gi"
    format: raw
---
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: cs-seed
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    users:
      - name: ubuntu
        sudo: ALL=(ALL) NOPASSWD:ALL
EOF

URL="https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"

echo "--- Provisioning copy-strategy SwiftImage ---"
kubectl apply -n "$NAMESPACE" -f - >/dev/null <<EOF
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata: {name: cs-source-copy}
spec:
  format: qcow2
  rootDisk: {size: "10Gi"}
  cloneStrategy: copy
  source: {http: {url: "$URL"}}
EOF

echo "--- Provisioning snapshot-strategy SwiftImage ---"
kubectl apply -n "$NAMESPACE" -f - >/dev/null <<EOF
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata: {name: cs-source-snap}
spec:
  format: qcow2
  rootDisk: {size: "10Gi"}
  cloneStrategy: snapshot
  volumeSnapshotClassName: $VSCLASS
  source: {http: {url: "$URL"}}
EOF

echo "  Waiting for both SwiftImages Ready (15m)..."
kubectl wait --for=jsonpath='{.status.phase}'=Ready swiftimage/cs-source-copy -n "$NAMESPACE" --timeout=15m
kubectl wait --for=jsonpath='{.status.phase}'=Ready swiftimage/cs-source-snap -n "$NAMESPACE" --timeout=15m

# Boot N guests from each strategy, measuring wall-clock to Running.
boot_one() {
  local strategy="$1" idx="$2"
  local image="cs-source-${strategy}"
  local name="cs-${strategy}-${idx}"
  local started=$(date +%s)
  kubectl apply -n "$NAMESPACE" -f - >/dev/null <<EOF
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata: {name: $name}
spec:
  imageRef: {name: $image}
  guestClassRef: {name: cs-class}
  seedProfileRef: {name: cs-seed}
  runPolicy: Running
EOF
  if ! kubectl wait --for=jsonpath='{.status.phase}'=Running "swiftguest/$name" -n "$NAMESPACE" --timeout="${GUEST_TIMEOUT_M}m" >/dev/null 2>&1; then
    echo "  FAIL: $name did not reach Running within ${GUEST_TIMEOUT_M}m" >&2
    return 1
  fi
  local finished=$(date +%s)
  echo "$((finished - started))"
}

echo ""
echo "--- Booting $N guests with cloneStrategy=copy ---"
COPY_TIMES=()
for i in $(seq 1 "$N"); do
  t=$(boot_one copy "$i")
  COPY_TIMES+=("$t")
  echo "  cs-copy-$i: ${t}s"
done

echo ""
echo "--- Booting $N guests with cloneStrategy=snapshot ---"
SNAP_TIMES=()
for i in $(seq 1 "$N"); do
  t=$(boot_one snap "$i")
  SNAP_TIMES+=("$t")
  echo "  cs-snap-$i: ${t}s"
done

avg() { local sum=0; for x in "$@"; do sum=$((sum + x)); done; echo $((sum / $#)); }
copy_avg=$(avg "${COPY_TIMES[@]}")
snap_avg=$(avg "${SNAP_TIMES[@]}")

echo ""
echo "=== RESULTS ==="
printf "  copy avg:     %ss  (samples: %s)\n" "$copy_avg" "${COPY_TIMES[*]}"
printf "  snapshot avg: %ss  (samples: %s)\n" "$snap_avg" "${SNAP_TIMES[*]}"

if [[ "$snap_avg" -le 0 ]]; then
  echo "  FAIL: snapshot avg is zero — clock skew or instant return?"
  exit 1
fi

# integer compare: copy_avg / snap_avg >= MIN_SPEEDUP_X
ratio_x10=$(( (copy_avg * 10) / snap_avg ))
min_x10=$(( MIN_SPEEDUP_X * 10 ))
echo "  speedup:      ${ratio_x10}/10 x  (min required: ${min_x10}/10 x)"

if [[ "$ratio_x10" -ge "$min_x10" ]]; then
  echo ""
  echo "=== clone-strategy e2e PASS (snapshot is at least ${MIN_SPEEDUP_X}x faster) ==="
  exit 0
fi

echo ""
echo "=== clone-strategy e2e FAIL ==="
echo "  snapshot strategy did not beat copy strategy by the required margin."
echo "  This may indicate a CSI driver where snapshot+dataSource is implemented"
echo "  as a full copy (e.g. Longhorn). See docs/images/clone-strategies.md."
exit 1
