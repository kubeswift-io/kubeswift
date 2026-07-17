package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func modelSandbox(name, ns, mountPath string) *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image: "alpine:3",
			Model: &sandboxv1alpha1.SandboxModel{
				ImageRef:  "reg.example/llama@sha256:abc",
				MountPath: mountPath,
			},
		},
	}
}

// The model rides a read-only virtio-fs share (tag "sandboxmodel") and the bridge
// is told where to mount it via the cmdline.
func TestBuildIntent_Model_ShareAndCmdline(t *testing.T) {
	sb := modelSandbox("m", "default", "/model")
	ri := buildIntent(sb, "sandbox", "/cache/x.ext4", "/models/sha256-abc/", execSpec{Argv: []string{"/bin/sh"}}, false)

	var found bool
	for _, fs := range ri.Filesystems {
		if fs.Tag == "sandboxmodel" {
			found = true
			if fs.SourcePath != "/models/sha256-abc/" {
				t.Errorf("model share sourcePath = %q, want the resolved tree path", fs.SourcePath)
			}
			if !fs.ReadOnly {
				t.Error("model share must be ReadOnly")
			}
		}
	}
	if !found {
		t.Fatalf("intent must carry the sandboxmodel virtio-fs share, got %+v", ri.Filesystems)
	}
	cmd := ri.KernelBoot.Cmdline
	if !strings.Contains(cmd, "kubeswift.model=virtiofs") {
		t.Errorf("cmdline missing kubeswift.model=virtiofs: %q", cmd)
	}
	if !strings.Contains(cmd, "kubeswift.modelpath=/model") {
		t.Errorf("cmdline missing kubeswift.modelpath=/model: %q", cmd)
	}
}

// A custom mountPath reaches the bridge.
func TestBuildIntent_Model_CustomMountPath(t *testing.T) {
	sb := modelSandbox("m", "default", "/weights")
	ri := buildIntent(sb, "sandbox", "/cache/x.ext4", "/models/d/", execSpec{}, false)
	if !strings.Contains(ri.KernelBoot.Cmdline, "kubeswift.modelpath=/weights") {
		t.Errorf("cmdline must carry the custom mountPath: %q", ri.KernelBoot.Cmdline)
	}
}

// LOAD-BEARING: the model is delivered over virtio-fs, which is NOT a virtio-blk
// device — so it must not shift the config disk's /dev/vdX enumeration. A block
// model disk WOULD have (rootfs=vda, config=vdb); this test locks the property in
// so a future "just make the model a data disk" refactor fails loudly instead of
// silently handing the bridge the wrong config device.
func TestBuildIntent_Model_DoesNotPerturbConfigDisk(t *testing.T) {
	sb := modelSandbox("m", "default", "/model")
	ri := buildIntent(sb, "sandbox", "/cache/x.ext4", "/models/d/", execSpec{Argv: []string{"/bin/sh"}}, false)
	if !strings.Contains(ri.KernelBoot.Cmdline, "kubeswift.config="+sandboxConfigDeviceBlock) {
		t.Errorf("config disk must stay %s with a model attached: %q", sandboxConfigDeviceBlock, ri.KernelBoot.Cmdline)
	}
	if len(ri.DataDisks) != 0 {
		t.Errorf("the model must add NO block data disk, got %+v", ri.DataDisks)
	}
}

// No model -> no share, no cmdline directives (the default path is untouched).
func TestBuildIntent_NoModel(t *testing.T) {
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "default"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "alpine:3"},
	}
	ri := buildIntent(sb, "sandbox", "/cache/x.ext4", "", execSpec{Argv: []string{"/bin/sh"}}, false)
	for _, fs := range ri.Filesystems {
		if fs.Tag == "sandboxmodel" {
			t.Error("no model requested -> no sandboxmodel share")
		}
	}
	if strings.Contains(ri.KernelBoot.Cmdline, "kubeswift.model") {
		t.Errorf("no model requested -> no model cmdline: %q", ri.KernelBoot.Cmdline)
	}
}

// A virtio-fs ROOTFS and a model coexist: two shares, both read-only.
func TestBuildIntent_Model_WithVirtiofsRootfs(t *testing.T) {
	sb := modelSandbox("m", "default", "/model")
	sb.Spec.RootfsMode = sandboxv1alpha1.SandboxRootfsVirtiofs
	ri := buildIntent(sb, "sandbox", "/cache/tree/", "/models/d/", execSpec{}, false)
	tags := map[string]bool{}
	for _, fs := range ri.Filesystems {
		tags[fs.Tag] = fs.ReadOnly
	}
	if ro, ok := tags["sandboxroot"]; !ok || !ro {
		t.Error("virtio-fs rootfs share missing or not read-only")
	}
	if ro, ok := tags["sandboxmodel"]; !ok || !ro {
		t.Error("model share missing or not read-only alongside a virtio-fs rootfs")
	}
}

