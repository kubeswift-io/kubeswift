package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	kubeswiftscheme "github.com/projectbeskar/kubeswift/internal/scheme"
)

// newTestResolver builds a GuestResolver backed by a controller-runtime
// fake client populated with the given objects. Both core/v1 and the
// swift API group are registered so SwiftGuests with PodRef can be
// resolved alongside their pods.
func newTestResolver(t *testing.T, objs ...client.Object) *GuestResolver {
	t.Helper()
	fc := fake.NewClientBuilder().
		WithScheme(kubeswiftscheme.Scheme).
		WithObjects(objs...).
		Build()
	return &GuestResolver{Client: fc}
}

// newGuest builds a SwiftGuest with optional status.podRef pointing at
// the given pod name (empty podRefName leaves the field nil).
func newGuest(name, namespace, podRefName string) *swiftv1alpha1.SwiftGuest {
	g := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "guest-uid"},
	}
	if podRefName != "" {
		g.Status.PodRef = &corev1.ObjectReference{
			Kind:      "Pod",
			Namespace: namespace,
			Name:      podRefName,
		}
	}
	return g
}

// newLabeledPod builds a pod carrying the swift.kubeswift.io/guest=<guestName>
// label, with the requested phase, optional DeletionTimestamp (Terminating),
// and a creationTimestamp set to now-age (positive age yields older pods).
func newLabeledPod(name, namespace, guestName string, phase corev1.PodPhase, terminating bool, age time.Duration) *corev1.Pod {
	created := metav1.NewTime(time.Now().Add(-age))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			Labels:            map[string]string{GuestLabelKey: guestName},
			CreationTimestamp: created,
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	if terminating {
		// Fake client tolerates a manually set DeletionTimestamp on
		// pods registered via WithObjects, BUT only when at least one
		// finalizer is present (otherwise the validating admission
		// strips the timestamp). The finalizer never fires in tests
		// because nothing reconciles it.
		now := metav1.NewTime(time.Now())
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"kubeswift.io/test-pin"}
	}
	return pod
}

// T1: PodRef set, pod exists -> returns named pod.
func TestResolvePod_PodRefHit_ReturnsNamedPod(t *testing.T) {
	guest := newGuest("g1", "ns", "g1")
	pod := newLabeledPod("g1", "ns", "g1", corev1.PodRunning, false, 5*time.Minute)
	r := newTestResolver(t, guest, pod)

	got, err := r.ResolvePod(context.Background(), guest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "g1" {
		t.Errorf("Name: want g1, got %q", got.Name)
	}
}

// T2: PodRef set, pod NotFound, no labeled pods -> returns "not found".
func TestResolvePod_PodRefNotFound_NoLabeledPods_ReturnsNotFound(t *testing.T) {
	guest := newGuest("g1", "ns", "g1-mig-abcdef") // podRef points at gone dst
	r := newTestResolver(t, guest)                 // no pods at all

	_, err := r.ResolvePod(context.Background(), guest)
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error: want substring 'not found', got %q", err.Error())
	}
}

