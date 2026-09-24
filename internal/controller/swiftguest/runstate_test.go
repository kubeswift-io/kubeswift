package swiftguest

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// ranStatus is a guest as its launcher left it: running, with an address.
func ranStatus(podUID types.UID) *swiftv1alpha1.SwiftGuestStatus {
	st := &swiftv1alpha1.SwiftGuestStatus{
		Phase:   swiftv1alpha1.SwiftGuestPhaseRunning,
		PodRef:  &corev1.ObjectReference{Name: testGuestName, Namespace: "ns", UID: podUID},
		Network: &swiftv1alpha1.GuestNetworkStatus{PrimaryIP: "192.0.2.10"},
	}
	for _, t := range []string{"GuestRunning", ConditionPodScheduled,
		swiftv1alpha1.ConditionNetworkReady, swiftv1alpha1.ConditionEgressReady,
		swiftv1alpha1.ConditionPortsProgrammed} {
		setCondition(st, metav1.Condition{Type: t, Status: metav1.ConditionTrue, Reason: "Ran", Message: "from the last run"})
	}
	setCondition(st, metav1.Condition{Type: ConditionStorageReady, Status: metav1.ConditionTrue, Reason: "StorageReady", Message: "its disk"})
	return st
}

func assertNotRunning(t *testing.T, st *swiftv1alpha1.SwiftGuestStatus, wantReason string) {
	t.Helper()
	for _, ty := range []string{"GuestRunning", swiftv1alpha1.ConditionNetworkReady,
		swiftv1alpha1.ConditionEgressReady, swiftv1alpha1.ConditionPortsProgrammed} {
		c := findCondition(st, ty)
		if c == nil {
			t.Errorf("%s is missing", ty)
			continue
		}
		if c.Status != metav1.ConditionFalse {
			t.Errorf("%s = %s (%s); a guest with no launcher is none of these", ty, c.Status, c.Reason)
		}
		if wantReason != "" && c.Reason != wantReason {
			t.Errorf("%s reason = %q, want %q", ty, c.Reason, wantReason)
		}
	}
	if ip := st.Network.PrimaryIP; ip != "" {
		t.Errorf("primaryIP = %q; the address went with the launcher", ip)
	}
}

// Storage and resolution describe the guest, not the run, and must survive.
func TestClearRunState_KeepsWhatIsNotAboutTheRun(t *testing.T) {
	st := ranStatus("pod-1")
	ClearRunState(st, "Stopped", "stopped")
	if c := findCondition(st, ConditionStorageReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("StorageReady = %+v; it is not run-scoped", c)
	}
}

// A guest that never ran gains nothing from being told it is not running.
func TestClearRunState_InventsNoConditions(t *testing.T) {
	st := &swiftv1alpha1.SwiftGuestStatus{}
	ClearRunState(st, "Stopped", "stopped")
	if len(st.Conditions) != 0 {
		t.Errorf("conditions = %+v, want none", st.Conditions)
	}
}

// THE reported bug: stop a guest and it still says it is running, with the
// address it had (#634).
func TestReconcile_StoppedGuestIsNotRunningAndHasNoAddress(t *testing.T) {
	g := asDiskBoot(kernelGuest())
	g.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	g.Status = *ranStatus("pod-1")
	c := guestClientBuilder(g, testGuestClass(), readyImage(), preparedPVC()).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	got, _, err := reconcileGuest(t, r) // its launcher is already gone
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseStopped {
		t.Fatalf("phase = %q, want Stopped", got.Status.Phase)
	}
	assertNotRunning(t, &got.Status, "Stopped")
	if c := findCondition(&got.Status, ConditionPodScheduled); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("PodScheduled = %+v; there is no pod", c)
	}
}

// The second half of the bug: a restarting guest reported the PREVIOUS run's
// state until its new launcher overwrote it, so a wait on it returned at once.
func TestMapPodToStatus_ANewLauncherStartsFromNothing(t *testing.T) {
	st := ranStatus("pod-1")
	fresh := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testGuestName, Namespace: "ns", UID: "pod-2"},
		Spec:       corev1.PodSpec{NodeName: "worker-1"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	MapPodToStatus(fresh, st)
	assertNotRunning(t, st, "GuestStarting")
	if st.PodRef == nil || st.PodRef.UID != "pod-2" {
		t.Errorf("podRef = %+v, want the new launcher", st.PodRef)
	}
}

// The same launcher reporting again is not a new run: what it has said stands.
func TestMapPodToStatus_TheSameLauncherKeepsItsState(t *testing.T) {
	st := ranStatus("pod-1")
	same := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testGuestName, Namespace: "ns", UID: "pod-1"},
		Spec:       corev1.PodSpec{NodeName: "worker-1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	MapPodToStatus(same, st)
	if c := findCondition(st, "GuestRunning"); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("GuestRunning = %+v; the running launcher never stopped", c)
	}
	if st.Network.PrimaryIP != "192.0.2.10" {
		t.Errorf("primaryIP = %q; it is still the same run", st.Network.PrimaryIP)
	}
}

