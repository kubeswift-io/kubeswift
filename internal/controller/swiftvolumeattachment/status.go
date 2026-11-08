package swiftvolumeattachment

import (
	"context"
	"encoding/json"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

const forcedDetachMessage = "detaching without asking the guest (" + swiftv1alpha1.AnnotationForceDetach + "=true)"

// setPhase sets the phase and the Ready condition (True only when Attached).
func setPhase(sva *swiftv1alpha1.SwiftVolumeAttachment, phase swiftv1alpha1.VolumeAttachmentPhase, reason, msg string) {
	sva.Status.Phase = phase
	status := metav1.ConditionFalse
	if phase == swiftv1alpha1.VolumeAttachmentAttached {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&sva.Status.Conditions, metav1.Condition{
		Type:               swiftv1alpha1.VolumeAttachmentConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: sva.Generation,
	})
}

func setInUse(sva *swiftv1alpha1.SwiftVolumeAttachment, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&sva.Status.Conditions, metav1.Condition{
		Type:               swiftv1alpha1.VolumeAttachmentConditionInUse,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: sva.Generation,
	})
}

func readyMessage(sva *swiftv1alpha1.SwiftVolumeAttachment) string {
	if c := apimeta.FindStatusCondition(sva.Status.Conditions, swiftv1alpha1.VolumeAttachmentConditionReady); c != nil {
		return c.Message
	}
	return ""
}

// patchFinalizer adds or removes the detach finalizer with a metadata patch
// carrying the resourceVersion, so it never overwrites another writer's
// finalizers (#806).
func patchFinalizer(ctx context.Context, c client.Client, sva *swiftv1alpha1.SwiftVolumeAttachment, add bool) error {
	orig := sva.DeepCopy()
	if add {
		controllerutil.AddFinalizer(sva, swiftv1alpha1.VolumeDetachFinalizer)
	} else if !controllerutil.RemoveFinalizer(sva, swiftv1alpha1.VolumeDetachFinalizer) {
		return nil
	}
	return client.IgnoreNotFound(c.Patch(ctx, sva, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})))
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
