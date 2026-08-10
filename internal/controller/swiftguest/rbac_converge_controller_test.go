package swiftguest

import "testing"

// launcherClassForBinding drives whether a RoleBinding is converged at all, and
// LauncherClass is an int whose zero value is a REAL class (GuestLauncher). A
// caller that trusts the class without the bool would converge every RoleBinding
// in the cluster as if it were a guest launcher.

func TestLauncherClassForBinding(t *testing.T) {
	for _, tc := range []struct {
		name      string
		binding   string
		wantClass LauncherClass
		wantOK    bool
	}{
		{"guest launcher", SwiftletdReporterRoleBindingName, GuestLauncher, true},
		{"sandbox launcher", SandboxReporterRoleBindingName, SandboxLauncher, true},
		{"unrelated binding", "some-operator-binding", GuestLauncher, false},
		{"empty", "", GuestLauncher, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := launcherClassForBinding(tc.binding)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.wantClass {
				t.Errorf("class = %v, want %v", got, tc.wantClass)
			}
		})
	}
}

// The zero value of LauncherClass is GuestLauncher, so "not ours" cannot be
// signalled through the class. This pins that the bool is the only usable
// signal — if someone later switches the sentinel to a class comparison, an
// unrelated RoleBinding would look exactly like a guest launcher.
func TestLauncherClassForBinding_ZeroValueIsNotASentinel(t *testing.T) {
	class, ok := launcherClassForBinding("not-a-launcher-binding")
	if ok {
		t.Fatal("unrelated binding reported as a launcher binding")
	}
	if class != GuestLauncher {
		t.Fatalf("class = %v; the point of this test is that it EQUALS GuestLauncher, "+
			"so the class alone cannot distinguish 'not ours'", class)
	}
}
