package swiftguest

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func waitingLauncher(node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec: corev1.PodSpec{NodeName: node, Volumes: []corev1.Volume{{Name: "root-disk",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "swiftguest-root-g"}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func rootPVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "swiftguest-root-g", Namespace: "ns"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-1"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func attachment(node string, attached bool, attachErr string) *storagev1.VolumeAttachment {
	pv := "pv-1"
	va := &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "csi-" + node},
		Spec:       storagev1.VolumeAttachmentSpec{NodeName: node, Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: &pv}},
		Status:     storagev1.VolumeAttachmentStatus{Attached: attached},
	}
	if attachErr != "" {
		va.Status.AttachError = &storagev1.VolumeError{Message: attachErr}
	}
	return va
}

// A scheduled launcher waiting for its root disk turns StorageReady False and
// names the step; once the kubelet has the volumes mounted, or for anything
// that is not a storage wait, the condition is left as it was (#909).
func TestReportRootDiskWait(t *testing.T) {
	sandboxReady := func(p *corev1.Pod) *corev1.Pod {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionTrue}}
		return p
	}
	initRunning := func(p *corev1.Pod) *corev1.Pod {
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "clone-grow-init",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		return p
	}
	for name, tc := range map[string]struct {
		pod     *corev1.Pod
		objs    []client.Object
		reason  string // "" = condition untouched (still True)
		inMsg   []string
		requeue bool
	}{
		"no attachment yet": {pod: waitingLauncher("n1"), objs: []client.Object{rootPVC()},
			reason: reasonRootDiskAttaching, inMsg: []string{"swiftguest-root-g", "node n1"}, requeue: true},
		"attach refused": {pod: waitingLauncher("n1"), objs: []client.Object{rootPVC(), attachment("n1", false, "volume is being cloned")},
			reason: reasonRootDiskAttaching, inMsg: []string{"volume is being cloned"}, requeue: true},
		"attached elsewhere only": {pod: waitingLauncher("n1"), objs: []client.Object{rootPVC(), attachment("n2", true, "")},
			reason: reasonRootDiskAttaching, requeue: true},
		"attached, mounting": {pod: waitingLauncher("n1"), objs: []client.Object{rootPVC(), attachment("n1", true, "")},
			reason: reasonRootDiskMounting, inMsg: []string{"kubelet on node n1"}, requeue: true},
		"sandbox ready": {pod: sandboxReady(waitingLauncher("n1")), objs: []client.Object{rootPVC()}},
		"init running":  {pod: initRunning(waitingLauncher("n1")), objs: []client.Object{rootPVC()}},
		"unscheduled":   {pod: waitingLauncher(""), objs: []client.Object{rootPVC()}},
		"no root PVC":   {pod: &corev1.Pod{Spec: corev1.PodSpec{NodeName: "n1"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}},
		"running":       {pod: func() *corev1.Pod { p := waitingLauncher("n1"); p.Status.Phase = corev1.PodRunning; return p }()},
		"PVC gone":      {pod: waitingLauncher("n1"), reason: reasonRootDiskMounting, requeue: true},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(tc.objs...).Build()
			r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
			guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}}
			status := &swiftv1alpha1.SwiftGuestStatus{}
			SetStorageReadyCondition(status, true, "", "")
			requeue, err := r.reportRootDiskWait(context.Background(), guest, tc.pod, status)
			if err != nil {
				t.Fatal(err)
			}
			cond := findCondition(status, ConditionStorageReady)
			if tc.reason == "" {
				if cond.Status != metav1.ConditionTrue || requeue != 0 {
					t.Errorf("want StorageReady untouched and no requeue, got %+v requeue %v", cond, requeue)
				}
				return
			}
			if cond.Status != metav1.ConditionFalse || cond.Reason != tc.reason {
				t.Errorf("StorageReady = %s/%s, want False/%s", cond.Status, cond.Reason, tc.reason)
			}
			for _, s := range tc.inMsg {
				if !strings.Contains(cond.Message, s) {
					t.Errorf("message %q lacks %q", cond.Message, s)
				}
			}
			if (requeue > 0) != tc.requeue {
				t.Errorf("requeue = %v", requeue)
			}
		})
	}
}

// A storage pre-flight failure stays on the condition: it is the more basic
// problem, and the wait would hide it.
func TestReportRootDiskWait_KeepsPreflightFailure(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(rootPVC()).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	status := &swiftv1alpha1.SwiftGuestStatus{}
	SetStorageReadyCondition(status, false, "LonghornMigratableMissing", "x")
	if _, err := r.reportRootDiskWait(context.Background(), &swiftv1alpha1.SwiftGuest{}, waitingLauncher("n1"), status); err != nil {
		t.Fatal(err)
	}
	if got := findCondition(status, ConditionStorageReady).Reason; got != "LonghornMigratableMissing" {
		t.Errorf("reason = %s", got)
	}
}
