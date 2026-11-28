package swiftimagepublish

import (
	"context"
	"fmt"
	"strings"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
	"github.com/kubeswift-io/kubeswift/internal/rootdisk"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
	"github.com/kubeswift-io/kubeswift/internal/storagelocation"
)

// Reasons a request waits on, or fails over, its source.
const (
	ReasonSourceNotFound       = "SourceNotFound"
	ReasonSourceNotReady       = "SourceNotReady"
	ReasonSourceUnsupported    = "SourceUnsupported"
	ReasonStorageClassRequired = "StorageClassRequired"
)

// sourceResult is a source the controller has looked at: resolved (status
// to record), still waiting (wait set), or unusable (fail set).
type sourceResult struct {
	status *imagev1alpha1.ImagePublishSourceStatus
	// storageClass and size shape the temporary PVC (snapshot source).
	storageClass string
	wait         *condition
	fail         *condition
}

type condition struct {
	reason, message string
}

// diskPVCName is the temporary PVC a snapshot source is restored into.
func diskPVCName(pub *imagev1alpha1.SwiftImagePublish) string {
	return names.Bounded(pub.Name, "-disk", 253)
}

// resolveSnapshotSource looks at a csi-volume-snapshot SwiftSnapshot: its
// root disk's VolumeSnapshot, that snapshot's size and volume mode, the
// class to restore it under, and the osType to record.
func resolveSnapshotSource(ctx context.Context, c client.Reader, pub *imagev1alpha1.SwiftImagePublish) (sourceResult, error) {
	name := pub.Spec.Source.SnapshotRef.Name
	var snap snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(ctx, client.ObjectKey{Namespace: pub.Namespace, Name: name}, &snap); err != nil {
		if apierrors.IsNotFound(err) {
			return sourceResult{wait: &condition{ReasonSourceNotFound, "SwiftSnapshot " + name + " does not exist"}}, nil
		}
		return sourceResult{}, err
	}
	switch snap.Spec.Backend.Type {
	case snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot, snapshotv1alpha1.SnapshotBackendOCI:
	default:
		return sourceResult{fail: &condition{ReasonSourceUnsupported, fmt.Sprintf(
			"SwiftSnapshot %s has backend %s; only a csi-volume-snapshot snapshot or a full-state oci snapshot can be published", name, snap.Spec.Backend.Type)}}, nil
	}
	switch snap.Status.Phase {
	case snapshotv1alpha1.SwiftSnapshotPhaseReady:
	case snapshotv1alpha1.SwiftSnapshotPhaseFailed:
		return sourceResult{fail: &condition{ReasonSourceUnsupported, "SwiftSnapshot " + name + " failed"}}, nil
	default:
		return sourceResult{wait: &condition{ReasonSourceNotReady, "SwiftSnapshot " + name + " is not Ready yet"}}, nil
	}
	if snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendOCI {
		return resolveOCISnapshotSource(ctx, c, pub, &snap)
	}
	var root *snapshotv1alpha1.SnapshotDiskRef
	for i := range snap.Status.Disks {
		if snap.Status.Disks[i].Role == "root" {
			root = &snap.Status.Disks[i]
		}
	}
	if root == nil {
		return sourceResult{fail: &condition{ReasonSourceUnsupported, "SwiftSnapshot " + name + " records no root disk"}}, nil
	}
	vsNS, vsName, ok := strings.Cut(root.Handle, "/")
	if !ok || vsNS != pub.Namespace || vsName == "" {
		return sourceResult{fail: &condition{ReasonSourceUnsupported, fmt.Sprintf(
			"SwiftSnapshot %s's root disk handle %q is not a VolumeSnapshot in namespace %s", name, root.Handle, pub.Namespace)}}, nil
	}
	var vs volumesnapshotv1.VolumeSnapshot
	if err := c.Get(ctx, client.ObjectKey{Namespace: vsNS, Name: vsName}, &vs); err != nil {
		if apierrors.IsNotFound(err) {
			return sourceResult{fail: &condition{ReasonSourceUnsupported, "VolumeSnapshot " + vsName + " of SwiftSnapshot " + name + " does not exist"}}, nil
		}
		return sourceResult{}, err
	}
	if vs.Status == nil || vs.Status.ReadyToUse == nil || !*vs.Status.ReadyToUse {
		return sourceResult{wait: &condition{ReasonSourceNotReady, "VolumeSnapshot " + vsName + " is not ready to use"}}, nil
	}
	size := root.SizeBytes
	if vs.Status.RestoreSize != nil && vs.Status.RestoreSize.Value() > size {
		size = vs.Status.RestoreSize.Value()
	}
	if size <= 0 {
		return sourceResult{fail: &condition{ReasonSourceUnsupported, "VolumeSnapshot " + vsName + " reports no restore size"}}, nil
	}

	// The restored volume keeps the snapshotted volume's mode: the content
	// records it, else the source guest's root PVC does.
	var mode corev1.PersistentVolumeMode
	if vs.Status.BoundVolumeSnapshotContentName != nil {
		var content volumesnapshotv1.VolumeSnapshotContent
		err := c.Get(ctx, client.ObjectKey{Name: *vs.Status.BoundVolumeSnapshotContentName}, &content)
		if err != nil && !apierrors.IsNotFound(err) {
			return sourceResult{}, err
		}
		if err == nil && content.Spec.SourceVolumeMode != nil {
			mode = *content.Spec.SourceVolumeMode
		}
	}

	// The source guest, while it exists, gives the class, the mode and the
	// osType. It is refused when it boots from a kernel: its root disk is
	// not a bootable image.
	var guest swiftv1alpha1.SwiftGuest
	guestErr := c.Get(ctx, client.ObjectKey{Namespace: pub.Namespace, Name: snap.Spec.GuestRef.Name}, &guest)
	if guestErr != nil && !apierrors.IsNotFound(guestErr) {
		return sourceResult{}, guestErr
	}
	haveGuest := guestErr == nil
	if haveGuest && guest.Spec.KernelRef != nil {
		return sourceResult{fail: &condition{ReasonSourceUnsupported, "SwiftGuest " + guest.Name +
			" boots from a kernel (spec.kernelRef); only a disk-boot guest's root disk can be published"}}, nil
	}
	class := pub.Spec.StorageClassName
	if haveGuest && (class == "" || mode == "") {
		if claim, _, err := rootdisk.Claim(ctx, c, &guest); err == nil {
			var pvc corev1.PersistentVolumeClaim
			if err := c.Get(ctx, client.ObjectKey{Namespace: pub.Namespace, Name: claim}, &pvc); err == nil {
				if class == "" && pvc.Spec.StorageClassName != nil {
					class = *pvc.Spec.StorageClassName
				}
				if mode == "" && pvc.Spec.VolumeMode != nil {
					mode = *pvc.Spec.VolumeMode
				}
			} else if !apierrors.IsNotFound(err) {
				return sourceResult{}, err
			}
		} else {
			return sourceResult{}, err
		}
	}
	if class == "" {
		return sourceResult{wait: &condition{ReasonStorageClassRequired, "cannot tell the storage class to restore VolumeSnapshot " + vsName +
			" under (the source guest's root disk is gone): set spec.storageClassName"}}, nil
	}
	if mode == "" {
		mode = corev1.PersistentVolumeFilesystem
	}
	osType := pub.Spec.OSType
	if osType == "" && haveGuest && guest.Spec.ImageRef != nil {
		var img imagev1alpha1.SwiftImage
		err := c.Get(ctx, client.ObjectKey{Namespace: pub.Namespace, Name: guest.Spec.ImageRef.Name}, &img)
		if err != nil && !apierrors.IsNotFound(err) {
			return sourceResult{}, err
		}
		osType = img.Spec.OSType
	}
	if osType == "" {
		osType = imagev1alpha1.OSTypeLinux
	}
	return sourceResult{
		status: &imagev1alpha1.ImagePublishSourceStatus{
			Kind:           "SwiftSnapshot",
			Name:           snap.Name,
			UID:            string(snap.UID),
			VolumeSnapshot: vsName,
			PVCName:        diskPVCName(pub),
			Block:          mode == corev1.PersistentVolumeBlock,
			SizeBytes:      size,
			OSType:         osType,
		},
		storageClass: class,
	}, nil
}

