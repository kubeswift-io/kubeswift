package swiftguest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/metrics"
)

func TestRecordGuestMetrics_CloneCounter(t *testing.T) {
	clone := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-a", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			CloneFromSnapshot: &swiftv1alpha1.CloneFromSnapshotSource{SnapshotRef: corev1.LocalObjectReference{Name: "snap"}},
		},
	}
	pending := &swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhasePending}
	running := &swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhaseRunning}
	failed := &swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhaseFailed}

	// Transition into Running -> clone_total{running} +1.
	before := testutil.ToFloat64(metrics.CloneTotal.WithLabelValues("running"))
	recordGuestMetrics(clone, pending, running, nil)
	if got := testutil.ToFloat64(metrics.CloneTotal.WithLabelValues("running")); got != before+1 {
		t.Errorf("CloneTotal{running} = %v, want %v", got, before+1)
	}

	// Transition into Failed -> clone_total{failed} +1.
	fb := testutil.ToFloat64(metrics.CloneTotal.WithLabelValues("failed"))
	recordGuestMetrics(clone, pending, failed, nil)
	if got := testutil.ToFloat64(metrics.CloneTotal.WithLabelValues("failed")); got != fb+1 {
		t.Errorf("CloneTotal{failed} = %v, want %v", got, fb+1)
	}

	// Running -> Running (no transition) must not increment.
	rb := testutil.ToFloat64(metrics.CloneTotal.WithLabelValues("running"))
	recordGuestMetrics(clone, running, running, nil)
	if got := testutil.ToFloat64(metrics.CloneTotal.WithLabelValues("running")); got != rb {
		t.Errorf("no phase transition must not increment clone_total{running}; %v -> %v", rb, got)
	}

	// A NON-clone guest never touches clone_total.
	nonClone := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}},
	}
	nb := testutil.ToFloat64(metrics.CloneTotal.WithLabelValues("running"))
	recordGuestMetrics(nonClone, pending, running, nil)
	if got := testutil.ToFloat64(metrics.CloneTotal.WithLabelValues("running")); got != nb {
		t.Errorf("a non-clone guest must not increment clone_total{running}; %v -> %v", nb, got)
	}
}
