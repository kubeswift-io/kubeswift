package swiftsandbox

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

func TestResolveEntrypoint(t *testing.T) {
	cfg := materialize.ImageConfig{Entrypoint: []string{"/ep"}, Cmd: []string{"/cmd"}}
	// spec.command wins (only [0]; v1 limitation).
	sb := &sandboxv1alpha1.SwiftSandbox{Spec: sandboxv1alpha1.SwiftSandboxSpec{Command: []string{"/mine", "arg"}}}
	if got := resolveEntrypoint(sb, cfg); got != "/mine" {
		t.Errorf("command should win: %q", got)
	}
	// else image entrypoint.
	if got := resolveEntrypoint(&sandboxv1alpha1.SwiftSandbox{}, cfg); got != "/ep" {
		t.Errorf("entrypoint: %q", got)
	}
	// else image cmd.
	if got := resolveEntrypoint(&sandboxv1alpha1.SwiftSandbox{}, materialize.ImageConfig{Cmd: []string{"/cmd"}}); got != "/cmd" {
		t.Errorf("cmd: %q", got)
	}
	// else empty (bridge default /sbin/init -> /bin/sh).
	if got := resolveEntrypoint(&sandboxv1alpha1.SwiftSandbox{}, materialize.ImageConfig{}); got != "" {
		t.Errorf("empty: %q", got)
	}
}

func TestBuildIntent(t *testing.T) {
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{CPU: 2, Memory: resource.MustParse("1Gi")},
	}
	intent := buildIntent(sb, "sandbox", "/var/lib/kubeswift/sandbox-rootfs/sha256-abc.ext4", "/bin/sh")
	if intent.KernelBoot == nil {
		t.Fatal("kernelBoot nil")
	}
	if !strings.HasSuffix(intent.KernelBoot.KernelPath, "/bzImage") ||
		!strings.HasSuffix(intent.KernelBoot.InitramfsPath, "/rootfs.cpio.gz") {
		t.Errorf("kernel paths: %s / %s", intent.KernelBoot.KernelPath, intent.KernelBoot.InitramfsPath)
	}
	if !strings.Contains(intent.KernelBoot.Cmdline, "kubeswift.rootfs=block") ||
		!strings.Contains(intent.KernelBoot.Cmdline, "kubeswift.entrypoint=/bin/sh") {
		t.Errorf("cmdline: %s", intent.KernelBoot.Cmdline)
	}
	if intent.SandboxRootfs == nil || intent.SandboxRootfs.Path != "/var/lib/kubeswift/sandbox-rootfs/sha256-abc.ext4" {
		t.Errorf("sandboxRootfs: %+v", intent.SandboxRootfs)
	}
	// Default/restricted mode -> networked; mode=none -> network-isolated.
	if !intent.Network {
		t.Error("default (restricted) sandbox should be networked")
	}
	sbNone := sb.DeepCopy()
	sbNone.Spec.Network.Mode = sandboxv1alpha1.SandboxNetworkNone
	if buildIntent(sbNone, "sandbox", "/x.ext4", "").Network {
		t.Error("mode=none sandbox must be network-isolated")
	}
	if intent.CPU != 2 || intent.Memory != 1024 {
		t.Errorf("cpu/mem: %d/%d (want 2/1024)", intent.CPU, intent.Memory)
	}
	if intent.Hypervisor != "cloud-hypervisor" {
		t.Errorf("hypervisor: %s", intent.Hypervisor)
	}
	// No entrypoint -> the cmdline omits kubeswift.entrypoint (bridge default).
	i2 := buildIntent(sb, "sandbox", "/x.ext4", "")
	if strings.Contains(i2.KernelBoot.Cmdline, "kubeswift.entrypoint") {
		t.Errorf("empty entrypoint should omit the arg: %s", i2.KernelBoot.Cmdline)
	}
}

func TestIsTerminal(t *testing.T) {
	if !isTerminal(sandboxv1alpha1.SwiftSandboxCompleted) || !isTerminal(sandboxv1alpha1.SwiftSandboxFailed) {
		t.Error("Completed/Failed must be terminal")
	}
	if isTerminal(sandboxv1alpha1.SwiftSandboxRunning) || isTerminal(sandboxv1alpha1.SwiftSandboxPending) {
		t.Error("Running/Pending must not be terminal")
	}
}
