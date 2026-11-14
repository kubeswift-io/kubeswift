package swiftsandbox

import (
	"strings"
	"testing"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// A sandbox's guest sees VMX/SVM only when it says Enabled: unset and
// Disabled both hide it (a SwiftGuest defaults the other way).
func TestBuildIntent_NestedVirtualization(t *testing.T) {
	for v, wantDisabled := range map[swiftv1alpha1.NestedVirtualization]bool{
		"": true,
		swiftv1alpha1.NestedVirtualizationDisabled: true,
		swiftv1alpha1.NestedVirtualizationEnabled:  false,
	} {
		sb := egSandbox("")
		sb.Spec.NestedVirtualization = v
		for _, idle := range []bool{false, true} {
			if got := buildIntent(sb, "sandbox", "/r", "", execSpec{}, idle).DisableNested; got != wantDisabled {
				t.Errorf("%q idle=%v: DisableNested = %v, want %v", v, idle, got, wantDisabled)
			}
		}
	}
}

// A pool's slots boot with the pool's setting, and the profile records the
// effective value, so a slot from before the field existed is recycled.
func TestPoolNestedVirtualization(t *testing.T) {
	pool := shapedPool()
	pool.Spec.NestedVirtualization = swiftv1alpha1.NestedVirtualizationEnabled
	r := &SwiftSandboxPoolReconciler{}
	if got := r.slotTemplate(pool, "slot").Spec.NestedVirtualization; got != swiftv1alpha1.NestedVirtualizationEnabled {
		t.Errorf("slot template = %q", got)
	}
	// Enabled is what every pre-existing slot ran with: no key, no recycle.
	if strings.Contains(poolSlotProfile(pool), `"nested"`) {
		t.Errorf("profile = %s", poolSlotProfile(pool))
	}
	if !strings.Contains(poolSlotProfile(shapedPool()), `"nested":"Disabled"`) {
		t.Errorf("unset pool profile = %s", poolSlotProfile(shapedPool()))
	}
	// Unset and Disabled are one setting: no cold fallback between them.
	sb := matchingSandbox()
	sb.Spec.NestedVirtualization = swiftv1alpha1.NestedVirtualizationDisabled
	if got := slotMismatches(shapedPool(), sb); len(got) != 0 {
		t.Errorf("Disabled vs unset: %q", got)
	}
	// An Enabled pool serves an Enabled checkout warm, and an unset one cold.
	sb.Spec.NestedVirtualization = swiftv1alpha1.NestedVirtualizationEnabled
	if got := slotMismatches(pool, sb); len(got) != 0 {
		t.Errorf("Enabled vs Enabled: %q", got)
	}
	sb.Spec.NestedVirtualization = ""
	if got := slotMismatches(pool, sb); len(got) != 1 || got[0] != "nestedVirtualization (pool Enabled, sandbox Disabled)" {
		t.Errorf("Enabled pool vs unset sandbox: %q", got)
	}
}