// T3: PodRef set, pod NotFound, one labeled pod -> returns labeled pod.
// Foot-gun 1 fix.
func TestResolvePod_PodRefNotFound_OneLabeledPod_ReturnsLabeledPod(t *testing.T) {
	guest := newGuest("g1", "ns", "g1-mig-abcdef") // podRef points at not-yet-created dst
	pod := newLabeledPod("g1", "ns", "g1", corev1.PodRunning, false, 5*time.Minute)
	r := newTestResolver(t, guest, pod)

	got, err := r.ResolvePod(context.Background(), guest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "g1" {
		t.Errorf("Name: want g1 (fallback labeled pod), got %q", got.Name)
	}
}

// T4: PodRef nil, one labeled pod -> returns labeled pod.
func TestResolvePod_PodRefNil_OneLabeledPod_ReturnsLabeledPod(t *testing.T) {
	guest := newGuest("g1", "ns", "")
	pod := newLabeledPod("g1", "ns", "g1", corev1.PodRunning, false, 5*time.Minute)
	r := newTestResolver(t, guest, pod)

	got, err := r.ResolvePod(context.Background(), guest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "g1" {
		t.Errorf("Name: want g1, got %q", got.Name)
	}
}

// T5: Two labeled pods (one Running, one Terminating) -> returns Running.
// Foot-gun 2 primary case.
func TestResolvePod_TwoLabeled_RunningVsTerminating_PrefersRunning(t *testing.T) {
	guest := newGuest("g1", "ns", "")
	terminating := newLabeledPod("g1", "ns", "g1", corev1.PodRunning, true, 10*time.Minute)
	running := newLabeledPod("g1-mig-abcdef", "ns", "g1", corev1.PodRunning, false, 1*time.Minute)
	r := newTestResolver(t, guest, terminating, running)

	got, err := r.ResolvePod(context.Background(), guest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "g1-mig-abcdef" {
		t.Errorf("Name: want g1-mig-abcdef (Running, non-Terminating), got %q", got.Name)
	}
}

// T6: Two labeled pods both Running, different CreationTimestamp -> newest.
// Foot-gun 2 tie-break.
func TestResolvePod_TwoRunning_TieBreakByNewest(t *testing.T) {
	guest := newGuest("g1", "ns", "")
	older := newLabeledPod("g1", "ns", "g1", corev1.PodRunning, false, 10*time.Minute)
	newer := newLabeledPod("g1-mig-abcdef", "ns", "g1", corev1.PodRunning, false, 1*time.Minute)
	r := newTestResolver(t, guest, older, newer)

	got, err := r.ResolvePod(context.Background(), guest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "g1-mig-abcdef" {
		t.Errorf("Name: want g1-mig-abcdef (newest), got %q", got.Name)
	}
}

// T7: Three labeled pods (Pending, Running, Terminating) -> returns Running.
// Foot-gun 2 full ordering.
func TestResolvePod_ThreeCandidates_PendingRunningTerminating_PrefersRunning(t *testing.T) {
	guest := newGuest("g1", "ns", "")
	pending := newLabeledPod("g1-mig-aaa", "ns", "g1", corev1.PodPending, false, 1*time.Minute)
	running := newLabeledPod("g1-mig-bbb", "ns", "g1", corev1.PodRunning, false, 5*time.Minute)
	terminating := newLabeledPod("g1", "ns", "g1", corev1.PodRunning, true, 30*time.Minute)
	r := newTestResolver(t, guest, pending, running, terminating)

	got, err := r.ResolvePod(context.Background(), guest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "g1-mig-bbb" {
		t.Errorf("Name: want g1-mig-bbb (Running, non-Terminating), got %q", got.Name)
	}
}

// T8: All pods Terminating -> returns newest, no error.
// Foot-gun 2 all-terminating fallback.
func TestResolvePod_AllTerminating_ReturnsNewestNoError(t *testing.T) {
	guest := newGuest("g1", "ns", "")
	older := newLabeledPod("g1", "ns", "g1", corev1.PodRunning, true, 10*time.Minute)
	newer := newLabeledPod("g1-mig-abcdef", "ns", "g1", corev1.PodRunning, true, 1*time.Minute)
	r := newTestResolver(t, guest, older, newer)

	got, err := r.ResolvePod(context.Background(), guest)
	if err != nil {
		t.Fatalf("unexpected error (all-Terminating must not error): %v", err)
	}
	if got.Name != "g1-mig-abcdef" {
		t.Errorf("Name: want g1-mig-abcdef (newest), got %q", got.Name)
	}
}

// T9: PodRef set, pod exists; a stale labeled pod also exists with
// matching guest label and different name. PodRef remains authoritative.
func TestResolvePod_PodRefHit_IgnoresLabeledCandidate(t *testing.T) {
	// PodRef points at the post-cutover dst pod. A stale src pod is
	// still hanging around with the same guest label (e.g., kubelet
	// has not finished termination). PodRef wins.
	guest := newGuest("g1", "ns", "g1-mig-abcdef")
	dst := newLabeledPod("g1-mig-abcdef", "ns", "g1", corev1.PodRunning, false, 1*time.Minute)
	stale := newLabeledPod("g1", "ns", "g1", corev1.PodRunning, true, 10*time.Minute)
	r := newTestResolver(t, guest, dst, stale)

	got, err := r.ResolvePod(context.Background(), guest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "g1-mig-abcdef" {
		t.Errorf("Name: want g1-mig-abcdef (PodRef authoritative), got %q", got.Name)
	}
}