// A launcher that exited takes the run with it, whatever the phase becomes.
func TestMapPodToStatus_AnExitedLauncherIsNotRunning(t *testing.T) {
	for phase, wantGuestPhase := range map[corev1.PodPhase]swiftv1alpha1.SwiftGuestPhase{
		corev1.PodFailed:    swiftv1alpha1.SwiftGuestPhaseFailed,
		corev1.PodSucceeded: swiftv1alpha1.SwiftGuestPhaseStopped,
	} {
		st := ranStatus("pod-1")
		MapPodToStatus(&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: testGuestName, Namespace: "ns", UID: "pod-1"},
			Spec:       corev1.PodSpec{NodeName: "worker-1"},
			Status:     corev1.PodStatus{Phase: phase},
		}, st)
		if st.Phase != wantGuestPhase {
			t.Errorf("%s: phase = %q, want %q", phase, st.Phase, wantGuestPhase)
		}
		if c := findCondition(st, "GuestRunning"); c == nil || c.Status != metav1.ConditionFalse ||
			!strings.Contains(c.Message, "not running") {
			t.Errorf("%s: GuestRunning = %+v; the launcher exited", phase, c)
		}
		if st.Network.PrimaryIP != "" {
			t.Errorf("%s: primaryIP = %q, want it cleared", phase, st.Network.PrimaryIP)
		}
	}
}

// The case the launcher-change rule alone misses: an upgrade can arrive with
// podRef already naming the current pod, so nothing re-evaluates a guest that
// is stuck Pending while its status still says it is running — seen on a
// cluster, on a guest whose volume would not attach.
func TestMapPodToStatus_APendingLauncherIsNotRunning(t *testing.T) {
	st := ranStatus("pod-1")
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testGuestName, Namespace: "ns", UID: "pod-1"}, // SAME pod
		Spec:       corev1.PodSpec{NodeName: "worker-1"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	MapPodToStatus(pending, st)
	assertNotRunning(t, st, "GuestStarting")
	if st.Phase != swiftv1alpha1.SwiftGuestPhaseScheduling {
		t.Errorf("phase = %q, want Scheduling", st.Phase)
	}
}

// A pod stays Pending while ANY container is still waiting, so a running
// launcher next to a sidecar that has not started yet (the migration stunnel
// server pulling its image) reads Pending. swiftletd reports GuestRunning=True
// only once, so clearing it here left the guest not-running for good.
func TestMapPodToStatus_PendingWithRunningLauncherKeepsRunState(t *testing.T) {
	st := ranStatus("pod-1")
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testGuestName, Namespace: "ns", UID: "pod-1"},
		Spec:       corev1.PodSpec{NodeName: "worker-1"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: LauncherContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "stunnel", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
			},
		},
	}
	MapPodToStatus(pending, st)
	if c := findCondition(st, "GuestRunning"); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("GuestRunning = %+v; the launcher is running, only a sidecar is waiting", c)
	}
	if st.Network.PrimaryIP != "192.0.2.10" {
		t.Errorf("primaryIP = %q; the VM still holds its address", st.Network.PrimaryIP)
	}

	// The launcher itself waiting is still "not running".
	pending.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	MapPodToStatus(pending, st)
	assertNotRunning(t, st, "GuestStarting")
}

// Clearing must be idempotent: setCondition stamps lastTransitionTime every
// time, so a guest sitting Pending would otherwise have its status rewritten —
// and claim a transition — on every reconcile.
func TestClearRunState_IsIdempotent(t *testing.T) {
	st := ranStatus("pod-1")
	ClearRunState(st, "Stopped", "stopped")
	before := map[string]metav1.Time{}
	for _, c := range st.Conditions {
		before[c.Type] = c.LastTransitionTime
	}
	ClearRunState(st, "Stopped", "stopped")
	for _, c := range st.Conditions {
		if !c.LastTransitionTime.Equal(&[]metav1.Time{before[c.Type]}[0]) {
			t.Errorf("%s moved its lastTransitionTime without transitioning", c.Type)
		}
	}
}

// A different reason still lands: stopped is not the same as starting.
func TestClearRunState_ANewReasonIsRecorded(t *testing.T) {
	st := ranStatus("pod-1")
	ClearRunState(st, "GuestStarting", "starting")
	ClearRunState(st, "Stopped", "stopped")
	if c := findCondition(st, "GuestRunning"); c == nil || c.Reason != "Stopped" {
		t.Errorf("GuestRunning = %+v, want the newer reason", c)
	}
}
