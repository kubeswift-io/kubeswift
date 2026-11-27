#!/usr/bin/env bash
# In-place disk rollback e2e (#711): a csi-volume-snapshot SwiftRestore onto
# the snapshot's own guest rolls its root disk back, keeping the guest.
#
# Checks, on a cluster with a snapshot-capable CSI driver:
#   - a file written before the snapshot is back after the restore, and one
#     written after it is gone;
#   - the guest runs from swiftguest-root-<guest>-g1; the PVC it had before is
#     kept, retired, because the snapshot was taken from it (some drivers keep
#     a snapshot inside its source volume);
#   - a snapshot taken after the swap captures the swapped-in PVC;
#   - a second rollback goes to generation 2 and deletes generation 1;
#   - a launcher restart keeps the restored disk;
#   - once the snapshots are deleted, the retired disks are deleted.
#
# Requires: kubectl; KubeSwift with CRDs; a disk-boot SwiftImage and a
# SwiftGuestClass in $NAMESPACE; an SSH key whose public half goes into the
# guest's seed (the test reaches the guest by ssh from its launcher).
#
# Usage:
#   ./inplace-rollback-test.sh [--vsclass <name>] [--image <swiftimage>]
#       [--class <swiftguestclass>] [--node <node>] [--namespace <ns>] [--no-cleanup]

set -euo pipefail

NAMESPACE="${NAMESPACE:-default}"
IMAGE="${IMAGE:-ubuntu-noble}"
CLASS="${CLASS:-default}"
NODE="${NODE:-}"
VSCLASS=""
NO_CLEANUP=false
IDENTITY="${KUBESWIFT_TEST_IDENTITY:-${HOME}/.ssh/id_ed25519}"
SAMPLES_DIR="$(cd "$(dirname "$0")/../../config/samples/local-snapshots" && pwd)"
GUEST=inplace-rollback
TIMEOUT="${TIMEOUT:-600}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --vsclass)    VSCLASS="$2"; shift 2 ;;
    --image)      IMAGE="$2"; shift 2 ;;
    --class)      CLASS="$2"; shift 2 ;;
    --node)       NODE="$2"; shift 2 ;;
    --namespace)  NAMESPACE="$2"; shift 2 ;;
    --identity)   IDENTITY="$2"; shift 2 ;;
    --no-cleanup) NO_CLEANUP=true; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

