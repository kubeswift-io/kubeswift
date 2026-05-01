package swiftimage

import (
	"context"
	"fmt"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
)

var swiftImageGVK = schema.GroupVersionKind{
	Group:   imagev1alpha1.GroupName,
	Version: imagev1alpha1.Version,
	Kind:    "SwiftImage",
}

const (
	// CloneSeedSnapshotSuffix is appended to the SwiftImage name to derive
	// the deterministic VolumeSnapshot name used as the clone seed.
	CloneSeedSnapshotSuffix = "-clone-seed"

	// ReasonSnapshotting is reported on the Ready condition while the
	// clone-seed VolumeSnapshot is being created or waiting for readiness.
	ReasonSnapshotting   = "Snapshotting"
	ReasonSnapshotReady  = "SnapshotReady"
	ReasonSnapshotFailed = "SnapshotFailed"

	// AllowVolumeModeChangeAnnotation is the CSI external-snapshotter
	// annotation that permits a clone PVC to use a volumeMode different
	// from the source VolumeSnapshot's volumeMode. Without it, the
	// snapshotter rejects clones that change Filesystem→Block (or vice
	// versa) at PVC provisioning time.
	//
	// W9.x — issue #37. The SwiftImage import PVC is RWO+Filesystem on
	// Longhorn (the default); a SwiftGuest with spec.storage.volumeMode:
	// Block clones from this seed via dataSource: VolumeSnapshot. Without
	// this annotation on the VolumeSnapshotContent, Longhorn's CSI
	// provisioner refuses with "modifies the mode of the source volume
	// but does not have permission to do so."
	//
	// The annotation must be on the VolumeSnapshotContent, NOT on the
	// VolumeSnapshot — the snapshotter's clone-mode check reads the
	// VSC, and there is no automatic propagation from VS to VSC. We
	// patch the VSC after the snapshotter binds it (status
	// .boundVolumeSnapshotContentName is populated). The annotation is
	// no-op when destination volumeMode matches source, so it's safe
	// (and idempotent) to set unconditionally on every cloneSeed VSC.
	AllowVolumeModeChangeAnnotation = "snapshot.storage.kubernetes.io/allow-volume-mode-change"
)

// CloneSeedSnapshotName returns the deterministic VolumeSnapshot name for
// a SwiftImage's clone seed.
func CloneSeedSnapshotName(imageName string) string {
	return imageName + CloneSeedSnapshotSuffix
}

// EnsureCloneSeed reconciles the SwiftImage's clone-seed VolumeSnapshot.
// Returns (ready, sourceSizeBytes, err):
//   - ready=true when the snapshot exists, readyToUse=true; sourceSizeBytes
//     reflects the source PVC's storage request (this is the size the clone
//     PVC must request — Longhorn refuses different-size dataSource clones).
//   - ready=false with err=nil means the snapshot is being created or is
//     not yet ready; caller should requeue.
//   - err non-nil for hard failures (missing source PVC, missing
//     volumeSnapshotClassName, etc.).
//
// Same-namespace constraint: the snapshot is created in the SwiftImage's
// own namespace, never elsewhere — see Phase 0 spike finding §6a.
func (r *SwiftImageReconciler) EnsureCloneSeed(ctx context.Context, img *imagev1alpha1.SwiftImage) (bool, int64, error) {
	if img.Spec.CloneStrategy != imagev1alpha1.CloneStrategySnapshot {
		return false, 0, fmt.Errorf("EnsureCloneSeed called for non-snapshot strategy %q", img.Spec.CloneStrategy)
	}
	if img.Spec.VolumeSnapshotClassName == "" {
		return false, 0, fmt.Errorf("spec.volumeSnapshotClassName is required when cloneStrategy=snapshot")
	}
	if img.Status.PreparedArtifact == nil || img.Status.PreparedArtifact.PVCRef == nil {
		return false, 0, fmt.Errorf("preparedArtifact.pvcRef not yet set; cannot snapshot")
	}

	// Resolve source PVC size — this is the size the snapshot will record
	// as restoreSize and the size every clone PVC must request.
	srcRef := img.Status.PreparedArtifact.PVCRef
	srcNS := srcRef.Namespace
	if srcNS == "" {
		srcNS = img.Namespace
	}
	if srcNS != img.Namespace {
		// Defensive: never let the source PVC live outside the SwiftImage's
		// namespace. Phase 0 §6a — cross-namespace dataSource silently fails.
		return false, 0, fmt.Errorf("prepared PVC must be in same namespace as SwiftImage: pvc=%s/%s image=%s", srcNS, srcRef.Name, img.Namespace)
	}
	var srcPVC corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: srcRef.Name, Namespace: srcNS}, &srcPVC); err != nil {
		return false, 0, fmt.Errorf("get source PVC %s/%s: %w", srcNS, srcRef.Name, err)
	}
	sourceSize, ok := srcPVC.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return false, 0, fmt.Errorf("source PVC %s/%s has no storage request", srcNS, srcRef.Name)
	}
	sourceSizeBytes := sourceSize.Value()

	// Look up (or create) the seed VolumeSnapshot.
	snapName := CloneSeedSnapshotName(img.Name)
	var snap volumesnapshotv1.VolumeSnapshot
	err := r.Get(ctx, client.ObjectKey{Name: snapName, Namespace: img.Namespace}, &snap)
	switch {
	case errors.IsNotFound(err):
		if createErr := r.createCloneSeedSnapshot(ctx, img, snapName); createErr != nil {
			return false, sourceSizeBytes, fmt.Errorf("create clone-seed snapshot: %w", createErr)
		}
		return false, sourceSizeBytes, nil
	case err != nil:
		return false, sourceSizeBytes, fmt.Errorf("get clone-seed snapshot: %w", err)
	}

	// Snapshot exists. Ensure the allow-volume-mode-change annotation
	// is set on the bound VolumeSnapshotContent before the snapshot is
	// used as a dataSource for any clone PVC (W9.x / issue #37).
	//
	// Sequence: snapshotter binds VSC first (status.boundVolumeSnapshotContentName
	// populated), then drives ReadyToUse=true. We patch in between so
	// the annotation is in place by the time any SwiftGuest's clone PVC
	// references the snapshot. If the binding hasn't happened yet,
	// this is a no-op and we requeue with the readyToUse check below.
	if err := r.ensureAllowVolumeModeChange(ctx, &snap); err != nil {
		return false, sourceSizeBytes, fmt.Errorf("ensure allow-volume-mode-change annotation: %w", err)
	}

	// Snapshot exists. Check readiness.
	if snap.Status == nil || snap.Status.ReadyToUse == nil || !*snap.Status.ReadyToUse {
		return false, sourceSizeBytes, nil
	}
	return true, sourceSizeBytes, nil
}

