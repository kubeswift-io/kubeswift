package swiftimage

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
)

func jobWith(name string, failed, succeeded int32, conds ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     batchv1.JobStatus{Failed: failed, Succeeded: succeeded, Conditions: conds},
	}
}

var backoffLimitExceeded = batchv1.JobCondition{
	Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
	Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
}

// A failed import pod is retried by its Job, so the image keeps importing:
// Failed is final for a SwiftImage, and a later pod may succeed. Only the Job
// failing fails the image.
func TestCheckImportStatus_FailsOnlyWhenTheJobDoes(t *testing.T) {
	img := &imagev1alpha1.SwiftImage{ObjectMeta: metav1.ObjectMeta{Name: "noble", Namespace: "default"}}
	name := names.JobName(importJobNamePrefix+img.Name, "")
	cases := []struct {
		name    string
		job     *batchv1.Job
		want    imagev1alpha1.SwiftImagePhase
		wantMsg string
	}{
		{"a pod failed, the Job retries", jobWith(name, 1, 0), imagev1alpha1.SwiftImagePhaseImporting, ""},
		{"a failure target, pods still terminating", jobWith(name, 6, 0, batchv1.JobCondition{
			Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue,
		}), imagev1alpha1.SwiftImagePhaseImporting, ""},
		{"a retry succeeded", jobWith(name, 2, 1), imagev1alpha1.SwiftImagePhaseValidating, ""},
		{"the Job failed", jobWith(name, 7, 0, backoffLimitExceeded),
			imagev1alpha1.SwiftImagePhaseFailed, "Job has reached the specified backoff limit"},
		{"the Job failed without a message", jobWith(name, 7, 0, batchv1.JobCondition{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		}), imagev1alpha1.SwiftImagePhaseFailed, "import job failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := testScheme()
			r := &SwiftImageReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.job).Build(), Scheme: scheme}
			phase, _, msg, err := r.CheckImportStatus(context.Background(), img)
			if err != nil {
				t.Fatal(err)
			}
			if phase != tc.want || msg != tc.wantMsg {
				t.Errorf("got (%s, %q), want (%s, %q)", phase, msg, tc.want, tc.wantMsg)
			}
		})
	}
}

// The size measurement follows the same rule: a failed pod is still measuring.
func TestValidate_MeasurementFailsOnlyWhenTheJobDoes(t *testing.T) {
	img := &imagev1alpha1.SwiftImage{ObjectMeta: metav1.ObjectMeta{Name: "noble", Namespace: "default"}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: importPVCNamePrefix + img.Name, Namespace: "default"}}
	name := names.JobName(measureJobNamePrefix+img.Name, "")
	for _, tc := range []struct {
		name string
		job  *batchv1.Job
		want string
	}{
		{"a pod failed, the Job retries", jobWith(name, 1, 0), "measuring"},
		{"the Job failed", jobWith(name, 7, 0, backoffLimitExceeded), "size measurement failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := testScheme()
			r := &SwiftImageReconciler{
				Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, tc.job).Build(),
				Scheme:    scheme,
				Clientset: k8sfake.NewSimpleClientset(),
			}
			res, err := r.Validate(context.Background(), img, "")
			if err != nil {
				t.Fatal(err)
			}
			if res.OK || res.Error != tc.want {
				t.Errorf("got %+v, want error %q", res, tc.want)
			}
		})
	}
}
