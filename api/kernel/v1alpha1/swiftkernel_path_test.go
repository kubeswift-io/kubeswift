package v1alpha1

import "testing"

// KernelLocalPath must not collide across namespaces. Namespace and name can
// both contain '-', so a '-'-joined path is ambiguous; nesting them as separate
// segments (neither can contain '/') is not. A collision would let one tenant's
// kernel pull overwrite another tenant's kernel/initramfs on a shared node.
func TestKernelLocalPath_NoCrossNamespaceCollision(t *testing.T) {
	a := KernelLocalPath("team", "a-prod")
	b := KernelLocalPath("team-a", "prod")
	if a == b {
		t.Fatalf("distinct (namespace, name) pairs collide on %q", a)
	}
	if got, want := KernelLocalPath("team", "a-prod"), "/var/lib/kubeswift/kernels/team/a-prod"; got != want {
		t.Errorf("KernelLocalPath = %q, want %q", got, want)
	}
}