// A warm keeper (idle) carries the model too — that is the whole point: the
// weights are resident BEFORE a checkout injects a workload.
func TestBuildIntent_Model_OnIdleWarmSlot(t *testing.T) {
	sb := modelSandbox("slot", "default", "/model")
	ri := buildIntent(sb, "sandbox", "/cache/x.ext4", "/models/d/", execSpec{}, true)
	var found bool
	for _, fs := range ri.Filesystems {
		if fs.Tag == "sandboxmodel" {
			found = true
		}
	}
	if !found {
		t.Error("an idle warm slot must still mount the model (resident before checkout)")
	}
	if !strings.Contains(ri.KernelBoot.Cmdline, "kubeswift.idle=1") {
		t.Error("idle slot lost its keeper flag")
	}
}

func TestBuildPod_Model_MaterializeInitAndShare(t *testing.T) {
	sb := modelSandbox("m", "default", "/model")
	pod := buildPod(sb, "sandbox")

	var mi *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == modelMaterializeInitName {
			mi = &pod.Spec.InitContainers[i]
		}
	}
	if mi == nil {
		t.Fatal("pod must carry the model-materialize init container")
	}
	args := strings.Join(mi.Args, " ")
	if !strings.Contains(args, "reg.example/llama@sha256:abc") {
		t.Errorf("model init must pull the model ref, args=%v", mi.Args)
	}
	if !strings.Contains(args, "--mode tree") {
		t.Errorf("model must materialize as a tree (virtio-fs source), args=%v", mi.Args)
	}
	if !strings.Contains(args, modelCacheDir) {
		t.Errorf("model must land in the node model cache, args=%v", mi.Args)
	}
	// The init WRITES the cache; the launcher only reads it.
	if !hasMount(mi, "model-cache", modelCacheDir) {
		t.Error("model init must mount the model cache")
	}
	if !hasVolume(pod, "model-cache") {
		t.Error("pod must carry the model-cache hostPath volume")
	}

	var launcher *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == launcherName {
			launcher = &pod.Spec.Containers[i]
		}
	}
	var ro bool
	for _, m := range launcher.VolumeMounts {
		if m.Name == "model-cache" {
			ro = m.ReadOnly
		}
	}
	// LOAD-BEARING: virtiofsd does NOT enforce read-only and the model has no
	// overlay, so a RW launcher mount would let one checkout corrupt the shared
	// node-cached model for every other slot.
	if !ro {
		t.Error("launcher must mount the model cache READ-ONLY (virtiofsd does not enforce ro)")
	}
}

// No model -> no init container, no volume (the default pod is untouched).
func TestBuildPod_NoModel(t *testing.T) {
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "default"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "alpine:3"},
	}
	pod := buildPod(sb, "sandbox")
	for _, c := range pod.Spec.InitContainers {
		if c.Name == modelMaterializeInitName {
			t.Error("no model requested -> no model-materialize init container")
		}
	}
	if hasVolume(pod, "model-cache") {
		t.Error("no model requested -> no model-cache volume")
	}
}

// The model reuses the rootfs cosign key (same trust domain) and must NOT
// re-declare the shared verify volumes (a duplicate volume name is an invalid pod).
func TestBuildPod_Model_ReusesVerifyKeyWithoutDuplicateVolumes(t *testing.T) {
	sb := modelSandbox("m", "default", "/model")
	sb.Spec.VerifyKeySecretRef = &sandboxv1alpha1.SecretObjectReference{Name: "cosign-key"}
	pod := buildPod(sb, "sandbox")

	seen := map[string]int{}
	for _, v := range pod.Spec.Volumes {
		seen[v.Name]++
	}
	for _, n := range []string{"verify-key", "cosign-home", "model-cache"} {
		if seen[n] != 1 {
			t.Errorf("volume %q declared %d times, want exactly 1", n, seen[n])
		}
	}
	var mi *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == modelMaterializeInitName {
			mi = &pod.Spec.InitContainers[i]
		}
	}
	if !strings.Contains(strings.Join(mi.Args, " "), "--verify-key=/verify-key/cosign.pub") {
		t.Errorf("model must be cosign-verified when a key is set, args=%v", mi.Args)
	}
}

func TestModelMountPath_Default(t *testing.T) {
	if got := (&sandboxv1alpha1.SandboxModel{ImageRef: "x"}).ModelMountPath(); got != "/model" {
		t.Errorf("ModelMountPath() = %q, want /model", got)
	}
	if got := (&sandboxv1alpha1.SandboxModel{ImageRef: "x", MountPath: "/w"}).ModelMountPath(); got != "/w" {
		t.Errorf("ModelMountPath() = %q, want /w", got)
	}
}

// The pool's model must reach every warm slot — that is what makes a checkout
// sub-second (no cold model load).
func TestSlotTemplate_PropagatesModel(t *testing.T) {
	pool := &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: sandboxv1alpha1.SwiftSandboxPoolSpec{
			Image: "alpine:3",
			Model: &sandboxv1alpha1.SandboxModel{ImageRef: "reg.example/llama@sha256:abc", MountPath: "/model"},
		},
	}
	r := &SwiftSandboxPoolReconciler{}
	slot := r.slotTemplate(pool, "p-slot-x")
	if slot.Spec.Model == nil || slot.Spec.Model.ImageRef != "reg.example/llama@sha256:abc" {
		t.Fatalf("warm slot must inherit the pool model, got %+v", slot.Spec.Model)
	}
}
