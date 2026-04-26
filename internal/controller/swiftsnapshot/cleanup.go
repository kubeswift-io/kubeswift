// Tier B (local-backend) hostPath cleanup finalizer.
//
// The on-disk snapshot directory is node-local state outside Kubernetes
// — the controller-manager pod can't reach it directly. When the
// SwiftSnapshot is deleted the directory has to be removed by a
// one-shot pod scheduled on the snapshot's source node.
//
// Scope: this finalizer cleans up the SwiftSnapshot's own snapshot
// directory only. Orphan cleanup (directories left behind by failed
// captures that never got finalizer-protected) is out of scope for
// Phase 2 — that belongs to a separate node-local janitor controller
// if it's needed later. Keeping this finalizer narrow ("delete what
// this resource owns") avoids cross-namespace reach and keeps the
// blast radius small.

package swiftsnapshot

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

// HostPathFinalizer is added to local-backend SwiftSnapshots once they
// transition to Ready. The deletion handler runs the cleanup pod and
// removes the finalizer once the pod reports Succeeded.
const HostPathFinalizer = "kubeswift.io/snapshot-hostpath-cleanup"

// CleanupImage is the container image used by the cleanup pod. Kept
// minimal — just needs `rm -rf` and a writable hostPath mount.
const CleanupImage = "busybox:1.36.1"

// HostPathBaseMount is where the parent /var/lib/kubeswift/snapshots/
// is mounted inside the cleanup pod. The pod removes a subdirectory
// of this mount; never the mount root itself (defense against an
// empty subdir name accidentally taking out other snapshots).
const HostPathBaseMount = "/snapshots"

// cleanupPodName derives the cleanup pod's name from the SwiftSnapshot.
// Stable so a re-run of the deletion handler is idempotent (Get returns
// the existing pod rather than creating a duplicate).
func cleanupPodName(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return "swift-snap-cleanup-" + snap.Name
}

// ensureFinalizer adds HostPathFinalizer to a Tier B SwiftSnapshot
// once it reaches Ready. No-op for csi-volume-snapshot snapshots —
// VolumeSnapshot deletion is handled via OwnerReferences, not a
// finalizer.
func (r *SwiftSnapshotReconciler) ensureFinalizer(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) error {
	if snap.Spec.Backend.Type != snapshotv1alpha1.SnapshotBackendLocal {
		return nil
	}
	if snap.DeletionTimestamp != nil {
		// Don't add finalizers during deletion — pointless and triggers
		// the apiserver's "finalizer added during deletion" warning.
		return nil
	}
	for _, f := range snap.Finalizers {
		if f == HostPathFinalizer {
			return nil
		}
	}
	patched := snap.DeepCopy()
	patched.Finalizers = append(patched.Finalizers, HostPathFinalizer)
	return r.Patch(ctx, patched, client.MergeFrom(snap))
}

// handleDeletion runs when DeletionTimestamp is set on a SwiftSnapshot
// that still carries HostPathFinalizer. Creates (or re-checks) a
// cleanup pod on the source node; once the pod reports Succeeded,
// removes the finalizer so the apiserver garbage-collects the
// SwiftSnapshot.
//
// Returns (done, requeue, err):
//   - done=true: finalizer removed. Caller can return without requeue.
//   - done=false, err==nil: pod still running or just created;
//     caller requeues.
func (r *SwiftSnapshotReconciler) handleDeletion(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
) (bool, error) {
	if !hasFinalizer(snap, HostPathFinalizer) {
		// Nothing to do — apiserver will GC the resource.
		return true, nil
	}
	// CSI-backed snapshots wouldn't have HostPathFinalizer in the first
	// place, but guard the cleanup logic against bad state where one
	// got added (e.g. operator hand-edited).
	if snap.Spec.Backend.Type != snapshotv1alpha1.SnapshotBackendLocal {
		return r.removeFinalizer(ctx, snap)
	}
	if snap.Spec.Backend.Local == nil || snap.Spec.Backend.Local.HostPath == "" {
		// No hostPath to clean. Drop the finalizer — there's nothing
		// for the cleanup pod to do.
		return r.removeFinalizer(ctx, snap)
	}
	// If the snapshot never recorded a node (e.g. failed during
	// Pending before we set status.NodeName), there's no node to
	// schedule the cleanup pod on. Best we can do is drop the
	// finalizer; orphan cleanup is operator-driven (out of scope per
	// the commit's narrowing).
	if snap.Status.NodeName == "" {
		return r.removeFinalizer(ctx, snap)
	}
	// hostPath subdir to remove. Defensive: we extract the trailing
	// path component and remove only that — never the parent.
	subdir := pathSubdir(snap.Spec.Backend.Local.HostPath)
	if subdir == "" || subdir == "." || subdir == "/" {
		// Malformed hostPath. Refuse to construct a cleanup command
		// that would touch the entire snapshot tree. Drop the
		// finalizer manually — operator must clean up by hand.
		return r.removeFinalizer(ctx, snap)
	}

	podName := cleanupPodName(snap)
	var pod corev1.Pod
	getErr := r.Get(ctx, client.ObjectKey{Name: podName, Namespace: snap.Namespace}, &pod)
	if apierrors.IsNotFound(getErr) {
		if err := r.createCleanupPod(ctx, snap, podName, subdir); err != nil {
			return false, fmt.Errorf("create cleanup pod: %w", err)
		}
		// Pod just created; requeue.
		return false, nil
	}
	if getErr != nil {
		return false, getErr
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		// Cleanup done. Best-effort delete of the pod itself (orphan
		// otherwise; the next reconcile would also see Succeeded and
		// skip creating a new one).
		_ = r.Delete(ctx, &pod)
		return r.removeFinalizer(ctx, snap)
	case corev1.PodFailed:
		// Pod ran but failed (e.g. permission error, stale mount).
		// Surface in status by leaving the finalizer; operator can
		// see the failure via `kubectl describe pod`. We don't
		// auto-retry (avoid loop on a permanent failure); operator
		// can `kubectl delete pod swift-snap-cleanup-...` to get a
		// re-create on the next reconcile.
		return false, nil
	default:
		// Pending / Running — requeue.
		return false, nil
	}
}

