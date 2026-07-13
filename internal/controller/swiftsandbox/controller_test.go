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
	intent := buildIntent(sb, "sandbox", "/var/lib/kubeswift/sandbox-rootfs/sha256-abc.ext4", execSpec{Argv: []string{"/bin/sh"}}, false)
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
	// ...and the k8s search domains for its namespace (ip=dhcp doesn't capture the
	// DHCP search list, so cluster-internal short names would otherwise NXDOMAIN).
	if !strings.Contains(intent.KernelBoot.Cmdline, "kubeswift.dns-search=default.svc.cluster.local,svc.cluster.local,cluster.local") {
		t.Errorf("networked sandbox cmdline should include the namespace search domains: %s", intent.KernelBoot.Cmdline)
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
	iNone := buildIntent(sbNone, "sandbox", "/x.ext4", execSpec{Argv: []string{"/bin/sh"}}, false)
	if iNone.Network {
		t.Error("mode=none sandbox must be network-isolated")
	}
	// network:none has no dnsmasq -> ip=dhcp (and the DNS search domains) would only
	// stall the boot / be meaningless; both must be absent.
	if strings.Contains(iNone.KernelBoot.Cmdline, "ip=dhcp") || strings.Contains(iNone.KernelBoot.Cmdline, "kubeswift.dns-search=") {
		t.Errorf("network:none must omit ip=dhcp: %s", iNone.KernelBoot.Cmdline)
	}
	if intent.CPU != 2 || intent.Memory != 1024 {
		t.Errorf("cpu/mem: %d/%d (want 2/1024)", intent.CPU, intent.Memory)
	}
	if intent.Hypervisor != "cloud-hypervisor" {
		t.Errorf("hypervisor: %s", intent.Hypervisor)
	}
	// Trivial exec (bare image, no overrides) -> no config disk, no SandboxExec.
	i2 := buildIntent(sb, "sandbox", "/x.ext4", execSpec{}, false)
	if strings.Contains(i2.KernelBoot.Cmdline, "kubeswift.config") || i2.SandboxExec != nil {
		t.Errorf("trivial exec should omit the config disk: cmdline=%s exec=%+v", i2.KernelBoot.Cmdline, i2.SandboxExec)
	}
	// A warm-pool keeper (idle) boots to the bridge idle loop with NO workload: the
	// cmdline carries kubeswift.idle=1 and NO config disk, even if an exec is passed.
	keeper := buildIntent(sb, "sandbox", "/x.ext4", execSpec{Argv: []string{"sleep", "infinity"}}, true)
	if !strings.Contains(keeper.KernelBoot.Cmdline, "kubeswift.idle=1") {
		t.Errorf("keeper cmdline missing kubeswift.idle=1: %s", keeper.KernelBoot.Cmdline)
	}
	if strings.Contains(keeper.KernelBoot.Cmdline, "kubeswift.config") || keeper.SandboxExec != nil {
		t.Errorf("keeper must carry no workload/config disk: cmdline=%s exec=%+v", keeper.KernelBoot.Cmdline, keeper.SandboxExec)
	}
	// A real workload (idle=false) never gets the idle flag.
	if strings.Contains(intent.KernelBoot.Cmdline, "kubeswift.idle") {
		t.Errorf("non-keeper cmdline should not carry kubeswift.idle: %s", intent.KernelBoot.Cmdline)
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

func TestBuildPod_VerifyKeyMount(t *testing.T) {
	// No verifyKeySecretRef: the materialize init container has no --verify-key arg
	// and no verify-key volume.
	plain := buildPod(&sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"}}, "sandbox")
	if hasVerifyKeyArg(matInit(t, plain)) {
		t.Error("no verifyKeySecretRef but --verify-key present")
	}
	if hasVolume(plain, "verify-key") {
		t.Error("no verifyKeySecretRef but verify-key volume present")
	}

	// With verifyKeySecretRef: the arg, the ro secret mount, the cosign-home
	// emptyDir, and the HOME/TMPDIR env are all wired onto the materialize init
	// container so cosign has a writable home.
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{VerifyKeySecretRef: &sandboxv1alpha1.SecretObjectReference{Name: "cosign-pub"}},
	}
	pod := buildPod(sb, "sandbox")
	init := matInit(t, pod)
	if !hasVerifyKeyArg(init) {
		t.Error("verifyKeySecretRef set but --verify-key missing from materialize args")
	}
	if !hasMount(init, "verify-key", "/verify-key") || !hasMount(init, "cosign-home", "/cosign-home") {
		t.Errorf("verify-key/cosign-home mounts missing: %+v", init.VolumeMounts)
	}
	env := map[string]string{}
	for _, e := range init.Env {
		env[e.Name] = e.Value
	}
	if env["HOME"] != "/cosign-home" || env["TMPDIR"] != "/cosign-home" {
		t.Errorf("cosign HOME/TMPDIR env not set: %+v", init.Env)
	}
	// The secret volume must project cosign.pub -> cosign.pub from the named Secret.
	var vk *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "verify-key" {
			vk = &pod.Spec.Volumes[i]
		}
	}
	if vk == nil || vk.Secret == nil || vk.Secret.SecretName != "cosign-pub" {
		t.Fatalf("verify-key volume not a Secret projection of cosign-pub: %+v", vk)
	}
	if len(vk.Secret.Items) != 1 || vk.Secret.Items[0].Key != "cosign.pub" {
		t.Errorf("verify-key must project the cosign.pub key: %+v", vk.Secret.Items)
	}
}

func matInit(t *testing.T, pod *corev1.Pod) *corev1.Container {
	t.Helper()
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == materializeInitName {
			return &pod.Spec.InitContainers[i]
		}
	}
	t.Fatal("no sandbox-materialize init container")
	return nil
}

