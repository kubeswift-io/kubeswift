// Storage location resolution for SwiftSnapshot.
//
// An oci snapshot that does not name its registry in spec.backend.oci, and a
// csi-volume-snapshot snapshot that does not name its VolumeSnapshotClass,
// take them from a storage location. Either way the result is resolved once,
// before the guest is touched, and recorded in status.location; everything
// after (the push, the disk chunking, restores, clones, the deletion) reads
// that record and never the spec or a location again.
package swiftsnapshot

import (
	"context"
	"fmt"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
	"github.com/kubeswift-io/kubeswift/internal/storagelocation"
)

// ReasonNoVolumeSnapshotClass: a csi-volume-snapshot snapshot names no class,
// no location gives one, and the cluster has no default class.
const ReasonNoVolumeSnapshotClass = "NoVolumeSnapshotClass"

// isDefaultClassAnnotation marks the cluster's default VolumeSnapshotClass
// (one per CSI driver).
const isDefaultClassAnnotation = "snapshot.storage.kubernetes.io/is-default-class"

// needsLocation reports whether the snapshot's backend records a location.
func needsLocation(snap *snapshotv1alpha1.SwiftSnapshot) bool {
	t := snap.Spec.Backend.Type
	return t == snapshotv1alpha1.SnapshotBackendOCI || t == snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot
}

// resolveLocation works out what status.location records. It returns the
// location, or a wait (the snapshot stays Pending with that reason), or a
// failMsg (terminal).
func (r *SwiftSnapshotReconciler) resolveLocation(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) (*snapshotv1alpha1.SnapshotLocation, *storagelocation.Wait, string, error) {
	if snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot {
		return r.resolveCSILocation(ctx, snap)
	}

	if snap.Spec.Backend.OCI != nil {
		// Used exactly as written; status.location is not recorded yet, so
		// this reads the spec.
		c, _ := clonecommon.SnapshotOCI(snap)
		return &snapshotv1alpha1.SnapshotLocation{
			Source:                snapshotv1alpha1.SnapshotLocationExplicit,
			Repository:            c.Repository,
			Tag:                   c.Tag,
			Insecure:              c.Insecure,
			CredentialsSecretName: c.CredentialsSecretName,
			SigningKeySecretName:  c.SigningKeySecretName,
		}, nil, "", nil
	}

	res, wait, err := storagelocation.Resolve(ctx, r.Client, snap.Namespace, snap.Spec.Backend.LocationRef, storagelocation.NeedOCI)
	if err != nil || wait != nil {
		return nil, wait, "", err
	}
	if res == nil {
		return nil, &storagelocation.Wait{Reason: storagelocation.ReasonNone, Message: fmt.Sprintf(
			"no storage location for this oci snapshot: set spec.backend.oci, name a location in spec.backend.locationRef, "+
				"or create a default SwiftStorageLocation in namespace %s or a default SwiftClusterStorageLocation", snap.Namespace)}, "", nil
	}
	o := res.Spec.OCI
	repo := storagelocation.SnapshotRepository(res, snap.Namespace)
	if _, err := storagelocation.Registry(repo); err != nil {
		return nil, &storagelocation.Wait{Reason: storagelocation.ReasonInvalid, Message: fmt.Sprintf(
			"%s gives the repository %q for this namespace's snapshots, which is not valid: %v", res.Source(), repo, err)}, "", nil
	}
	loc := &snapshotv1alpha1.SnapshotLocation{
		Source:                res.Source(),
		Repository:            repo,
		Tag:                   clonecommon.LocationOCITag(snap),
		Insecure:              o.Insecure,
		CABundle:              o.CABundle,
		CredentialsSecretName: storagelocation.CredentialsSecret(o),
		SigningKeySecretName:  o.SigningKeySecretName,
	}
	wait, err = storagelocation.CheckSecrets(ctx, r.Client, snap.Namespace, loc.CredentialsSecretName, loc.SigningKeySecretName)
	if err != nil || wait != nil {
		return nil, wait, "", err
	}
	return loc, nil, "", nil
}