log() { printf '==> %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl -n "$NAMESPACE" "$@"; }

# until_ok SECONDS DESCRIPTION CMD...
until_ok() {
  local deadline=$((SECONDS + $1)) what=$2
  shift 2
  until "$@" >/dev/null 2>&1; do
    [ "$SECONDS" -lt "$deadline" ] || die "timed out: $what"
    sleep 3
  done
}

cleanup() {
  if [[ "$NO_CLEANUP" == "true" ]]; then
    echo "(skipping cleanup; --no-cleanup)"
    return
  fi
  log "cleanup"
  k delete --ignore-not-found --wait=false \
    swiftrestore/${GUEST}-r1 swiftrestore/${GUEST}-r2 \
    swiftsnapshot/${GUEST}-s1 swiftsnapshot/${GUEST}-s2 \
    swiftguest/${GUEST} swiftseedprofile/${GUEST}-seed >/dev/null 2>&1 || true
}
trap cleanup EXIT

# guest_exec GUEST CMD: run CMD in the guest over ssh from its launcher.
guest_exec() {
  local guest="$1"; shift
  local ip pod cmd_b64
  ip=$(k get swiftguest "$guest" -o jsonpath='{.status.network.primaryIP}')
  pod=$(k get swiftguest "$guest" -o jsonpath='{.status.podRef.name}')
  [[ -n "$ip" ]] || return 1
  [[ -n "$pod" ]] || pod="$guest"
  cmd_b64=$(printf '%s' "$*" | base64 -w0)
  cat <<EOF | k exec -i "$pod" -c launcher -- sh
set -eu
KEY=\$(mktemp); chmod 600 "\$KEY"
cat > "\$KEY" <<'KUBESWIFT_TEST_KEY'
$(cat "$IDENTITY")
KUBESWIFT_TEST_KEY
CMD=\$(printf %s '$cmd_b64' | base64 -d)
RC=0
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=5 -i "\$KEY" kubeswift@$ip "\$CMD" || RC=\$?
rm -f "\$KEY"
exit \$RC
EOF
}

marker() { guest_exec "$GUEST" 'cat /var/lib/ks-marker'; }
ssh_ready() { guest_exec "$GUEST" true; }
restore_ready() { [ "$(k get swiftrestore "$1" -o jsonpath='{.status.phase}')" = Ready ]; }
snap_ready() { [ "$(k get swiftsnapshot "$1" -o jsonpath='{.status.phase}')" = Ready ]; }
pvc_gone() { ! k get pvc "$1"; }
retired() { [ "$(k get pvc "$1" -o jsonpath='{.metadata.labels.swift\.kubeswift\.io/role}')" = root-disk-retired ]; }
launcher_claim() {
  local pod
  pod=$(k get swiftguest "$GUEST" -o jsonpath='{.status.podRef.name}')
  k get pod "${pod:-$GUEST}" -o jsonpath='{.spec.volumes[?(@.name=="root-disk")].persistentVolumeClaim.claimName}'
}

snapshot() {
  cat <<EOF | k apply -f - >/dev/null
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftSnapshot
metadata: {name: $1}
spec:
  guestRef: {name: $GUEST}
  backend:
    type: csi-volume-snapshot
    csiVolumeSnapshot: {volumeSnapshotClassName: $VSCLASS}
EOF
  until_ok "$TIMEOUT" "snapshot $1 Ready" snap_ready "$1"
}

restore() { # restore NAME SNAPSHOT
  cat <<EOF | k apply -f - >/dev/null
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftRestore
metadata: {name: $1}
spec:
  snapshotRef: {name: $2}
  targetGuest: {name: $GUEST, overwriteExisting: true}
EOF
}

if [[ -z "$VSCLASS" ]]; then
  VSCLASS=$(kubectl get volumesnapshotclass -o jsonpath='{range .items[?(@.metadata.annotations.snapshot\.storage\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{"\n"}{end}' | head -1)
fi
[[ -n "$VSCLASS" ]] || die "no default VolumeSnapshotClass; pass --vsclass"
[[ -r "$IDENTITY" ]] || die "SSH identity $IDENTITY not readable (--identity)"

log "guest $GUEST from $IMAGE (class $CLASS), snapshots via $VSCLASS"
pubkey="$(cat "${IDENTITY}.pub" 2>/dev/null || ssh-keygen -y -f "$IDENTITY")"
pubkey="$(printf '%s' "$pubkey" | awk '{print $1, $2}')"
sed -E "s|^([[:space:]]*- )ssh-[^ ]+ .*|\1${pubkey}|; s|name: snapshot-local-test-seed|name: ${GUEST}-seed|" \
  "$SAMPLES_DIR/01-seed-profile.yaml" | k apply -f - >/dev/null
cat <<EOF | k apply -f - >/dev/null
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata: {name: $GUEST}
spec:
  imageRef: {name: $IMAGE}
  guestClassRef: {name: $CLASS}
  seedProfileRef: {name: ${GUEST}-seed}
  runPolicy: Running
${NODE:+  nodeName: $NODE}
EOF
until_ok "$TIMEOUT" "guest reachable by ssh" ssh_ready
gen0="swiftguest-root-$GUEST"
[ "$(launcher_claim)" = "$gen0" ] || die "launcher mounts $(launcher_claim), want $gen0"

log "write 'before', snapshot, write 'after'"
guest_exec "$GUEST" 'echo before | sudo tee /var/lib/ks-marker >/dev/null && sync'
snapshot ${GUEST}-s1
guest_exec "$GUEST" 'echo after | sudo tee /var/lib/ks-marker >/dev/null && sync'
[ "$(marker)" = after ] || die "marker not written"

log "roll back in place to ${GUEST}-s1"
start=$SECONDS
restore ${GUEST}-r1 ${GUEST}-s1
until_ok "$TIMEOUT" "restore r1 Ready" restore_ready ${GUEST}-r1
log "r1 Ready after $((SECONDS - start))s"
gen1="swiftguest-root-$GUEST-g1"
[ "$(k get swiftrestore ${GUEST}-r1 -o jsonpath='{.status.rootDisk.claimName}')" = "$gen1" ] || die "status.rootDisk.claimName"
[ "$(launcher_claim)" = "$gen1" ] || die "launcher mounts $(launcher_claim), want $gen1"
retired "$gen0" || die "$gen0 is not retired (s1 was taken from it)"
until_ok "$TIMEOUT" "guest reachable after the rollback" ssh_ready
[ "$(marker)" = before ] || die "marker is '$(marker)' after the rollback, want 'before'"
[ -z "$(k get swiftguest $GUEST -o jsonpath='{.metadata.annotations.snapshot\.kubeswift\.io/disk-restore}')" ] || die "guest still held"
log "rolled back: marker 'before', root disk $gen1, $gen0 retired"

log "a snapshot after the swap captures $gen1"
guest_exec "$GUEST" 'echo after2 | sudo tee /var/lib/ks-marker >/dev/null && sync'
snapshot ${GUEST}-s2
vs=$(k get swiftsnapshot ${GUEST}-s2 -o jsonpath='{.status.disks[0].handle}')
src=$(k get volumesnapshot "${vs#*/}" -o jsonpath='{.spec.source.persistentVolumeClaimName}')
[ "$src" = "$gen1" ] || die "snapshot s2 captured $src, want $gen1"

log "second rollback to ${GUEST}-s1 goes to generation 2"
restore ${GUEST}-r2 ${GUEST}-s1
until_ok "$TIMEOUT" "restore r2 Ready" restore_ready ${GUEST}-r2
gen2="swiftguest-root-$GUEST-g2"
[ "$(launcher_claim)" = "$gen2" ] || die "launcher mounts $(launcher_claim), want $gen2"
retired "$gen1" || die "$gen1 is not retired (s2 was taken from it)"
until_ok "$TIMEOUT" "guest reachable after the second rollback" ssh_ready
[ "$(marker)" = before ] || die "marker is '$(marker)' after the second rollback"

log "deleting the snapshots frees the retired disks; a launcher restart keeps $gen2"
k delete swiftsnapshot ${GUEST}-s1 ${GUEST}-s2 --wait=true --timeout=180s >/dev/null
vs_gone() { [ -z "$(k get volumesnapshot -o name | grep "swift-snap-${GUEST}-")" ]; }
until_ok 180 "VolumeSnapshots deleted" vs_gone
pod=$(k get swiftguest "$GUEST" -o jsonpath='{.status.podRef.name}')
k delete pod "${pod:-$GUEST}" --wait=true >/dev/null
until_ok "$TIMEOUT" "guest reachable after the restart" ssh_ready
[ "$(launcher_claim)" = "$gen2" ] || die "after restart the launcher mounts $(launcher_claim)"
[ "$(marker)" = before ] || die "marker is '$(marker)' after the restart"
until_ok 180 "retired $gen0 deleted" pvc_gone "$gen0"
until_ok 180 "retired $gen1 deleted" pvc_gone "$gen1"

log "PASS: in-place rollback restores the disk and keeps the guest"
