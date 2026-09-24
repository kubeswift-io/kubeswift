package clonecommon

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func reportJob() *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns", UID: "job-uid"}}
}

// jobPod is a pod of reportJob (controlled by it, Succeeded when terminated).
func jobPod(name, jobName, msg string, terminated bool) *corev1.Pod {
	cs := corev1.ContainerStatus{Name: "x"}
	phase := corev1.PodRunning
	if terminated {
		cs.State.Terminated = &corev1.ContainerStateTerminated{Message: msg}
		phase = corev1.PodSucceeded
	}
	isController := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns", Labels: map[string]string{"job-name": jobName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: jobName, UID: "job-uid", Controller: &isController,
			}},
		},
		Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

func newReader(objs ...client.Object) client.Reader {
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
}

func TestJobTransferReport(t *testing.T) {
	ctx := context.Background()

	// Happy path: a terminated container with a valid byte report.
	c := newReader(reportJob(), jobPod("j-abc", "j", `{"transferredBytes":4096,"skippedBytes":0,"totalBytes":4096}`, true))
	rep, ok, err := JobTransferReport(ctx, c, "ns", "j")
	if err != nil || !ok {
		t.Fatalf("expected a report; ok=%v err=%v", ok, err)
	}
	if rep.TransferredBytes != 4096 || rep.TotalBytes != 4096 {
		t.Errorf("report = %+v, want transferred=4096 total=4096", rep)
	}

	// No pods for the job -> (_, false, nil).
	if _, ok, err := JobTransferReport(ctx, newReader(reportJob()), "ns", "j"); err != nil || ok {
		t.Errorf("no pods should yield ok=false, nil err; ok=%v err=%v", ok, err)
	}

	// No Job -> (_, false, nil).
	if _, ok, err := JobTransferReport(ctx, newReader(), "ns", "j"); err != nil || ok {
		t.Errorf("no job should yield ok=false, nil err; ok=%v err=%v", ok, err)
	}

	// Pod present but container not terminated -> ok=false.
	c = newReader(reportJob(), jobPod("j-run", "j", "", false))
	if _, ok, err := JobTransferReport(ctx, c, "ns", "j"); err != nil || ok {
		t.Errorf("non-terminated container should yield ok=false; ok=%v err=%v", ok, err)
	}

	// Terminated but garbled message -> ok=false (not an error).
	c = newReader(reportJob(), jobPod("j-bad", "j", "not-json", true))
	if _, ok, err := JobTransferReport(ctx, c, "ns", "j"); err != nil || ok {
		t.Errorf("garbled message should yield ok=false, nil err; ok=%v err=%v", ok, err)
	}
}

// Any pod can carry the job-name label. Only the Job's own pods report: the
// report carries the manifest digest restores pin the artifact by.
func TestJobTransferReport_IgnoresPodsTheJobDoesNotControl(t *testing.T) {
	spoof := jobPod("spoof", "j", `{"manifestDigest":"sha256:attacker"}`, true)
	spoof.OwnerReferences = nil
	if rep, ok, err := JobTransferReport(context.Background(), newReader(reportJob(), spoof), "ns", "j"); err != nil || ok {
		t.Errorf("unowned pod supplied a report %+v (ok=%v err=%v)", rep, ok, err)
	}

	failed := jobPod("failed", "j", `{"manifestDigest":"sha256:partial"}`, true)
	failed.Status.Phase = corev1.PodFailed
	if rep, ok, err := JobTransferReport(context.Background(), newReader(reportJob(), failed), "ns", "j"); err != nil || ok {
		t.Errorf("a failed attempt's pod supplied a report %+v (ok=%v err=%v)", rep, ok, err)
	}
}
