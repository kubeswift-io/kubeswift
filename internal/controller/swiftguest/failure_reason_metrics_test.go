package swiftguest

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
)

// TestRecordGuestMetrics_FailureReasonIsBoundedToken guards the O2
// cardinality fix: the vm_failures_total reason label must carry the
// condition's machine Reason token, never its free-text Message (messages
// embed pod/node names — unbounded series cardinality, observability
// design doc §1.5).
func TestRecordGuestMetrics_FailureReasonIsBoundedToken(t *testing.T) {
	g := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "reasonns"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}},
	}
	pending := &swiftv1alpha1.SwiftGuestStatus{Phase: swiftv1alpha1.SwiftGuestPhasePending}
	failed := &swiftv1alpha1.SwiftGuestStatus{
		Phase: swiftv1alpha1.SwiftGuestPhaseFailed,
		Conditions: []metav1.Condition{
			// A healthy True condition whose Reason must NOT be picked.
			{Type: "PodScheduled", Status: metav1.ConditionTrue, Reason: "PodScheduled",
				Message: "pod scheduled to node miles"},
			// The failing condition: Reason is the bounded token, Message the
			// free text that previously leaked into the label.
			{Type: "GuestRunning", Status: metav1.ConditionFalse, Reason: "PodFailed",
				Message: "launcher pod g-mig-abc123 failed on node miles: init container gpu-init exited 1"},
		},
	}

	before := testutil.ToFloat64(metrics.VMFailuresTotal.WithLabelValues("reasonns", "PodFailed"))
	recordGuestMetrics(g, pending, failed, nil)
	if got := testutil.ToFloat64(metrics.VMFailuresTotal.WithLabelValues("reasonns", "PodFailed")); got != before+1 {
		t.Errorf("vm_failures_total{reason=\"PodFailed\"} = %v, want %v", got, before+1)
	}

	// The free-text message must not appear as any label value.
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "kubeswift_vm_failures_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if strings.Contains(l.GetValue(), "g-mig-abc123") || strings.Contains(l.GetValue(), "exited 1") {
					t.Errorf("free-text message leaked into label %s=%q", l.GetName(), l.GetValue())
				}
			}
		}
	}

	// No usable Reason on any failing condition -> "Unknown", never "".
	failedNoReason := &swiftv1alpha1.SwiftGuestStatus{
		Phase: swiftv1alpha1.SwiftGuestPhaseFailed,
		Conditions: []metav1.Condition{
			{Type: "GuestRunning", Status: metav1.ConditionFalse, Message: "free text only"},
		},
	}
	ub := testutil.ToFloat64(metrics.VMFailuresTotal.WithLabelValues("reasonns", "Unknown"))
	recordGuestMetrics(g, pending, failedNoReason, nil)
	if got := testutil.ToFloat64(metrics.VMFailuresTotal.WithLabelValues("reasonns", "Unknown")); got != ub+1 {
		t.Errorf("vm_failures_total{reason=\"Unknown\"} = %v, want %v", got, ub+1)
	}
}
