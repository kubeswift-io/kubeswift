// Tier B (local-backend) hostPath cleanup finalizer.
//
// The on-disk snapshot directory is node-local state outside Kubernetes
// — the controller-manager pod can't reach it directly. When the
// SwiftSnapshot is deleted the directory has to be removed by a
// one-shot pod scheduled on the snapshot's source node. The s3 and oci
// backends use the same pod for the copy their capture leaves on its node.
//
// That pod runs in the controller's own namespace, not the snapshot's.
// Deleting a namespace is an ordinary way to delete its snapshots, and a
// namespace being deleted refuses new pods: a cleanup pod there could
// never be created, and the finalizer held the namespace in Terminating
// for good. A pod that mounts the host's snapshot tree does not belong in
// a tenant's namespace either. An owner reference cannot cross
// namespaces, so the controller deletes the pod itself: once it has
// succeeded, or once its snapshot is gone (deleteOrphanCleanupPods).
//
// Scope: this finalizer cleans up the SwiftSnapshot's own snapshot
// directory only. Orphan cleanup (directories left behind by failed
// captures that never got finalizer-protected) is out of scope for
// Phase 2 — that belongs to a separate node-local janitor controller
// if it's needed later. Keeping this finalizer narrow ("delete what
// this resource owns") keeps the blast radius small.

package swiftsnapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
	swiftsnapshotwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsnapshot"
)

// HostPathFinalizer is added to local-backend SwiftSnapshots once they
// transition to Ready. The deletion handler runs the cleanup pod and
// removes the finalizer once the pod reports Succeeded.
const HostPathFinalizer = "kubeswift.io/snapshot-hostpath-cleanup"

// S3ObjectFinalizer is added to s3-backend (Tier C) SwiftSnapshots once
// they transition to Ready. The deletion handler runs a delete Job that
// purges the snapshot's object-storage prefix, then removes the finalizer.
const S3ObjectFinalizer = "kubeswift.io/snapshot-s3-cleanup"

// OCIArtifactFinalizer is added to oci-backend SwiftSnapshots once they reach
// Ready. The deletion handler deletes the pushed artifact(s) from the registry
// and the capture node's local copy, then removes the finalizer.
const OCIArtifactFinalizer = "kubeswift.io/snapshot-oci-cleanup"