// resolveCSILocation picks the VolumeSnapshotClass: the snapshot's own, else a
// location's, else the cluster's default class. With none of them the
// external snapshotter would only fail later with "cannot find default
// snapshot class", so the snapshot fails now, saying what to set.
func (r *SwiftSnapshotReconciler) resolveCSILocation(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) (*snapshotv1alpha1.SnapshotLocation, *storagelocation.Wait, string, error) {
	if c := snap.Spec.Backend.CSIVolumeSnapshot; c != nil && c.VolumeSnapshotClassName != "" {
		return &snapshotv1alpha1.SnapshotLocation{Source: snapshotv1alpha1.SnapshotLocationExplicit, VolumeSnapshotClassName: c.VolumeSnapshotClassName}, nil, "", nil
	}
	res, wait, err := storagelocation.Resolve(ctx, r.Client, snap.Namespace, snap.Spec.Backend.LocationRef, storagelocation.NeedCSI)
	if err != nil || wait != nil {
		return nil, wait, "", err
	}
	if res != nil {
		return &snapshotv1alpha1.SnapshotLocation{Source: res.Source(), VolumeSnapshotClassName: res.Spec.CSI.VolumeSnapshotClassName}, nil, "", nil
	}
	// The default class is chosen by the snapshotter per CSI driver, so it is
	// left to it; only the case where there is none at all is caught here.
	if r.VolumeSnapshotEnabled {
		var classes volumesnapshotv1.VolumeSnapshotClassList
		if err := r.List(ctx, &classes); err != nil {
			return nil, nil, "", err
		}
		found := false
		for i := range classes.Items {
			if classes.Items[i].Annotations[isDefaultClassAnnotation] == "true" {
				found = true
				break
			}
		}
		if !found {
			return nil, nil, fmt.Sprintf("no VolumeSnapshotClass to use: set spec.backend.csiVolumeSnapshot.volumeSnapshotClassName, "+
				"give a storage location a csi.volumeSnapshotClassName, or mark a class as the cluster default (annotation %s: \"true\")",
				isDefaultClassAnnotation), nil
		}
	}
	return &snapshotv1alpha1.SnapshotLocation{Source: snapshotv1alpha1.SnapshotLocationDefaultClass}, nil, "", nil
}

// ReasonLocationResolved: status.location was just recorded; the capture
// starts on the next reconcile.
const ReasonLocationResolved = "LocationResolved"

// locationWaitRequeue is how often a snapshot waiting for a location, or for
// a Secret a location names, checks again.
const locationWaitRequeue = 15 * time.Second

// recordLocation resolves status.location once, before anything is captured.
// proceed is true when it is in place. Otherwise handlePending returns
// (advanced, requeue, err): the snapshot waits with a reason, fails, or (oci)
// has just recorded the location and continues on the next reconcile, after
// the status write, so the capture never starts on a location that was not
// recorded. A csi snapshot goes on at once: its next step, creating the
// VolumeSnapshot, already happens on a later reconcile.
func (r *SwiftSnapshotReconciler) recordLocation(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, status *snapshotv1alpha1.SwiftSnapshotStatus) (proceed, advanced bool, requeue time.Duration, err error) {
	if !needsLocation(snap) || status.Location != nil {
		return true, false, 0, nil
	}
	loc, wait, failMsg, err := r.resolveLocation(ctx, snap)
	switch {
	case err != nil:
		return false, false, 0, err
	case failMsg != "":
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonNoVolumeSnapshotClass, failMsg)
		return false, true, 0, nil
	case wait != nil:
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
		setReadyCondition(status, metav1.ConditionFalse, wait.Reason, wait.Message)
		return false, false, locationWaitRequeue, nil
	}
	status.Location = loc
	if snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot {
		return true, false, 0, nil
	}
	setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
	setReadyCondition(status, metav1.ConditionFalse, ReasonLocationResolved, describeLocation(loc))
	return false, false, time.Second, nil
}

func describeLocation(l *snapshotv1alpha1.SnapshotLocation) string {
	if l.Repository != "" {
		return fmt.Sprintf("storing in %s:%s (from %s)", l.Repository, l.Tag, l.Source)
	}
	if l.VolumeSnapshotClassName != "" {
		return fmt.Sprintf("using VolumeSnapshotClass %s (from %s)", l.VolumeSnapshotClassName, l.Source)
	}
	return "using the cluster's default VolumeSnapshotClass"
}
