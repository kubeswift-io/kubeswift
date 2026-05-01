#!/usr/bin/env bash
#
# W9.x cluster integration test (issue #37).
#
# Verifies that a SwiftImage with cloneStrategy=snapshot can be cloned
# into a Block-mode destination PVC by a SwiftGuest declaring
# spec.storage.{accessMode: ReadWriteMany, volumeMode: Block,
# storageClassName: longhorn-migratable}. Without the fix, the CSI
# external-snapshotter rejects the clone PVC with the
# allow-volume-mode-change error captured in PR #35 walkthrough W11.
#
# A/B REPRODUCTION PATTERN
# ------------------------
#
# This script does NOT rebuild the controller. It expects the operator
# to redeploy with the W9.x fix BEFORE running the validate phase. The
# reproduce phase establishes the failure baseline against the OLD
# image; the validate phase confirms the fix works under the NEW image.
#
# Use:
#   ./w9x-snapshot-block.sh reproduce   # apply manifests; expect PVC ProvisioningFailed
#   # operator: deploy NEW controller image with the W9.x fix
#   ./w9x-snapshot-block.sh validate    # expect PVC bound + SwiftGuest Running
#   ./w9x-snapshot-block.sh w10check    # bundled W10 verification
#   ./w9x-snapshot-block.sh cleanup     # tear down test resources
#
# Or run the full sequence with manual redeploy in the middle:
#   ./w9x-snapshot-block.sh reproduce
#   # ... rebuild + redeploy controller ...
#   ./w9x-snapshot-block.sh validate
#   ./w9x-snapshot-block.sh w10check
#   ./w9x-snapshot-block.sh cleanup
#
# Prerequisites:
#   - KUBECONFIG pointing at a cluster with Longhorn (default class)
#   - StorageClass `longhorn-migratable` with parameters.migratable=true
#   - VolumeSnapshotClass `longhorn-snapshot` (Longhorn default)
#   - SwiftSeedProfile `minimal` in `default` namespace
#
# Operator workaround until W9.x ships: use cloneStrategy=copy for any
# RWX+Block guest. This script proves the snapshot path works post-fix.

set -euo pipefail

NS="${NS:-default}"
SWIFTIMAGE="${SWIFTIMAGE:-w9x-noble-snap}"
SWIFTGUESTCLASS="${SWIFTGUESTCLASS:-w9x-live-migratable}"
SWIFTGUEST="${SWIFTGUEST:-w9x-rwx-snap-test}"
FS_GUEST="${FS_GUEST:-w9x-fs-baseline}"  # for the W10 check
TIMEOUT_SECS="${TIMEOUT_SECS:-600}"

cmd="${1:-help}"

ensure_prereqs() {
    kubectl get sc longhorn-migratable >/dev/null \
        || { echo "ERROR: StorageClass longhorn-migratable not found"; exit 1; }
    [ "$(kubectl get sc longhorn-migratable -o jsonpath='{.parameters.migratable}')" = "true" ] \
        || { echo "ERROR: longhorn-migratable.parameters.migratable != true"; exit 1; }
    kubectl get volumesnapshotclass longhorn-snapshot >/dev/null \
        || { echo "ERROR: VolumeSnapshotClass longhorn-snapshot not found"; exit 1; }
    kubectl -n "$NS" get swiftseedprofile minimal >/dev/null \
        || { echo "ERROR: SwiftSeedProfile minimal not found in ns=$NS"; exit 1; }
    echo "OK: prerequisites present"
}

apply_manifests() {
    cat <<EOF | kubectl apply -f -
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: $SWIFTIMAGE
  namespace: $NS
spec:
  format: qcow2
  cloneStrategy: snapshot
  volumeSnapshotClassName: longhorn-snapshot
  rootDisk:
    size: "10Gi"
  source:
    http:
      url: https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: $SWIFTGUESTCLASS
spec:
  cpu: "2"
  memory: "4Gi"
  rootDisk:
    size: "40Gi"
    format: raw
  storage:
    accessMode: ReadWriteMany
    volumeMode: Block
    storageClassName: longhorn-migratable
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: $SWIFTGUEST
  namespace: $NS
spec:
  imageRef:
    name: $SWIFTIMAGE
  guestClassRef:
    name: $SWIFTGUESTCLASS
  seedProfileRef:
    name: minimal
  runPolicy: Running
EOF
}

