package swiftrestore

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ReasonPVCProvisioningFailed: the PVC a restore seeds from a VolumeSnapshot
// is not Bound and its provisioner reports why (for example, the snapshot is
// gone from the storage although its VolumeSnapshot still says readyToUse).
// The restore keeps waiting: a provisioner retries, and some causes clear.
const ReasonPVCProvisioningFailed = "PVCProvisioningFailed"

// provisioningErrorMaxLen bounds the quoted provisioner message; CSI drivers
// can return kilobytes.
const provisioningErrorMaxLen = 300

// pvcProvisioningError returns the message of pvc's latest ProvisioningFailed
// Warning event, or "". Events are listed through the uncached APIReader and
// only for this PVC, so no informer holds every Event in the cluster; without
// an APIReader, or when the list fails, it reports nothing.
func (r *SwiftRestoreReconciler) pvcProvisioningError(ctx context.Context, pvc *corev1.PersistentVolumeClaim) string {
	if r.APIReader == nil {
		return ""
	}
	var events corev1.EventList
	if err := r.APIReader.List(ctx, &events,
		client.InNamespace(pvc.Namespace),
		client.MatchingFields{"involvedObject.name": pvc.Name},
	); err != nil {
		log.FromContext(ctx).Info("could not list the PVC's events", "pvc", pvc.Name, "error", err.Error())
		return ""
	}
	var latest *corev1.Event
	var latestAt time.Time
	for i := range events.Items {
		ev := &events.Items[i]
		if ev.Type != corev1.EventTypeWarning || ev.Reason != "ProvisioningFailed" ||
			ev.InvolvedObject.Kind != "PersistentVolumeClaim" ||
			(pvc.UID != "" && ev.InvolvedObject.UID != pvc.UID) {
			continue
		}
		if at := eventSeen(ev); latest == nil || at.After(latestAt) {
			latest, latestAt = ev, at
		}
	}
	if latest == nil {
		return ""
	}
	msg := latest.Message
	if len(msg) > provisioningErrorMaxLen {
		msg = msg[:provisioningErrorMaxLen] + "..."
	}
	return msg
}

func eventSeen(ev *corev1.Event) time.Time {
	t := ev.LastTimestamp.Time
	if ev.EventTime.After(t) {
		t = ev.EventTime.Time
	}
	if ev.Series != nil && ev.Series.LastObservedTime.After(t) {
		t = ev.Series.LastObservedTime.Time
	}
	return t
}

// notBoundCondition says why a restore waits for a PVC to bind: the
// provisioner's error when it reports one.
func (r *SwiftRestoreReconciler) notBoundCondition(ctx context.Context, pvc *corev1.PersistentVolumeClaim, what string) (string, string) {
	msg := what + " " + pvc.Name + " not yet Bound (phase=" + string(pvc.Status.Phase) + ")"
	if perr := r.pvcProvisioningError(ctx, pvc); perr != "" {
		return ReasonPVCProvisioningFailed, msg + "; the provisioner reports: " + perr
	}
	return ReasonRestoring, msg
}
