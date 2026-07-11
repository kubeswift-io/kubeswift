package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

func TestResolveExec(t *testing.T) {
	cfg := materialize.ImageConfig{
		Entrypoint: []string{"/ep"}, Cmd: []string{"--serve"},
		Env: []string{"PATH=/bin", "A=1"}, WorkingDir: "/img",
	}
	// spec.command overrides ENTRYPOINT, spec.args overrides CMD, spec.env overrides
	// by key + appends, spec.workingDir wins.
	sb := &sandboxv1alpha1.SwiftSandbox{Spec: sandboxv1alpha1.SwiftSandboxSpec{
		Command:    []string{"/bin/sh", "-c"},
		Args:       []string{"echo hi"},
		Env:        []corev1.EnvVar{{Name: "A", Value: "2"}, {Name: "B", Value: "3"}},
		WorkingDir: "/work",
	}}
	e := resolveExec(sb, cfg)
	if strings.Join(e.Argv, " ") != "/bin/sh -c echo hi" {
		t.Errorf("argv = %v", e.Argv)
	}
	if e.Cwd != "/work" {
		t.Errorf("cwd = %q", e.Cwd)
	}
	envs := strings.Join(e.Env, ",")
	if !strings.Contains(envs, "PATH=/bin") || !strings.Contains(envs, "A=2") || !strings.Contains(envs, "B=3") {
		t.Errorf("env = %v (want PATH kept, A overridden to 2, B appended)", e.Env)
	}
	// No spec command: image ENTRYPOINT + CMD; image cwd.
	if e2 := resolveExec(&sandboxv1alpha1.SwiftSandbox{}, cfg); strings.Join(e2.Argv, " ") != "/ep --serve" || e2.Cwd != "/img" {
		t.Errorf("image defaults: argv=%v cwd=%q", e2.Argv, e2.Cwd)
	}
	// command set, no args: the image CMD is suppressed (k8s rule).
	if e3 := resolveExec(&sandboxv1alpha1.SwiftSandbox{Spec: sandboxv1alpha1.SwiftSandboxSpec{Command: []string{"/only"}}}, cfg); strings.Join(e3.Argv, " ") != "/only" {
		t.Errorf("command should suppress image CMD: %v", e3.Argv)
	}
	// bare image, no overrides: empty argv+env+cwd -> not worth a config disk.
	if bare := resolveExec(&sandboxv1alpha1.SwiftSandbox{}, materialize.ImageConfig{}); bare.nonTrivial() {
		t.Errorf("bare spec should be trivial: %+v", bare)
	}
}

func TestBuildIntent(t *testing.T) {
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{CPU: 2, Memory: resource.MustParse("1Gi")},
	}
	intent := buildIntent(sb, "sandbox", "/var/lib/kubeswift/sandbox-rootfs/sha256-abc.ext4", execSpec{Argv: []string{"/bin/sh"}})
	if intent.KernelBoot == nil {
		t.Fatal("kernelBoot nil")
	}
	if !strings.HasSuffix(intent.KernelBoot.KernelPath, "/bzImage") ||
		!strings.HasSuffix(intent.KernelBoot.InitramfsPath, "/rootfs.cpio.gz") {
		t.Errorf("kernel paths: %s / %s", intent.KernelBoot.KernelPath, intent.KernelBoot.InitramfsPath)
	}
	// The exec spec rides the config disk: cmdline points the bridge at it (no argv
	// on the cmdline), and intent.SandboxExec carries the argv.
	if !strings.Contains(intent.KernelBoot.Cmdline, "kubeswift.rootfs=block") ||
		!strings.Contains(intent.KernelBoot.Cmdline, "kubeswift.config=/dev/vdb") {
		t.Errorf("cmdline: %s", intent.KernelBoot.Cmdline)
	}
	if intent.SandboxExec == nil || strings.Join(intent.SandboxExec.Argv, " ") != "/bin/sh" {
		t.Errorf("sandboxExec: %+v", intent.SandboxExec)
	}
	// A networked sandbox gets kernel IP autoconfig so the guest actually DHCPs.
	if !strings.Contains(intent.KernelBoot.Cmdline, "ip=dhcp") {
		t.Errorf("networked sandbox cmdline should include ip=dhcp: %s", intent.KernelBoot.Cmdline)
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
	iNone := buildIntent(sbNone, "sandbox", "/x.ext4", execSpec{Argv: []string{"/bin/sh"}})
	if iNone.Network {
		t.Error("mode=none sandbox must be network-isolated")
	}
	// network:none has no dnsmasq -> ip=dhcp would only stall the boot; it must be absent.
	if strings.Contains(iNone.KernelBoot.Cmdline, "ip=dhcp") {
		t.Errorf("network:none must omit ip=dhcp: %s", iNone.KernelBoot.Cmdline)
	}
	if intent.CPU != 2 || intent.Memory != 1024 {
		t.Errorf("cpu/mem: %d/%d (want 2/1024)", intent.CPU, intent.Memory)
	}
	if intent.Hypervisor != "cloud-hypervisor" {
		t.Errorf("hypervisor: %s", intent.Hypervisor)
	}
	// Trivial exec (bare image, no overrides) -> no config disk, no SandboxExec.
	i2 := buildIntent(sb, "sandbox", "/x.ext4", execSpec{})
	if strings.Contains(i2.KernelBoot.Cmdline, "kubeswift.config") || i2.SandboxExec != nil {
		t.Errorf("trivial exec should omit the config disk: cmdline=%s exec=%+v", i2.KernelBoot.Cmdline, i2.SandboxExec)
	}
}