func hasVerifyKeyArg(c *corev1.Container) bool {
	for _, a := range c.Args {
		if a == "--verify-key=/verify-key/cosign.pub" {
			return true
		}
	}
	return false
}

func hasMount(c *corev1.Container, name, path string) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == name && m.MountPath == path {
			return true
		}
	}
	return false
}

func hasVolume(pod *corev1.Pod, name string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func TestBuildIntent_VirtiofsRootfs(t *testing.T) {
	tree := "/var/lib/kubeswift/sandbox-rootfs/sha256-abc"

	// block (default): SandboxRootfs carries the ext4 path, no filesystems,
	// cmdline kubeswift.rootfs=block, config disk at /dev/vdb.
	blk := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"}}
	bi := buildIntent(blk, "sandbox", "/cache/x.ext4", execSpec{Argv: []string{"/bin/sh"}}, false)
	if bi.SandboxRootfs == nil || bi.SandboxRootfs.Virtiofs || bi.SandboxRootfs.Path != "/cache/x.ext4" {
		t.Errorf("block SandboxRootfs = %+v", bi.SandboxRootfs)
	}
	if len(bi.Filesystems) != 0 {
		t.Errorf("block must have no filesystems: %+v", bi.Filesystems)
	}
	if !strings.Contains(bi.KernelBoot.Cmdline, "kubeswift.rootfs=block") ||
		!strings.Contains(bi.KernelBoot.Cmdline, "kubeswift.config=/dev/vdb") {
		t.Errorf("block cmdline = %q", bi.KernelBoot.Cmdline)
	}

	// virtiofs: SandboxRootfs is the marker (Virtiofs=true, no block path), the
	// tree rides a sandboxroot filesystems share, cmdline kubeswift.rootfs=virtiofs,
	// and the config disk moves to /dev/vda (no block rootfs precedes it).
	vfs := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "ns"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{RootfsMode: sandboxv1alpha1.SandboxRootfsVirtiofs},
	}
	vi := buildIntent(vfs, "sandbox", tree, execSpec{Argv: []string{"/bin/sh"}}, false)
	if vi.SandboxRootfs == nil || !vi.SandboxRootfs.Virtiofs || vi.SandboxRootfs.Path != "" {
		t.Errorf("virtiofs SandboxRootfs = %+v", vi.SandboxRootfs)
	}
	if len(vi.Filesystems) != 1 || vi.Filesystems[0].Tag != "sandboxroot" || vi.Filesystems[0].SourcePath != tree {
		t.Fatalf("virtiofs filesystems = %+v", vi.Filesystems)
	}
	if !vi.Filesystems[0].ReadOnly {
		t.Error("virtiofs rootfs share must be read-only")
	}
	if !strings.Contains(vi.KernelBoot.Cmdline, "kubeswift.rootfs=virtiofs") ||
		!strings.Contains(vi.KernelBoot.Cmdline, "kubeswift.config=/dev/vda") {
		t.Errorf("virtiofs cmdline = %q", vi.KernelBoot.Cmdline)
	}

	// The materialize --mode follows the rootfs mode.
	if got := sandboxRootfsMode(blk); got != "block" {
		t.Errorf("block mode = %q", got)
	}
	if got := sandboxRootfsMode(vfs); got != "tree" {
		t.Errorf("virtiofs mode = %q", got)
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
