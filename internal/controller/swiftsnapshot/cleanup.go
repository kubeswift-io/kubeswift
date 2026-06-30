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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// HostPathFinalizer is added to local-backend SwiftSnapshots once they
// transition to Ready. The deletion handler runs the cleanup pod and
// removes the finalizer once the pod reports Succeeded.
const HostPathFinalizer = "kubeswift.io/snapshot-hostpath-cleanup"

// S3ObjectFinalizer is added to s3-backend (Tier C) SwiftSnapshots once
// they transition to Ready. The deletion handler runs a delete Job that
// purges the snapshot's object-storage prefix, then removes the finalizer.
const S3ObjectFinalizer = "kubeswift.io/snapshot-s3-cleanup"

// cleanupFinalizerFor returns the cleanup finalizer a SwiftSnapshot's
// backend needs, or "" for backends with no controller-managed artifact
// cleanup (csi-volume-snapshot — the VolumeSnapshot lifecycle handles it).
func cleanupFinalizerFor(snap *snapshotv1alpha1.SwiftSnapshot) string {
	switch snap.Spec.Backend.Type {
	case snapshotv1alpha1.SnapshotBackendLocal:
		return HostPathFinalizer
	case snapshotv1alpha1.SnapshotBackendS3:
		return S3ObjectFinalizer
	default:
		return ""
	}
}

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

// ensureFinalizer adds the backend's cleanup finalizer once a SwiftSnapshot
// reaches Ready: HostPathFinalizer for Tier B (local), S3ObjectFinalizer for
// Tier C (s3). No-op for csi-volume-snapshot — VolumeSnapshot deletion is
// handled via OwnerReferences, not a finalizer.
func (r *SwiftSnapshotReconciler) ensureFinalizer(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) error {
	fin := cleanupFinalizerFor(snap)
	if fin == "" {
		return nil
	}
	if snap.DeletionTimestamp != nil {
		// Don't add finalizers during deletion — pointless and triggers
		// the apiserver's "finalizer added during deletion" warning.
		return nil
	}
	if hasFinalizer(snap, fin) {
		return nil
	}
	patched := snap.DeepCopy()
	patched.Finalizers = append(patched.Finalizers, fin)
	return r.Patch(ctx, patched, client.MergeFrom(snap))
}

// handleDeletion dispatches the backend-specific artifact cleanup when a
// SwiftSnapshot is being deleted: Tier B (local) runs a node-pinned hostPath
// cleanup pod; Tier C (s3) runs a delete Job that purges the object-storage
// prefix. Each removes its finalizer once cleanup succeeds, so the apiserver
// can GC. A snapshot with neither finalizer (csi-volume-snapshot, or never
// reached Ready) has nothing to clean — done immediately.
//
// Returns (done, err): done=true means the finalizer is gone (or never
// existed); done=false (nil err) means cleanup is in flight — requeue.
func (r *SwiftSnapshotReconciler) handleDeletion(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
) (bool, error) {
	switch {
	case hasFinalizer(snap, S3ObjectFinalizer):
		return r.handleS3Deletion(ctx, snap)
	case hasFinalizer(snap, HostPathFinalizer):
		return r.handleLocalDeletion(ctx, snap)
	default:
		return true, nil
	}
}

// retainArtifacts reports whether the snapshot's deletionPolicy is Retain — in
// which case the deletion handlers drop the cleanup finalizer WITHOUT purging.
// An empty policy (snapshots created before the field existed) means Delete.
func retainArtifacts(snap *snapshotv1alpha1.SwiftSnapshot) bool {
	return snap.Spec.DeletionPolicy == snapshotv1alpha1.SnapshotDeletionPolicyRetain
}

// handleLocalDeletion runs the Tier B hostPath cleanup pod on the source node;
// once it reports Succeeded, removes HostPathFinalizer.
func (r *SwiftSnapshotReconciler) handleLocalDeletion(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
) (bool, error) {
	if !hasFinalizer(snap, HostPathFinalizer) {
		// Nothing to do — apiserver will GC the resource.
		return true, nil
	}
	// deletionPolicy: Retain — leave the hostPath, just drop the finalizer.
	if retainArtifacts(snap) {
		return r.removeFinalizer(ctx, snap)
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
				"snapshot.kubeswift.io/role":           "hostpath-cleanup",
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
	return r.removeNamedFinalizer(ctx, snap, HostPathFinalizer)
}

// removeNamedFinalizer strips a specific finalizer and patches. Returns
// (true, nil) on success so a caller can `return r.removeNamedFinalizer(...)`.
func (r *SwiftSnapshotReconciler) removeNamedFinalizer(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, name string) (bool, error) {
	patched := snap.DeepCopy()
	out := patched.Finalizers[:0]
	for _, f := range patched.Finalizers {
		if f != name {
			out = append(out, f)
		}
	}
	patched.Finalizers = out
	if err := r.Patch(ctx, patched, client.MergeFrom(snap)); err != nil {
		return false, err
	}
	return true, nil
}

// handleS3Deletion purges a Tier C snapshot's object-storage prefix via a
// delete Job, then removes S3ObjectFinalizer. Drops the finalizer without a
// purge in the cases where there is nothing to purge or no way to (never
// uploaded, no s3 config, or the snapshot-s3 image is unconfigured) — never
// wedge namespace deletion on a snapshot we cannot clean (the finalizer-trap
// lesson, Design Principle #10).
//
// deletionPolicy: Retain short-circuits the purge (drop the finalizer, keep the
// objects); Delete (the default) purges.
func (r *SwiftSnapshotReconciler) handleS3Deletion(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
) (bool, error) {
	if !hasFinalizer(snap, S3ObjectFinalizer) {
		return true, nil
	}
	// deletionPolicy: Retain — leave the S3 objects, just drop the finalizer.
	if retainArtifacts(snap) {
		return r.removeNamedFinalizer(ctx, snap, S3ObjectFinalizer)
	}
	// Nothing to purge: never uploaded, or no s3 config. Drop the finalizer.
	if snap.Status.S3 == nil || snap.Spec.Backend.S3 == nil || snap.Spec.Backend.S3.Bucket == "" {
		return r.removeNamedFinalizer(ctx, snap, S3ObjectFinalizer)
	}
	// Can't purge without the snapshot-s3 image — don't wedge deletion forever.
	if r.SnapshotS3Image == "" {
		log.FromContext(ctx).Info("snapshot-s3 image not configured; dropping S3 cleanup finalizer without purging objects (orphan objects remain)", "snapshot", snap.Name)
		return r.removeNamedFinalizer(ctx, snap, S3ObjectFinalizer)
	}

	podName := s3DeleteJobName(snap)
	var job batchv1.Job
	getErr := r.Get(ctx, client.ObjectKey{Name: podName, Namespace: snap.Namespace}, &job)
	if apierrors.IsNotFound(getErr) {
		if err := r.ensureDeleteJob(ctx, snap); err != nil {
			return false, fmt.Errorf("create s3 delete Job: %w", err)
		}
		return false, nil // Job just created; requeue.
	}
	if getErr != nil {
		return false, getErr
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			// Objects purged. Best-effort delete the Job (+ its pod) then drop
			// the finalizer so the apiserver GCs the SwiftSnapshot.
			_ = r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground))
			return r.removeNamedFinalizer(ctx, snap, S3ObjectFinalizer)
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			// Leave the finalizer so the failure is visible; operator can
			// `kubectl delete job` to retry, or delete the finalizer by hand.
			return false, nil
		}
	}
	return false, nil // still purging
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