// The annotation keys are asserted as literals (not via the swiftguest
// constants) on purpose: they pin the wire contract swiftletd hard-codes in
// rust/swiftletd/src/report.rs — the Go side must not drift from it.
func TestApplyGuestAnnotations(t *testing.T) {
	sb := &sandboxv1alpha1.SwiftSandbox{}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		"kubeswift.io/guest-runtime-pid": "4242",
		"kubeswift.io/guest-hypervisor":  "cloud-hypervisor",
		"kubeswift.io/guest-ip":          "192.168.99.12",
	}}}
	applyGuestAnnotations(sb, pod)
	if sb.Status.Runtime == nil || sb.Status.Runtime.PID != 4242 || sb.Status.Runtime.Hypervisor != "cloud-hypervisor" {
		t.Fatalf("runtime not mapped: %+v", sb.Status.Runtime)
	}
	if sb.Status.Network == nil || sb.Status.Network.PrimaryIP != "192.168.99.12" {
		t.Fatalf("network not mapped: %+v", sb.Status.Network)
	}
	if msg := guestRunningMessage(sb); !strings.Contains(msg, "pid 4242") || !strings.Contains(msg, "192.168.99.12") {
		t.Errorf("guestRunningMessage = %q", msg)
	}

	// No annotations (e.g. network:none, or before socket-ready) -> untouched,
	// message falls back to the launcher-readiness proxy.
	none := &sandboxv1alpha1.SwiftSandbox{}
	applyGuestAnnotations(none, &corev1.Pod{})
	if none.Status.Runtime != nil || none.Status.Network != nil {
		t.Errorf("no annotations should leave status untouched: %+v %+v", none.Status.Runtime, none.Status.Network)
	}
	if msg := guestRunningMessage(none); msg != "launcher ready (guest starting)" {
		t.Errorf("fallback message = %q", msg)
	}
}

func TestBuildPod_LauncherReportEnv(t *testing.T) {
	sb := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"}}
	pod := buildPod(sb, "sandbox")
	var launcher *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == launcherName {
			launcher = &pod.Spec.Containers[i]
		}
	}
	if launcher == nil {
		t.Fatal("no launcher container")
	}
	env := map[string]corev1.EnvVar{}
	for _, e := range launcher.Env {
		env[e.Name] = e
	}
	// Without POD_NAME/POD_NAMESPACE (downward API) swiftletd skips the report +
	// lease paths entirely, so pid/hypervisor/IP never reach the sandbox status.
	for _, name := range []string{"POD_NAME", "POD_NAMESPACE"} {
		e, ok := env[name]
		if !ok || e.ValueFrom == nil || e.ValueFrom.FieldRef == nil {
			t.Errorf("%s must be a downward-API fieldRef, got %+v (present=%v)", name, e, ok)
		}
	}
	if fr := env["POD_NAME"].ValueFrom; fr != nil && fr.FieldRef.FieldPath != "metadata.name" {
		t.Errorf("POD_NAME fieldPath = %q", fr.FieldRef.FieldPath)
	}
	// Sandbox has no SwiftGuest CR to patch -> suppress that report path.
	if env["KUBESWIFT_REPORT_GUEST_CR"].Value != "false" {
		t.Errorf("KUBESWIFT_REPORT_GUEST_CR = %q, want false", env["KUBESWIFT_REPORT_GUEST_CR"].Value)
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
