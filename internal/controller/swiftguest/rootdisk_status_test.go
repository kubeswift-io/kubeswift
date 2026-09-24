package swiftguest

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// A disk-boot guest whose root-disk clone Job has failed for good.
func guestWithFailedCloneJob() (*swiftv1alpha1.SwiftGuest, *corev1.PersistentVolumeClaim, *batchv1.Job) {
	g := asDiskBoot(kernelGuest())
	g.UID = "guest-uid"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: RootDiskCloneName(g.Name), Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(g, swiftGuestGVK)},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: CloneJobName(g.Name), Namespace: "ns"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
		}}},
	}
	return g, pvc, job
}

// A failed clone Job used to leave the guest in Scheduling with nothing saying
// why: EnsureRootDiskClone's error was dropped on requeue.
func TestReconcile_AFailedRootDiskCloneIsReported(t *testing.T) {
	g, pvc, job := guestWithFailedCloneJob()
	c := guestClientBuilder(g, testGuestClass(), readyImage(), preparedPVC(), pvc, job).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseScheduling {
		t.Errorf("phase = %q, want Scheduling", got.Status.Phase)
	}
	cond := guestCondition(t, got, ConditionStorageReady)
	if cond.Status != metav1.ConditionFalse || cond.Reason != reasonRootDiskCloneFailed ||
		!strings.Contains(cond.Message, "backoff limit") {
		t.Fatalf("StorageReady = %+v, want False/%s naming the Job's failure", cond, reasonRootDiskCloneFailed)
	}

	// Nothing changed, so a second pass writes nothing (the pre-flight sets
	// StorageReady=True first in every pass).
	rv := got.ResourceVersion
	again, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if again.ResourceVersion != rv {
		t.Errorf("status rewritten on an unchanged pass (resourceVersion %s -> %s)", rv, again.ResourceVersion)
	}
}

// A clone still in progress says so, as progress rather than failure.
func TestReconcile_ARootDiskCloneInProgressIsReported(t *testing.T) {
	g, pvc, job := guestWithFailedCloneJob()
	job.Status.Conditions = nil
	c := guestClientBuilder(g, testGuestClass(), readyImage(), preparedPVC(), pvc, job).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	cond := guestCondition(t, got, ConditionStorageReady)
	if cond.Status != metav1.ConditionFalse || cond.Reason != reasonRootDiskCloning ||
		!strings.Contains(cond.Message, "in progress") {
		t.Fatalf("StorageReady = %+v, want False/%s", cond, reasonRootDiskCloning)
	}
}

// An unchanged status is not patched. The comparison used to set the stored
// status against a pointer, which never matched, so every reconcile sent a
// patch; with the optimistic lock a stale cache turned it into a conflict.
func TestPatchStatus_UnchangedStatusSendsNothing(t *testing.T) {
	g := kernelGuest()
	g.Status = *ranStatus("pod-1")
	patches := 0
	c := guestClientBuilder(g).WithInterceptorFuncs(interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			patches++
			return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	var stored swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), testGuestKey, &stored); err != nil {
		t.Fatal(err)
	}
	if err := r.patchStatus(context.Background(), &stored, stored.Status.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if patches != 0 {
		t.Errorf("%d status patch(es) sent for an unchanged status", patches)
	}
}
