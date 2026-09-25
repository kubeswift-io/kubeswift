package swiftkernel

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
)

// A failed pull pod is retried by its Job, so the node keeps pulling: Failed
// is final for a SwiftKernel, and one registry blip on one node would
// otherwise fail the kernel for every node.
func TestCheckNodePullStatus_FailsOnlyWhenTheJobDoes(t *testing.T) {
	sk := &kernelv1alpha1.SwiftKernel{ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: "default"}}
	name := pullJobName(sk.Name, "node-a", kernelv1alpha1.KernelLocalPath(sk.Namespace, sk.Name))
	job := func(failed, succeeded int32, conds ...batchv1.JobCondition) *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Status:     batchv1.JobStatus{Failed: failed, Succeeded: succeeded, Conditions: conds},
		}
	}
	cases := []struct {
		name    string
		job     *batchv1.Job
		want    kernelv1alpha1.SwiftKernelPhase
		wantMsg string
	}{
		{"a pod failed, the Job retries", job(1, 0), kernelv1alpha1.SwiftKernelPhasePulling, ""},
		{"a retry succeeded", job(2, 1), kernelv1alpha1.SwiftKernelPhaseReady, ""},
		{"the Job failed", job(7, 0, batchv1.JobCondition{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
		}), kernelv1alpha1.SwiftKernelPhaseFailed, "Job has reached the specified backoff limit"},
		{"the Job failed without a message", job(7, 0, batchv1.JobCondition{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		}), kernelv1alpha1.SwiftKernelPhaseFailed, "pull job failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			r := &SwiftKernelReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.job).Build(), Scheme: scheme}
			phase, msg, err := r.CheckNodePullStatus(context.Background(), sk, "node-a")
			if err != nil {
				t.Fatal(err)
			}
			if phase != tc.want || msg != tc.wantMsg {
				t.Errorf("got (%s, %q), want (%s, %q)", phase, msg, tc.want, tc.wantMsg)
			}
		})
	}
}
