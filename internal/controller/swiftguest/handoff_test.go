package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// handedOffLauncher is the test guest's source launcher after a live
// migration's send completed: the VM runs in the destination pod, and the
// plaintext-transport launcher exited 0.
func handedOffLauncher() *corev1.Pod {
	p := runningLauncher("worker-1")
	p.UID = "pod-1"
	p.Status.Phase = corev1.PodSucceeded
	p.Annotations = map[string]string{PodAnnotationMigrationStatus: "complete"}
	return p
}

// The source launcher's exit is not the guest's: mapping it cleared the
// GuestRunning=True the destination had already written (once), and the
// migration then waited in Resuming until spec.timeout.
func TestMapPodToStatus_AHandedOffLauncherChangesNothing(t *testing.T) {
	st := ranStatus("pod-1")
	MapPodToStatus(handedOffLauncher(), st)
	if st.Phase != swiftv1alpha1.SwiftGuestPhaseRunning {
		t.Errorf("phase = %q; the VM is still running, in the destination pod", st.Phase)
	}
	if c := findCondition(st, "GuestRunning"); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("GuestRunning = %+v; the source's exit must not clear it", c)
	}
	if st.Network.PrimaryIP != "192.0.2.10" {
		t.Errorf("primaryIP = %q; the migrated VM keeps it", st.Network.PrimaryIP)
	}
}

// With runPolicy: Always the source launcher's exit read as a guest shutdown:
// the controller deleted it and started a fresh launcher, a second copy of the
// VM on the same disk as the migrated one.
func TestReconcile_AlwaysDoesNotRestartAHandedOffLauncher(t *testing.T) {
	g := kernelGuest()
	g.Spec.RunPolicy = swiftv1alpha1.RunPolicyAlways
	g.Status = *ranStatus("pod-1")
	src := handedOffLauncher()
	c := guestClientBuilder(g, testGuestClass(), readyKernel(), src).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	pods := launcherPods(t, c)
	if len(pods) != 1 || pods[0].UID != src.UID {
		t.Fatalf("launcher pods = %d (first uid %v); the handed-off launcher must be left for cutover, not replaced",
			len(pods), func() any {
				if len(pods) > 0 {
					return pods[0].UID
				}
				return nil
			}())
	}
	if got.Status.RestartCount != 0 {
		t.Errorf("restartCount = %d; nothing restarted", got.Status.RestartCount)
	}
	if c := findCondition(&got.Status, "GuestRunning"); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("GuestRunning = %+v; the source's exit must not clear it", c)
	}
}
