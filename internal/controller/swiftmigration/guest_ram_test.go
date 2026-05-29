package swiftmigration

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestGuestRAMMiB verifies the progress-estimate input wiring: the
// controller resolves the guest's RAM from its (cluster-scoped)
// SwiftGuestClass so the send action-args carry guest_ram_mib, which
// swiftletd-source requires to emit the migration-progress-estimate
// annotation. PR 2 shipped without this wiring (the field was absent
// from the send args), so the progress estimate never appeared in the
// controller-driven path.
func TestGuestRAMMiB(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("g", "default", "class-4g")
	class := newGuestClass("class-4g", 2, 4096) // 4096 MiB
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	got := guestRAMMiB(context.Background(), r, guest)
	if got == nil {
		t.Fatal("guestRAMMiB = nil, want 4096")
	}
	if *got != 4096 {
		t.Errorf("guestRAMMiB = %d, want 4096", *got)
	}
}

// TestGuestRAMMiB_ClassMissing_NilBestEffort verifies the best-effort
// posture: a missing/unreadable class yields nil (progress estimate
// disabled) rather than an error — it must never fail the migration.
func TestGuestRAMMiB_ClassMissing_NilBestEffort(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("g", "default", "nonexistent-class")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	if got := guestRAMMiB(context.Background(), r, guest); got != nil {
		t.Fatalf("guestRAMMiB with missing class = %d, want nil (best-effort)", *got)
	}
}