// diskPVC is the temporary PVC a snapshot source is restored into, owned by
// the request so it goes with it at the latest.
func diskPVC(pub *imagev1alpha1.SwiftImagePublish, src *imagev1alpha1.ImagePublishSourceStatus, class string) *corev1.PersistentVolumeClaim {
	mode := corev1.PersistentVolumeFilesystem
	if src.Block {
		mode = corev1.PersistentVolumeBlock
	}
	apiGroup := volumesnapshotv1.GroupName
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      src.PVCName,
			Namespace: pub.Namespace,
			Labels:    labels(pub),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:       &mode,
			StorageClassName: ptr.To(class),
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     src.VolumeSnapshot,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(src.SizeBytes, resource.BinarySI)},
			},
		},
	}
}

// resolveOCISnapshotSource takes a full-state oci snapshot's disk artifact,
// already a golden image, as the source of a registry-side copy. It is
// read with the snapshot's recorded connection: its CA, its credentials
// Secret (in this namespace) and its verify key.
func resolveOCISnapshotSource(ctx context.Context, c client.Reader, pub *imagev1alpha1.SwiftImagePublish, snap *snapshotv1alpha1.SwiftSnapshot) (sourceResult, error) {
	if snap.Status.OCI == nil || snap.Status.OCI.Disk == nil || snap.Status.OCI.Disk.ManifestDigest == "" {
		return sourceResult{fail: &condition{ReasonSourceUnsupported, "SwiftSnapshot " + snap.Name +
			" has no disk artifact (a memory-only snapshot); take one with includeDisk: true, or use a csi-volume-snapshot snapshot"}}, nil
	}
	conn, ok := clonecommon.SnapshotOCI(snap)
	if !ok {
		return sourceResult{fail: &condition{ReasonSourceUnsupported, "SwiftSnapshot " + snap.Name + " records no registry"}}, nil
	}
	if conn.CredentialsSecretName != "" {
		w, err := storagelocation.CheckSecrets(ctx, c, pub.Namespace, conn.CredentialsSecretName, "")
		if err != nil {
			return sourceResult{}, err
		}
		if w != nil {
			return sourceResult{wait: &condition{w.Reason, "source " + w.Message}}, nil
		}
	}
	osType := pub.Spec.OSType
	if osType == "" && snap.Status.GuestSpec != nil && snap.Status.GuestSpec.OSType != "" {
		osType = imagev1alpha1.OSType(snap.Status.GuestSpec.OSType)
	}
	if osType == "" {
		osType = imagev1alpha1.OSTypeLinux
	}
	return sourceResult{status: &imagev1alpha1.ImagePublishSourceStatus{
		Kind:      "SwiftSnapshot",
		Name:      snap.Name,
		UID:       string(snap.UID),
		SizeBytes: snap.Status.OCI.Disk.PushedBytes,
		OSType:    osType,
		CopyFrom: &imagev1alpha1.ImagePublishCopySource{
			Repository:            conn.Repository,
			Digest:                snap.Status.OCI.Disk.ManifestDigest,
			Insecure:              conn.Insecure,
			CABundle:              conn.CABundle,
			CredentialsSecretName: conn.CredentialsSecretName,
			VerifyKey:             conn.VerifyKey,
		},
	}}, nil
}
