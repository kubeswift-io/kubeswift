package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func sandboxWithOutputs() *sandboxv1alpha1.SwiftSandbox {
	sb := plainSandbox("busybox:1")
	sb.UID = "sb-uid"
	sb.Spec.Outputs = []sandboxv1alpha1.SandboxOutput{
		{Name: "compiled", Path: "/output", MaxSize: ptr.To(resource.MustParse("256Mi"))},
		{Name: "index", Path: "/index", MaxEntries: ptr.To(int64(500))},
	}
	return sb
}

func TestBuildIntent_Outputs(t *testing.T) {
	sb := sandboxWithOutputs()
	ri := buildIntent(sb, "sandbox", "/var/lib/kubeswift/sandbox-rootfs/x.ext4", "", execSpec{}, false)
	if !strings.Contains(ri.KernelBoot.Cmdline, " kubeswift.outputs=sbxout0:/output,sbxout1:/index") {
		t.Errorf("cmdline %q lacks the output mounts", ri.KernelBoot.Cmdline)
	}
	if len(ri.Outputs) != 2 || ri.Outputs[0].Tag != "sbxout0" || ri.Outputs[0].MaxBytes != 256<<20 ||
		ri.Outputs[1].MaxBytes != sandboxv1alpha1.DefaultOutputMaxSize.Value() || ri.Outputs[1].MaxEntries != 500 {
		t.Errorf("outputs = %+v", ri.Outputs)
	}
	for _, fs := range ri.Filesystems {
		if strings.HasPrefix(fs.Tag, outputTagPrefix) {
			t.Errorf("an output share is in the intent's filesystems (%+v): swiftletd makes it", fs)
		}
	}
	// A warm slot never has one.
	idle := buildIntent(sb, "sandbox", "/x.ext4", "", execSpec{}, true)
	if len(idle.Outputs) != 0 || strings.Contains(idle.KernelBoot.Cmdline, "kubeswift.outputs") {
		t.Errorf("a warm slot got outputs: %+v", idle.Outputs)
	}
	// No outputs: nothing changes.
	plain := buildIntent(plainSandbox("busybox:1"), "sandbox", "/x.ext4", "", execSpec{}, false)
	if len(plain.Outputs) != 0 || strings.Contains(plain.KernelBoot.Cmdline, "kubeswift.outputs") {
		t.Error("a sandbox without outputs got output plumbing")
	}
}

func TestBuildPod_OutputVolumes(t *testing.T) {
	pod := buildPod(sandboxWithOutputs(), "sandbox")
	var staging, spool *corev1.Volume
	for i := range pod.Spec.Volumes {
		switch pod.Spec.Volumes[i].Name {
		case outputStagingVolume:
			staging = &pod.Spec.Volumes[i]
		case outputSpoolVolume:
			spool = &pod.Spec.Volumes[i]
		}
	}
	if staging == nil || staging.EmptyDir == nil || staging.EmptyDir.SizeLimit == nil {
		t.Fatalf("no sized staging emptyDir: %+v", staging)
	}
	want := OutputImageSize(256<<20) + OutputImageSize(sandboxv1alpha1.DefaultOutputMaxSize.Value())
	if staging.EmptyDir.SizeLimit.Value() != want {
		t.Errorf("staging sizeLimit %d, want %d", staging.EmptyDir.SizeLimit.Value(), want)
	}
	// Only this sandbox's spool directory, never the parent.
	if spool == nil || spool.HostPath == nil || spool.HostPath.Path != OutputSpoolDir+"/sb-uid" {
		t.Errorf("spool volume = %+v, want hostPath %s/sb-uid", spool, OutputSpoolDir)
	}
	c := pod.Spec.Containers[0]
	if _, ok := c.Resources.Requests[corev1.ResourceEphemeralStorage]; !ok {
		t.Error("no ephemeral-storage request for the staging and spool disk")
	}
	if plain := buildPod(plainSandbox("busybox:1"), "sandbox"); len(plain.Spec.Containers[0].Resources.Requests) != 0 {
		t.Errorf("a sandbox without outputs got requests: %+v", plain.Spec.Containers[0].Resources.Requests)
	}
}

func TestSlotMismatches_Outputs(t *testing.T) {
	sb := sandboxWithOutputs()
	found := false
	pool := &sandboxv1alpha1.SwiftSandboxPool{Spec: sandboxv1alpha1.SwiftSandboxPoolSpec{Image: sb.Spec.Image, Memory: sb.Spec.Memory}}
	for _, m := range slotMismatches(pool, sb) {
		if strings.HasPrefix(m, "outputs") {
			found = true
		}
	}
	if !found {
		t.Error("a sandbox with outputs would take a warm slot")
	}
}
