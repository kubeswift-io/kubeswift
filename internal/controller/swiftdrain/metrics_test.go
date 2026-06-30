package swiftdrain

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kubeswift-io/kubeswift/internal/metrics"
)

// TestReconcile_CreatesMigration_CountsDrainMetric: the O3 drain counter
// fires once per created drain migration, labelled with the guest's
// drainPolicy. The in-flight duplicate guard means a second reconcile must
// not re-count.
func TestReconcile_CreatesMigration_CountsDrainMetric(t *testing.T) {
	r, c := newR(guest("mg", drain("miles"), statusNode("miles")), node("miles"), node("boba"), smallClass())

	before := testutil.ToFloat64(metrics.DrainMigrationsTotal.WithLabelValues("Migrate", "created"))
	reconcileGuest(t, r, "mg")
	if got := testutil.ToFloat64(metrics.DrainMigrationsTotal.WithLabelValues("Migrate", "created")); got != before+1 {
		t.Errorf("drain_migrations_total{policy=Migrate,result=created} = %v, want %v", got, before+1)
	}
	if migs := listMigs(t, c); len(migs) != 1 {
		t.Fatalf("expected 1 migration; got %d", len(migs))
	}

	// Second reconcile observes the in-flight migration — no new create, no
	// re-count.
	reconcileGuest(t, r, "mg")
	if got := testutil.ToFloat64(metrics.DrainMigrationsTotal.WithLabelValues("Migrate", "created")); got != before+1 {
		t.Errorf("in-flight duplicate guard must not re-count; got %v, want %v", got, before+1)
	}
}