// ensureAllowVolumeModeChange patches the VolumeSnapshotContent bound to
// the given VolumeSnapshot with
// snapshot.storage.kubernetes.io/allow-volume-mode-change: "true" so that
// clone PVCs may differ in volumeMode from the source. No-op when the
// VSC has not been bound yet (snapshotter hasn't created it) or when the
// annotation already carries the expected value.
//
// Idempotent: returns nil whenever the annotation is in the desired
// state, including the not-yet-bound case (caller's outer loop requeues
// via the ReadyToUse check).
//
// Permissions required (controller-manager ClusterRole):
//   - snapshot.storage.k8s.io/volumesnapshotcontents: get, patch
//
// Without these the patch fails with 403 Forbidden; the controller
// surfaces the error and the next reconcile retries. See PR #35
// walkthrough W11 finding for the failure mode this prevents.
func (r *SwiftImageReconciler) ensureAllowVolumeModeChange(ctx context.Context, snap *volumesnapshotv1.VolumeSnapshot) error {
	if snap.Status == nil || snap.Status.BoundVolumeSnapshotContentName == nil {
		// Snapshotter has not created the VSC yet; the next reconcile
		// loop will re-check.
		return nil
	}
	vscName := *snap.Status.BoundVolumeSnapshotContentName
	if vscName == "" {
		return nil
	}
	var vsc volumesnapshotv1.VolumeSnapshotContent
	if err := r.Get(ctx, client.ObjectKey{Name: vscName}, &vsc); err != nil {
		if errors.IsNotFound(err) {
			// VSC referenced by status but not yet visible to the cache;
			// retry on next reconcile.
			return nil
		}
		return fmt.Errorf("get VolumeSnapshotContent %q: %w", vscName, err)
	}
	if vsc.Annotations[AllowVolumeModeChangeAnnotation] == "true" {
		return nil
	}
	patch := client.MergeFrom(vsc.DeepCopy())
	if vsc.Annotations == nil {
		vsc.Annotations = map[string]string{}
	}
	vsc.Annotations[AllowVolumeModeChangeAnnotation] = "true"
	if err := r.Patch(ctx, &vsc, patch); err != nil {
		return fmt.Errorf("patch VolumeSnapshotContent %q with %s=true: %w", vscName, AllowVolumeModeChangeAnnotation, err)
	}
	return nil
}

// createCloneSeedSnapshot creates a VolumeSnapshot of the SwiftImage's
// prepared PVC. The snapshot lives in the SwiftImage's namespace and is
// owned by the SwiftImage so it is garbage-collected when the SwiftImage
// is deleted (after the clone-seed-protected finalizer is removed by the
// reconciler's deletion path — see commit 6).
func (r *SwiftImageReconciler) createCloneSeedSnapshot(ctx context.Context, img *imagev1alpha1.SwiftImage, name string) error {
	srcPVCName := img.Status.PreparedArtifact.PVCRef.Name
	className := img.Spec.VolumeSnapshotClassName
	snap := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: img.Namespace,
			Labels: map[string]string{
				"image.kubeswift.io/swift-image": img.Name,
				"image.kubeswift.io/role":        "clone-seed",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(img, swiftImageGVK),
			},
		},
		Spec: volumesnapshotv1.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &className,
			Source: volumesnapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &srcPVCName,
			},
		},
	}
	if err := r.Create(ctx, snap); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