wait_for_swiftimage_ready() {
    echo "waiting for SwiftImage $SWIFTIMAGE to become Ready (cloneSeed populated)..."
    local deadline=$(( $(date +%s) + TIMEOUT_SECS ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        local phase
        phase=$(kubectl -n "$NS" get swiftimage "$SWIFTIMAGE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        local seed
        seed=$(kubectl -n "$NS" get swiftimage "$SWIFTIMAGE" -o jsonpath='{.status.cloneSeed.name}' 2>/dev/null || true)
        echo "  swiftimage=$phase cloneSeed=$seed"
        [ "$phase" = "Ready" ] && [ -n "$seed" ] && return 0
        sleep 15
    done
    echo "TIMEOUT waiting for SwiftImage Ready"
    return 1
}

reproduce() {
    echo "=== W9.x REPRODUCE phase ==="
    echo "Goal: apply snapshot+Block manifests; PVC should fail with"
    echo "      allow-volume-mode-change error (PROVES the bug exists)."
    echo "Run this AGAINST THE OLD CONTROLLER IMAGE (pre-W9.x)."
    echo ""

    ensure_prereqs
    apply_manifests
    wait_for_swiftimage_ready

    echo ""
    echo "Waiting up to 90s for PVC ProvisioningFailed event..."
    local pvc="swiftguest-root-$SWIFTGUEST"
    local deadline=$(( $(date +%s) + 90 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if kubectl -n "$NS" get events --field-selector involvedObject.name="$pvc" \
                -o json 2>/dev/null | grep -q 'allow-volume-mode-change'; then
            echo ""
            echo "=== EXPECTED FAILURE OBSERVED ==="
            kubectl -n "$NS" get events --field-selector involvedObject.name="$pvc" \
                --sort-by=.lastTimestamp 2>&1 | grep -i 'allow-volume-mode-change\|ProvisioningFailed' | tail -5
            echo ""
            echo "Reproduction successful. Now redeploy with the W9.x fix and run:"
            echo "    $0 validate"
            return 0
        fi
        sleep 5
    done
    echo "WARNING: did not observe the expected allow-volume-mode-change error within 90s."
    echo "Possible causes: (a) cluster already has the fix deployed; (b) a different"
    echo "CSI driver is in use; (c) Longhorn version differs. Inspect events:"
    kubectl -n "$NS" get events --field-selector involvedObject.name="$pvc" --sort-by=.lastTimestamp
    return 1
}

validate() {
    echo "=== W9.x VALIDATE phase ==="
    echo "Goal: with the fix deployed, the snapshot+Block guest reaches Phase=Running."
    echo "Run this AFTER redeploying the controller image with the W9.x fix."
    echo ""

    # Re-apply ensures the manifests are in a sane state even if reproduce
    # was skipped or partially completed. Apply is idempotent.
    apply_manifests
    wait_for_swiftimage_ready

    # The fix patches the VSC annotation post-bind. Existing PVC may be in
    # ProvisioningFailed state from the reproduce phase; deletion + re-creation
    # picks up the now-annotated VSC.
    local pvc="swiftguest-root-$SWIFTGUEST"
    if kubectl -n "$NS" get pvc "$pvc" >/dev/null 2>&1; then
        local pvc_phase
        pvc_phase=$(kubectl -n "$NS" get pvc "$pvc" -o jsonpath='{.status.phase}')
        if [ "$pvc_phase" != "Bound" ]; then
            echo "PVC $pvc is in phase=$pvc_phase (likely ProvisioningFailed from reproduce);"
            echo "deleting so the controller recreates with the now-annotated VSC."
            kubectl -n "$NS" delete pvc "$pvc" --wait=false || true
            sleep 5
        fi
    fi

    echo "Waiting for SwiftGuest $SWIFTGUEST to reach Phase=Running with primaryIP..."
    local deadline=$(( $(date +%s) + TIMEOUT_SECS ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        local phase ip
        phase=$(kubectl -n "$NS" get swiftguest "$SWIFTGUEST" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        ip=$(kubectl -n "$NS" get swiftguest "$SWIFTGUEST" -o jsonpath='{.status.network.primaryIP}' 2>/dev/null || true)
        echo "  guest=$phase ip=$ip"
        if [ "$phase" = "Running" ] && [ -n "$ip" ]; then
            echo ""
            echo "=== W9.x FIX VALIDATED ==="
            local vsc
            vsc=$(kubectl -n "$NS" get volumesnapshot "${SWIFTIMAGE}-clone-seed" \
                -o jsonpath='{.status.boundVolumeSnapshotContentName}')
            local annot
            annot=$(kubectl get volumesnapshotcontent "$vsc" \
                -o jsonpath='{.metadata.annotations.snapshot\.storage\.kubernetes\.io/allow-volume-mode-change}')
            echo "VolumeSnapshotContent: $vsc"
            echo "allow-volume-mode-change annotation: $annot (expected: true)"
            kubectl -n "$NS" get pvc "$pvc" -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,ACCESS:.spec.accessModes,VOLUMEMODE:.spec.volumeMode,CLASS:.spec.storageClassName
            return 0
        fi
        sleep 15
    done
    echo "TIMEOUT waiting for SwiftGuest Running"
    kubectl -n "$NS" get events --field-selector involvedObject.name="$pvc" --sort-by=.lastTimestamp | tail -10
    return 1
}

w10check() {
    echo "=== W10 verification (binary decision rule) ==="
    echo "Per PR #35 walkthrough W10: the launcher logs of Block-mode guests show"
    echo "two boot-time CH 'Request check failed: ... ReadOnly' WARNs at sector 0."
    echo "Open question: does this happen on Filesystem-mode guests too?"
    echo ""
    echo "Method: deploy a Filesystem-mode guest, count ReadOnly WARNs in launcher logs."
    echo ""

    cat <<EOF | kubectl apply -f -
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: w9x-fs-baseline
spec:
  cpu: "2"
  memory: "2Gi"
  rootDisk:
    size: "10Gi"
    format: raw
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: $FS_GUEST
  namespace: $NS
spec:
  imageRef:
    name: $SWIFTIMAGE  # snapshot SwiftImage from the validate phase
  guestClassRef:
    name: w9x-fs-baseline
  seedProfileRef:
    name: minimal
  runPolicy: Running
EOF

    echo "Waiting for $FS_GUEST to reach Running..."
    local deadline=$(( $(date +%s) + TIMEOUT_SECS ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        local phase ip
        phase=$(kubectl -n "$NS" get swiftguest "$FS_GUEST" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        ip=$(kubectl -n "$NS" get swiftguest "$FS_GUEST" -o jsonpath='{.status.network.primaryIP}' 2>/dev/null || true)
        if [ "$phase" = "Running" ] && [ -n "$ip" ]; then break; fi
        sleep 15
    done

    # Give CH a few seconds to log boot messages.
    sleep 20

    local count
    count=$(kubectl -n "$NS" logs "$FS_GUEST" -c launcher 2>/dev/null | grep -c 'ReadOnly' || echo 0)
    echo ""
    echo "=== W10 decision ==="
    echo "Filesystem-mode guest $FS_GUEST launcher log 'ReadOnly' WARN count: $count"
    if [ "$count" -eq 0 ]; then
        echo ""
        echo "DECISION: W10 is BLOCK-SPECIFIC."
        echo "Action: file W10 as a separate GitHub issue with full launcher log"
        echo "        evidence (kubectl -n $NS logs <block-guest-pod> -c launcher | grep ReadOnly)"
        echo "        and CH version (cloud-hypervisor v51.1). Cross-link to PR #35."
    else
        echo ""
        echo "DECISION: W10 is GENERIC CH BOOT NOISE."
        echo "Action: close W10 as wontfix with a one-line note in kubeswift_context.md"
        echo "        under PR #32 walkthrough findings: 'W10 — boot-time CH ReadOnly WARN"
        echo "        appears on both Block and Filesystem guests; generic CH v51.1 diagnostic"
        echo "        noise; no functional impact; closed as wontfix.'"
    fi
}

cleanup() {
    echo "=== cleanup ==="
    kubectl -n "$NS" delete swiftguest "$SWIFTGUEST" "$FS_GUEST" --wait=false 2>/dev/null || true
    kubectl delete swiftguestclass "$SWIFTGUESTCLASS" w9x-fs-baseline --wait=false 2>/dev/null || true
    kubectl -n "$NS" delete swiftimage "$SWIFTIMAGE" --wait=false 2>/dev/null || true
    echo "queued for deletion. PVCs will be GC'd via finalizer chain."
}

case "$cmd" in
    reproduce) reproduce ;;
    validate)  validate ;;
    w10check)  w10check ;;
    cleanup)   cleanup ;;
    help|*)
        cat <<EOF
W9.x cluster integration test (Issue #37).

Usage:
  $0 reproduce   # apply manifests; expect PVC ProvisioningFailed pre-fix
  $0 validate    # post-redeploy; expect PVC bound + SwiftGuest Running
  $0 w10check    # bundled W10 binary-decision verification
  $0 cleanup     # tear down test resources

Env overrides:
  NS=$NS
  SWIFTIMAGE=$SWIFTIMAGE
  SWIFTGUESTCLASS=$SWIFTGUESTCLASS
  SWIFTGUEST=$SWIFTGUEST
  FS_GUEST=$FS_GUEST
  TIMEOUT_SECS=$TIMEOUT_SECS

The script does NOT redeploy the controller — that is the operator's
responsibility between reproduce and validate. The A/B contrast (failure
pre-fix vs success post-fix) is the load-bearing piece per PR #35
walkthrough's binding scope addition.
EOF
        ;;
esac