// cleanupFinalizerFor returns the cleanup finalizer a SwiftSnapshot's
// backend needs, or "" for backends with no controller-managed artifact
// cleanup (csi-volume-snapshot — the VolumeSnapshot lifecycle handles it).
func cleanupFinalizerFor(snap *snapshotv1alpha1.SwiftSnapshot) string {
	switch snap.Spec.Backend.Type {
	case snapshotv1alpha1.SnapshotBackendLocal:
		return HostPathFinalizer
	case snapshotv1alpha1.SnapshotBackendS3:
		return S3ObjectFinalizer
	case snapshotv1alpha1.SnapshotBackendOCI:
		return OCIArtifactFinalizer
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

// Labels on a cleanup pod. The controller's namespace holds the cleanup pods
// of every namespace's snapshots, so a pod names its snapshot by namespace and
// UID as well as by name.
const (
	cleanupRoleLabel       = "snapshot.kubeswift.io/role"
	cleanupRoleValue       = "hostpath-cleanup"
	snapshotNameLabel      = "snapshot.kubeswift.io/swift-snapshot"
	snapshotNamespaceLabel = "snapshot.kubeswift.io/swift-snapshot-namespace"
	snapshotUIDLabel       = "snapshot.kubeswift.io/swift-snapshot-uid"
)

// ReasonPurgeSkipped is the Warning event a deletion emits when it drops a
// remote backend's finalizer without purging, because the snapshot's
// namespace is being deleted.
const ReasonPurgeSkipped = "PurgeSkipped"

const (
	cleanupPodPrefix = "swift-snap-cleanup-"
	// maxCleanupPodName keeps the name a DNS label: a pod's hostname is its
	// name, cut short when longer.
	maxCleanupPodName = 63
)

// cleanupPodName derives the cleanup pod's name from the SwiftSnapshot.
// Stable so a re-run of the deletion handler is idempotent (Get returns
// the existing pod rather than creating a duplicate). The hash of the
// snapshot's namespace, name and UID keeps it unique in the controller's
// namespace, where same-named snapshots of different namespaces meet.
func cleanupPodName(snap *snapshotv1alpha1.SwiftSnapshot) string {
	sum := sha256.Sum256([]byte(snap.Namespace + "/" + snap.Name + "/" + string(snap.UID)))
	return names.Bounded(cleanupPodPrefix+snap.Name, "-"+hex.EncodeToString(sum[:5]), maxCleanupPodName)
}

// legacyCleanupPodName is the name earlier versions gave the cleanup pod,
// which they ran in the snapshot's own namespace.
func legacyCleanupPodName(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return cleanupPodPrefix + snap.Name
}

// cleanupPodNamespace is where the cleanup pods of the snapshots in namespace
// run: the controller's namespace, or the snapshot's own when the controller
// does not know its namespace.
func (r *SwiftSnapshotReconciler) cleanupPodNamespace(namespace string) string {
	if r.ControllerNamespace != "" {
		return r.ControllerNamespace
	}
	return namespace
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
// cleanup pod in the controller's namespace; Tier C (s3) runs a delete Job
// that purges the object-storage prefix. Each removes its finalizer once
// cleanup succeeds, so the apiserver can GC. A snapshot with neither
// finalizer (csi-volume-snapshot, or never reached Ready) has nothing to
// clean — done immediately.
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
	case hasFinalizer(snap, OCIArtifactFinalizer):
		return r.handleOCIDeletion(ctx, snap)
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
	// If the snapshot never recorded a node (e.g. failed during
	// Pending before we set status.NodeName), no capture began, so
	// nothing was written and there's no node to schedule the cleanup
	// pod on. Drop the finalizer.
	if snap.Status.NodeName == "" {
		return r.removeFinalizer(ctx, snap)
	}
	// The directory the capture went into (clonecommon.NodeDir), not
	// spec.backend.local.hostPath: a snapshot that never captured wrote
	// nothing, and deleting what its spec names would let a tenant name
	// another tenant's directory and delete the snapshot to remove it.
	// Defensive: we extract the trailing path component and remove only
	// that — never the parent.
	subdir := pathSubdir(captureDestDir(snap))
	if subdir == "" || subdir == "." || subdir == "/" {
		// Malformed hostPath. Refuse to construct a cleanup command
		// that would touch the entire snapshot tree. Drop the
		// finalizer manually — operator must clean up by hand.
		return r.removeFinalizer(ctx, snap)
	}

	done, err := r.cleanupNodeDir(ctx, snap, subdir)
	if err != nil || !done {
		return false, err
	}
	return r.removeFinalizer(ctx, snap)
}

// cleanupNodeDir removes <HostPathBaseDir>/<subdir> on the snapshot's capture
// node with a one-shot pod, reporting done once the pod has succeeded.
func (r *SwiftSnapshotReconciler) cleanupNodeDir(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
	subdir string,
) (bool, error) {
	key := client.ObjectKey{Namespace: r.cleanupPodNamespace(snap.Namespace), Name: cleanupPodName(snap)}
	var pod corev1.Pod
	getErr := r.Get(ctx, key, &pod)
	if apierrors.IsNotFound(getErr) {
		// A pod an earlier version started in the snapshot's namespace may
		// already have done the work.
		done, err := r.legacyCleanupDone(ctx, snap)
		if err != nil || done {
			return done, err
		}
		if err := r.createCleanupPod(ctx, snap, key, subdir); err != nil {
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
		// Cleanup done. The pod goes before the finalizer does: nothing
		// owns it, so nothing else would delete it. Should the finalizer
		// update then fail, the next pass runs a second, harmless pod.
		if err := r.Delete(ctx, &pod); client.IgnoreNotFound(err) != nil {
			return false, err
		}
		return true, nil
	case corev1.PodFailed:
		// Pod ran but failed (e.g. permission error, stale mount).
		// Surface in status by leaving the finalizer; operator can
		// see the failure via `kubectl describe pod`. We don't
		// auto-retry (avoid loop on a permanent failure); operator
		// can delete the pod (in the controller's namespace, found by
		// its snapshot.kubeswift.io/swift-snapshot* labels) to get a
		// re-create on the next reconcile.
		return false, nil
	default:
		// Pending / Running — requeue.
		return false, nil
	}
}

// legacyCleanupDone reports whether a cleanup pod an earlier version ran in
// the snapshot's own namespace has succeeded, and deletes any such pod: a
// finished one has done its work, and an unfinished one would remove the
// directory alongside its replacement. A namespace stuck in Terminating by
// such a pod's refused creation has none, and cleans up through the new pod.
// Only a pod this controller made for this snapshot counts.
func (r *SwiftSnapshotReconciler) legacyCleanupDone(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) (bool, error) {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: snap.Namespace, Name: legacyCleanupPodName(snap)}, &pod)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !metav1.IsControlledBy(&pod, snap) {
		return false, nil
	}
	if err := r.Delete(ctx, &pod); client.IgnoreNotFound(err) != nil {
		return false, err
	}
	return pod.Status.Phase == corev1.PodSucceeded, nil
}

// deleteOrphanCleanupPods deletes the cleanup pods of snapshots in namespace
// that no longer exist. Nothing else would: a cleanup pod has no owner
// reference. One is left when a snapshot's finalizer is removed by hand while
// its pod is pending, running or failed. Matched by UID, not name, so a
// snapshot recreated under the same name keeps its own pod.
func (r *SwiftSnapshotReconciler) deleteOrphanCleanupPods(ctx context.Context, namespace string) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.cleanupPodNamespace(namespace)), client.MatchingLabels{
		cleanupRoleLabel:       cleanupRoleValue,
		snapshotNamespaceLabel: namespace,
	}); err != nil {
		return err
	}
	if len(pods.Items) == 0 {
		return nil
	}
	var snaps snapshotv1alpha1.SwiftSnapshotList
	if err := r.List(ctx, &snaps, client.InNamespace(namespace)); err != nil {
		return err
	}
	live := make(map[string]bool, len(snaps.Items))
	for i := range snaps.Items {
		live[string(snaps.Items[i].UID)] = true
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if live[p.Labels[snapshotUIDLabel]] {
			continue
		}
		if err := r.Delete(ctx, p); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// cleanupCaptureCopy removes the node-local copy an s3/oci capture leaves on
// its capture node (the directory the upload/push Job read from). It holds
// the guest's full RAM image and was never removed: every s3/oci snapshot
// ever taken stayed on its node's disk -- secrets and all -- after the
// snapshot was deleted. Done immediately when there is no node to clean.
func (r *SwiftSnapshotReconciler) cleanupCaptureCopy(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) (bool, error) {
	if snap.Status.NodeName == "" {
		return true, nil
	}
	subdir := pathSubdir(captureDestDir(snap))
	if subdir == "" || subdir == "." || subdir == "/" {
		return true, nil
	}
	return r.cleanupNodeDir(ctx, snap, subdir)
}

// createCleanupPod schedules a one-shot Pod that removes the snapshot
// subdir on the source node. The Pod mounts the parent
// /var/lib/kubeswift/snapshots/ and runs `rm -rf /snapshots/<subdir>/`.
//
// The pod carries no owner reference: one cannot cross namespaces. The
// controller deletes it once it has succeeded (cleanupNodeDir), or once its
// snapshot is gone (deleteOrphanCleanupPods). The labels name the snapshot
// for both, and for an operator looking for the pod of a failed cleanup.
func (r *SwiftSnapshotReconciler) createCleanupPod(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
	key client.ObjectKey,
	subdir string,
) error {
	// We mount the parent dir, not the snapshot dir itself, so that
	// `rm -rf` can act on a subdir from inside the pod's namespace.
	// The hostPath base is HostPathBaseDir (the only prefix the
	// validation webhook permits).
	parent := HostPathBaseDir
	hostPathType := corev1.HostPathDirectoryOrCreate
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				cleanupRoleLabel:       cleanupRoleValue,
				snapshotNameLabel:      names.LabelValue(snap.Name),
				snapshotNamespaceLabel: snap.Namespace,
				snapshotUIDLabel:       string(snap.UID),
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      snap.Status.NodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			// rm needs no API access, so the pod gets no token.
			AutomountServiceAccountToken: ptr.To(false),
			Containers: []corev1.Container{{
				Name:  "rm",
				Image: CleanupImage,
				// No shell: the path is the argv operand to rm, so a subdir
				// carrying a shell metacharacter cannot be interpreted (it was
				// already constrained to a single [A-Za-z0-9._-] segment by
				// ValidateSnapshotDir and pathSubdir). "--" stops rm from
				// reading the path as an option even if it began with '-'.
				Command: []string{"rm", "-rf", "--"},
				Args:    []string{HostPathBaseMount + "/" + subdir},
				// Not privileged: the hostPath mount is all it needs. It runs
				// as root, with the runtime's default capabilities, to remove
				// what the launcher wrote as root.
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
				},
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
	err := r.Create(ctx, pod)
	if apierrors.IsAlreadyExists(err) {
		// Created on an earlier pass the cache has not caught up with.
		return nil
	}
	if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) && r.ControllerNamespace == "" {
		return fmt.Errorf("namespace %s is being deleted and accepts no cleanup pod, and POD_NAMESPACE does not "+
			"name the controller's namespace to run it in: %w", snap.Namespace, err)
	}
	return err
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
// uploaded, no s3 config, the snapshot-s3 image is unconfigured, or the
// namespace is being deleted and refuses the Job) — never wedge namespace
// deletion on a snapshot we cannot clean (the finalizer-trap lesson, Design
// Principle #10).
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
	// The capture node's local copy goes in every case -- it is a cache of
	// the guest's RAM, not the retained artifact.
	if done, err := r.cleanupCaptureCopy(ctx, snap); err != nil || !done {
		return false, err
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
			if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
				return r.skipPurge(ctx, snap, S3ObjectFinalizer, "the objects under "+s3Location(snap))
			}
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

// handleOCIDeletion deletes an oci snapshot's pushed artifact(s) from the
// registry and the capture node's local copy, then removes
// OCIArtifactFinalizer. deletionPolicy: Delete promised this, but the
// controller never purged anything.
//
// A registry that refuses deletes (several do not implement manifest DELETE)
// fails the Job; the finalizer is then dropped with the artifact left in
// place, rather than holding the snapshot -- and its namespace -- in
// Terminating for good. So is a namespace being deleted, which refuses the
// Job.
func (r *SwiftSnapshotReconciler) handleOCIDeletion(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) (bool, error) {
	logger := log.FromContext(ctx)
	if done, err := r.cleanupCaptureCopy(ctx, snap); err != nil || !done {
		return false, err
	}
	refs := ociArtifactRefs(snap)
	if retainArtifacts(snap) || len(refs) == 0 {
		return r.removeNamedFinalizer(ctx, snap, OCIArtifactFinalizer)
	}
	if r.SnapshotORASImage == "" {
		logger.Info("snapshot-oras image not configured; dropping OCI cleanup finalizer without deleting the artifact", "snapshot", snap.Name)
		return r.removeNamedFinalizer(ctx, snap, OCIArtifactFinalizer)
	}

	var job batchv1.Job
	getErr := r.Get(ctx, client.ObjectKey{Name: ociDeleteJobName(snap), Namespace: snap.Namespace}, &job)
	if apierrors.IsNotFound(getErr) {
		j := buildOCIDeleteJob(snap, r.SnapshotORASImage, refs)
		if err := ctrl.SetControllerReference(snap, j, r.Scheme); err != nil {
			return false, err
		}
		if err := r.Create(ctx, j); err != nil && !apierrors.IsAlreadyExists(err) {
			if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
				return r.skipPurge(ctx, snap, OCIArtifactFinalizer, "the artifacts "+ociRefList(refs))
			}
			return false, fmt.Errorf("create oci delete Job: %w", err)
		}
		return false, nil
	}
	if getErr != nil {
		return false, getErr
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			_ = r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground))
			return r.removeNamedFinalizer(ctx, snap, OCIArtifactFinalizer)
		case batchv1.JobFailed:
			logger.Info("could not delete the OCI artifact (the registry may not support deletes); dropping the cleanup finalizer and leaving it in place",
				"snapshot", snap.Name, "artifacts", refs, "reason", c.Message)
			_ = r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground))
			return r.removeNamedFinalizer(ctx, snap, OCIArtifactFinalizer)
		}
	}
	return false, nil // still deleting
}

