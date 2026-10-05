package swiftmigration

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
)

func attachment(name, pv, node string) *storagev1.VolumeAttachment {
	return &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1.VolumeAttachmentSpec{
			Attacher: "driver.longhorn.io",
			NodeName: node,
			Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pv},
		},
		Status: storagev1.VolumeAttachmentStatus{Attached: true},
	}
}

// liveValidating is a live migration in Validating of a guest whose source pod
// (on worker-1) uses PVC root, bound to PV pv-root, with the given attachments.
func liveValidating(t *testing.T, objs ...client.Object) (*SwiftMigrationReconciler, *migrationv1alpha1.SwiftMigration) {
	t.Helper()
	scheme := validatingScheme(t)
	srcPod := newSourcePod("guest", "default", "src-pod-uid-1")
	srcPod.Spec.Volumes = []corev1.Volume{{Name: "root", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "root"}}}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "root", Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-root"}}
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * time.Minute}
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating
	all := append([]client.Object{mig, newGuestForValidating("guest", "default", "class-default"),
		newGuestClass("class-default", 2, 2048), newSpaciousNode("worker-2", 8, 65536), srcPod, pvc}, objs...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(all...).WithStatusSubresource(mig).Build()
	return &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}, mig
}

// A failed live attempt can leave its target attached for minutes after the
// destination pod is gone (#692). The next migration waits for it, saying
// which volume and where, instead of starting an attach that would stall.
func TestValidatingLive_WaitsForAVolumeAttachedToAnotherNode(t *testing.T) {
	r, mig := liveValidating(t, attachment("csi-src", "pv-root", "worker-1"), attachment("csi-stale", "pv-root", "worker-2"))
	status := mig.Status.DeepCopy()
	res := r.handleValidatingLive(context.Background(), mig, status)
	if res.FailureMsg != "" || res.Advanced || res.Requeue != volumeDetachPollInterval {
		t.Fatalf("want a requeue in Validating; got %+v", res)
	}
	c := apimeta.FindStatusCondition(status.Conditions, migrationv1alpha1.SwiftMigrationConditionCompatible)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != ReasonAwaitingVolumeDetach ||
		!strings.Contains(c.Message, `PVC "root" on node worker-2 (VolumeAttachment csi-stale)`) {
		t.Errorf("Compatible = %+v", c)
	}
	if status.PhaseDetail != phaseDetailAwaitingVolumeDetach {
		t.Errorf("phaseDetail = %q", status.PhaseDetail)
	}
}

// Attachments on the source's own node, and other volumes' attachments
// elsewhere, are not a reason to wait.
func TestValidatingLive_OwnNodeAndOtherVolumesDoNotWait(t *testing.T) {
	r, mig := liveValidating(t, attachment("csi-src", "pv-root", "worker-1"), attachment("csi-other", "pv-unrelated", "worker-2"))
	status := mig.Status.DeepCopy()
	if res := r.handleValidatingLive(context.Background(), mig, status); !res.Advanced {
		t.Fatalf("want Preparing; got %+v (%v)", res, status.Conditions)
	}
}

// The wait is bounded: an attachment still there after volumeDetachWait is
// stuck, and the migration fails, naming it.
func TestValidatingLive_VolumeDetachWaitRunsOut(t *testing.T) {
	r, mig := liveValidating(t, attachment("csi-stale", "pv-root", "worker-2"))
	status := mig.Status.DeepCopy()
	status.Conditions = []metav1.Condition{{Type: migrationv1alpha1.SwiftMigrationConditionCompatible, Status: metav1.ConditionUnknown,
		Reason: ReasonAwaitingVolumeDetach, LastTransitionTime: metav1.NewTime(time.Now().Add(-volumeDetachWait - time.Second))}}
	res := r.handleValidatingLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonOther || !strings.Contains(res.FailureMsg, "csi-stale") ||
		!strings.Contains(res.FailureMsg, "kubectl get volumeattachments") {
		t.Fatalf("got %q / %q", res.FailureReason, res.FailureMsg)
	}
	if c := apimeta.FindStatusCondition(status.Conditions, migrationv1alpha1.SwiftMigrationConditionCompatible); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("a failed migration must not keep Compatible Unknown: %+v", c)
	}
}

// Once the attachment is gone the wait's condition goes too, so a later wait
// in Validating (pods terminating on the target) starts its own clock rather
// than inheriting this one's: both keep Compatible Unknown.
func TestAwaitVolumeDetach_ClearedWaitRemovesItsCondition(t *testing.T) {
	r, _ := liveValidating(t, attachment("csi-src", "pv-root", "worker-1"))
	var src corev1.Pod
	if err := r.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &src); err != nil {
		t.Fatal(err)
	}
	status := &migrationv1alpha1.SwiftMigrationStatus{Conditions: []metav1.Condition{{Type: migrationv1alpha1.SwiftMigrationConditionCompatible,
		Status: metav1.ConditionUnknown, Reason: ReasonAwaitingVolumeDetach, LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute))}}}
	if res := r.awaitVolumeDetach(context.Background(), status, &src); res != nil {
		t.Fatalf("want no wait; got %+v", res)
	}
	if c := apimeta.FindStatusCondition(status.Conditions, migrationv1alpha1.SwiftMigrationConditionCompatible); c != nil {
		t.Errorf("the finished wait's condition was left: %+v", c)
	}
	// Another wait's condition is not this gate's to remove.
	status.Conditions = []metav1.Condition{{Type: migrationv1alpha1.SwiftMigrationConditionCompatible,
		Status: metav1.ConditionUnknown, Reason: ReasonAwaitingTerminatingPods, LastTransitionTime: metav1.Now()}}
	_ = r.awaitVolumeDetach(context.Background(), status, &src)
	if c := apimeta.FindStatusCondition(status.Conditions, migrationv1alpha1.SwiftMigrationConditionCompatible); c == nil || c.Reason != ReasonAwaitingTerminatingPods {
		t.Errorf("removed another wait's condition: %+v", c)
	}
}