// createCleanupPod schedules a one-shot Pod that removes the snapshot
// subdir on the source node. The Pod mounts the parent
// /var/lib/kubeswift/snapshots/ and runs `rm -rf /snapshots/<subdir>/`.
func (r *SwiftSnapshotReconciler) createCleanupPod(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
	podName, subdir string,
) error {
	// We mount the parent dir, not the snapshot dir itself, so that
	// `rm -rf` can act on a subdir from inside the pod's namespace.
	// The hostPath base is HostPathBaseDir (the only prefix the
	// validation webhook permits).
	parent := HostPathBaseDir
	hostPathType := corev1.HostPathDirectoryOrCreate
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: snap.Namespace,
			Labels: map[string]string{
				"snapshot.kubeswift.io/role":         "hostpath-cleanup",
				"snapshot.kubeswift.io/swift-snapshot": snap.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(snap, swiftSnapshotGVK),
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      snap.Status.NodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "rm",
				Image: CleanupImage,
				// The shell-quoted subdir is generated from the
				// trailing path component of the operator-set
				// hostPath, which the webhook constrained to
				// /var/lib/kubeswift/snapshots/<...> with no `..`.
				// We additionally guard against empty/./.. above,
				// so this rm cannot escape the parent.
				Command: []string{"sh", "-c"},
				Args:    []string{fmt.Sprintf("rm -rf %s/%s", HostPathBaseMount, subdir)},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "snapshots",
					MountPath: HostPathBaseMount,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "snapshots",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: parent,
						Type: &hostPathType,
					},
				},
			}},
		},
	}
	return r.Create(ctx, pod)
}

func (r *SwiftSnapshotReconciler) removeFinalizer(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) (bool, error) {
	patched := snap.DeepCopy()
	out := patched.Finalizers[:0]
	for _, f := range patched.Finalizers {
		if f != HostPathFinalizer {
			out = append(out, f)
		}
	}
	patched.Finalizers = out
	if err := r.Patch(ctx, patched, client.MergeFrom(snap)); err != nil {
		return false, err
	}
	return true, nil
}

func hasFinalizer(snap *snapshotv1alpha1.SwiftSnapshot, target string) bool {
	for _, f := range snap.Finalizers {
		if f == target {
			return true
		}
	}
	return false
}

// pathSubdir returns the trailing path component of an absolute
// hostPath under HostPathBaseDir. Returns empty if the path doesn't
// match the expected shape — caller treats empty as a refusal-to-act.
func pathSubdir(hostPath string) string {
	hp := strings.TrimSuffix(hostPath, "/")
	if !strings.HasPrefix(hp, HostPathBaseDir) {
		return ""
	}
	tail := strings.TrimPrefix(hp, HostPathBaseDir)
	// Reject paths that contain further slashes — we only support a
	// single subdirectory under HostPathBaseDir, which is what the
	// webhook + controller produce.
	if strings.Contains(tail, "/") {
		return ""
	}
	if tail == "" || tail == "." || tail == ".." {
		return ""
	}
	return tail
}