// skipPurge drops a remote backend's finalizer without purging, because the
// snapshot's namespace is being deleted and refused the purge Job. The Job
// needs the tenant's credentials Secret, which only that namespace holds, so
// it cannot run anywhere else, and holding the finalizer would hold the
// namespace in Terminating for good. What is left is named in a Warning event
// and in the controller's log. The log is the record that lasts: the event is
// new content in the same namespace, which the apiserver refuses too.
func (r *SwiftSnapshotReconciler) skipPurge(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, finalizer, left string) (bool, error) {
	msg := fmt.Sprintf("namespace %s is being deleted and admits no purge Job, so %s may remain; delete them by hand",
		snap.Namespace, left)
	log.FromContext(ctx).Info(msg, "snapshot", snap.Namespace+"/"+snap.Name)
	if r.Recorder != nil {
		r.Recorder.Event(snap, corev1.EventTypeWarning, ReasonPurgeSkipped, msg)
	}
	return r.removeNamedFinalizer(ctx, snap, finalizer)
}

// ociArtifact is one registry artifact to delete.
type ociArtifact struct{ repository, tag string }

// ociRefList renders artifacts as a comma-separated list of references.
func ociRefList(refs []ociArtifact) string {
	out := make([]string, len(refs))
	for i, a := range refs {
		out[i] = a.repository + ":" + a.tag
	}
	return strings.Join(out, ", ")
}

