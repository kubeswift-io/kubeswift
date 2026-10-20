package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// previousGuestLauncher is the launcher of an earlier guest named like the
// test guest: same pod name, controlled by a different UID, with a VM that
// reported an IP.
func previousGuestLauncher(stopping bool) *corev1.Pod {
	p := runningLauncher("worker-1")
	p.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(
		&swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: testGuestName, UID: types.UID("previous-uid")}},
		schema.GroupVersionKind{Group: swiftv1alpha1.GroupName, Version: swiftv1alpha1.Version, Kind: "SwiftGuest"})}
	p.Annotations = map[string]string{PodAnnotationGuestIP: "192.168.99.13"}
	if stopping {
		now := metav1.Now()
		p.DeletionTimestamp = &now
		p.Finalizers = []string{"test/hold"} // the fake client keeps a deleted object only with a finalizer
	}
	return p
}

func TestReconcile_RecreatedGuestDoesNotReportThePreviousLauncher(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopping   bool
		wantReason string
	}{
		{"previous launcher stopping", true, reasonPreviousLauncherStopping},
		{"live pod it does not control", false, reasonLauncherPodConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := guestClientBuilder(kernelGuest(), testGuestClass(), readyKernel(), previousGuestLauncher(tc.stopping)).Build()
			r := &SwiftGuestReconciler{Client: c, Scheme: c.Scheme()}

			g, res, err := reconcileGuest(t, r)
			if err != nil {
				t.Fatal(err)
			}
			if g.Status.Phase != swiftv1alpha1.SwiftGuestPhasePending {
				t.Errorf("phase = %q, want Pending: the pod is not this guest's", g.Status.Phase)
			}
			if c := guestCondition(t, g, ConditionPodScheduled); c.Status != metav1.ConditionFalse || c.Reason != tc.wantReason {
				t.Errorf("PodScheduled = %s/%s, want False/%s", c.Status, c.Reason, tc.wantReason)
			}
			if g.Status.NodeName != "" || g.Status.Network != nil || g.Status.PodRef != nil {
				t.Errorf("status reports the other pod: nodeName=%q network=%+v podRef=%+v",
					g.Status.NodeName, g.Status.Network, g.Status.PodRef)
			}
			if res.RequeueAfter == 0 {
				t.Error("no requeue: the guest must look again once the pod is gone")
			}
			pods := launcherPods(t, c)
			if len(pods) != 1 || pods[0].OwnerReferences[0].UID != "previous-uid" {
				t.Errorf("pods = %+v, want only the previous guest's, untouched", pods)
			}
		})
	}
}

func TestReconcile_StoppedGuestDoesNotStopAnotherGuestsLauncher(t *testing.T) {
	g := kernelGuest()
	g.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	c := guestClientBuilder(g, testGuestClass(), readyKernel(), previousGuestLauncher(false)).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: c.Scheme()}

	if _, _, err := reconcileGuest(t, r); err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), testGuestKey, &pod); err != nil {
		t.Fatalf("the other guest's launcher was deleted: %v", err)
	}
	if pod.DeletionTimestamp != nil {
		t.Error("the other guest's launcher was deleted")
	}
}

func TestReconcile_RecreatedGuestStartsItsLauncherOnceThePreviousOneIsGone(t *testing.T) {
	old := previousGuestLauncher(true)
	c := guestClientBuilder(kernelGuest(), testGuestClass(), readyKernel(), old).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: c.Scheme()}
	if _, _, err := reconcileGuest(t, r); err != nil {
		t.Fatal(err)
	}

	// The previous pod finishes stopping.
	old.Finalizers = nil
	if err := c.Update(context.Background(), old); err != nil {
		t.Fatal(err)
	}

	g, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status.Phase != swiftv1alpha1.SwiftGuestPhaseScheduling {
		t.Errorf("phase = %q, want Scheduling: its own launcher was just created", g.Status.Phase)
	}
	pods := launcherPods(t, c)
	if len(pods) != 1 || !metav1.IsControlledBy(&pods[0], g) {
		t.Errorf("pods = %+v, want one launcher controlled by this guest", pods)
	}
}