// ociArtifactRefs lists everything the snapshot pushed: the memory artifact
// and, for a full-state capture, its disk and data-disk artifacts.
func ociArtifactRefs(snap *snapshotv1alpha1.SwiftSnapshot) []ociArtifact {
	st := snap.Status.OCI
	if st == nil {
		return nil
	}
	var out []ociArtifact
	add := func(ref string) {
		if a, ok := splitOCIReference(ref); ok {
			out = append(out, a)
		}
	}
	add(st.Reference)
	if st.Disk != nil {
		add(st.Disk.Reference)
	}
	for _, d := range st.DataDisks {
		add(d.Reference)
	}
	return out
}

// splitOCIReference splits "registry/repo:tag" at the tag's colon (the last
// one after the last slash, so a registry port is not mistaken for it).
func splitOCIReference(ref string) (ociArtifact, bool) {
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if ref == "" || colon <= slash || colon == len(ref)-1 {
		return ociArtifact{}, false
	}
	return ociArtifact{repository: ref[:colon], tag: ref[colon+1:]}, true
}

func ociDeleteJobName(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return names.JobName(snap.Name, "-oci-delete")
}

// buildOCIDeleteJob runs snapshot-oras --mode=delete once per artifact (one
// container each; the binary deletes a single tag). Registry credentials come
// from the same dockerconfigjson Secret the push used. Node-agnostic,
// non-root, no host access.
func buildOCIDeleteJob(snap *snapshotv1alpha1.SwiftSnapshot, image string, refs []ociArtifact) *batchv1.Job {
	oci := snap.Spec.Backend.OCI
	var env []corev1.EnvVar
	var mounts []corev1.VolumeMount
	var volumes []corev1.Volume
	if oci != nil && oci.CredentialsSecretRef != nil && oci.CredentialsSecretRef.Name != "" {
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: ociAuthMount})
		mounts = append(mounts, corev1.VolumeMount{Name: "oras-auth", MountPath: ociAuthMount, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "oras-auth",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: oci.CredentialsSecretRef.Name,
				Items:      []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}},
			}},
		})
	}
	containers := make([]corev1.Container, 0, len(refs))
	for i, a := range refs {
		args := []string{"--mode=delete", "--repository=" + a.repository, "--tag=" + a.tag}
		if oci != nil && oci.Insecure {
			args = append(args, "--insecure")
		}
		containers = append(containers, corev1.Container{
			Name:         fmt.Sprintf("delete-%d", i),
			Image:        image,
			Args:         args,
			Env:          env,
			VolumeMounts: mounts,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				RunAsNonRoot:             ptr.To(true),
				RunAsUser:                ptr.To(int64(65534)),
				ReadOnlyRootFilesystem:   ptr.To(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ociDeleteJobName(snap),
			Namespace: snap.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "kubeswift",
				"app.kubernetes.io/component": "snapshot-oci-delete",
				"kubeswift.io/swiftsnapshot":  names.LabelValue(snap.Name),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(s3UploadBackoffLimit),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyOnFailure,
					AutomountServiceAccountToken: ptr.To(false),
					Containers:                   containers,
					Volumes:                      volumes,
				},
			},
		},
	}
}

func hasFinalizer(snap *snapshotv1alpha1.SwiftSnapshot, target string) bool {
	for _, f := range snap.Finalizers {
		if f == target {
			return true
		}
	}
	return false
}

// pathSubdir returns the trailing path component of an absolute hostPath under
// HostPathBaseDir, or "" if the path is not a single safe segment under it —
// the caller treats "" as a refusal-to-act. This is the last check before the
// path reaches the cleanup Pod (ValidateSnapshotDir), so the delete path is
// self-protecting even for an object persisted before the other guards
// existed.
func pathSubdir(hostPath string) string {
	if swiftsnapshotwebhook.ValidateSnapshotDir(hostPath) != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSuffix(hostPath, "/"), HostPathBaseDir)
}
