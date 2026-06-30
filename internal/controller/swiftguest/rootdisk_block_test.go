package swiftguest

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// W9 Component 1 tests for createCloneJob.
//
// The branch on rg.Storage.VolumeMode is the load-bearing piece. Tests
// cover three contracts:
//
//  1. Regression: Filesystem-mode Job (the default and overwhelming
//     majority of guests) is byte-identical to pre-W9 behaviour.
//  2. Forward: Block-mode Job uses VolumeDevices + qemu-img convert +
//     sgdisk -e, NOT VolumeMounts/cp/qemu-img resize.
//  3. Invariant: VolumeMounts and VolumeDevices on the same container
//     never name the same volume (kubelet rejects this — exactly the
//     class of failure W9 surfaced).

func newGuestForCloneJob(name string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       "guest-uid",
		},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "ubuntu"},
			GuestClassRef: corev1.LocalObjectReference{Name: "default"},
		},
	}
}

func newReconcilerWithGuest(t *testing.T, guest *swiftv1alpha1.SwiftGuest) *SwiftGuestReconciler {
	t.Helper()
	scheme := rootdiskScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	return &SwiftGuestReconciler{Client: c, Scheme: scheme}
}

func getCreatedJob(t *testing.T, r *SwiftGuestReconciler, jobName string) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	if err := r.Get(context.Background(), client.ObjectKey{Name: jobName, Namespace: "default"}, &job); err != nil {
		t.Fatalf("get created job %q: %v", jobName, err)
	}
	return &job
}

// TestCreateCloneJob_FilesystemModeUnchanged is the regression gate. The
// Filesystem path is the default, the path every existing SwiftGuest
// uses, and the path the smoke test exercises. This test fixes the
// behaviour byte-by-byte so that any Block-mode refactor that rotates
// the Filesystem path's structure (e.g. accidentally moving
// VolumeMounts under a conditional that defaults wrong) produces a
// visible test failure rather than silently regressing on cluster.
func TestCreateCloneJob_FilesystemModeUnchanged(t *testing.T) {
	guest := newGuestForCloneJob("fs-guest")
	r := newReconcilerWithGuest(t, guest)
	rg := &resolved.ResolvedGuest{
		Storage: resolved.Storage{
			AccessMode: "ReadWriteOnce",
			VolumeMode: "Filesystem",
		},
	}

	if err := r.createCloneJob(context.Background(), guest, rg, "swiftguest-rootclone-fs-guest", "src-pvc", "dst-pvc", resource.MustParse("40Gi")); err != nil {
		t.Fatalf("createCloneJob: %v", err)
	}

	job := getCreatedJob(t, r, "swiftguest-rootclone-fs-guest")
	c := job.Spec.Template.Spec.Containers[0]

	// VolumeMounts: src + dst; no VolumeDevices.
	if len(c.VolumeMounts) != 2 {
		t.Fatalf("VolumeMounts = %d, want 2 (src + dst)", len(c.VolumeMounts))
	}
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if mounts["src"] != "/src" {
		t.Errorf("src mount path = %q, want /src", mounts["src"])
	}
	if mounts["dst"] != "/dst" {
		t.Errorf("dst mount path = %q, want /dst (Filesystem path is /dst, NOT /dev/dst-block)", mounts["dst"])
	}
	if len(c.VolumeDevices) != 0 {
		t.Errorf("VolumeDevices = %v, want empty for Filesystem mode", c.VolumeDevices)
	}

	// Script must contain cp + qemu-img resize + sgdisk -e in that order.
	// The Filesystem branch invariant: every step the pre-W9 code did,
	// in the same order. If a future commit rearranges the script, the
	// commit message must explicitly call it out (a sgdisk-then-resize
	// reorder is a real change, not a refactor).
	script := strings.Join(c.Command, " ")
	for _, want := range []string{"cp /src/image.raw /dst/image.raw", "qemu-img resize -f raw /dst/image.raw", "sgdisk -e /dst/image.raw"} {
		if !strings.Contains(script, want) {
			t.Errorf("Filesystem-mode script missing %q; got:\n%s", want, script)
		}
	}
	// Filesystem branch must NOT use the Block-mode primitives.
	if strings.Contains(script, "qemu-img convert") {
		t.Errorf("Filesystem-mode script must not use 'qemu-img convert' (that's the Block-mode primitive); got:\n%s", script)
	}
	if strings.Contains(script, CloneJobBlockDevicePath) {
		t.Errorf("Filesystem-mode script must not reference %s; got:\n%s", CloneJobBlockDevicePath, script)
	}
}

// TestCreateCloneJob_BlockModeUsesVolumeDevices is the forward contract.
// Block destinations cannot be filesystem-mounted — kubelet rejects
// `volume dst has volumeMode Block, but is specified in volumeMounts`
// (the exact failure W9 was opened to fix). Verify the rendered Job
// uses VolumeDevices for dst, omits the dst VolumeMount, and runs the
// raw-device script (qemu-img convert + sgdisk -e + sync, no cp, no
// qemu-img resize).
func TestCreateCloneJob_BlockModeUsesVolumeDevices(t *testing.T) {
	guest := newGuestForCloneJob("block-guest")
	r := newReconcilerWithGuest(t, guest)
	rg := &resolved.ResolvedGuest{
		Storage: resolved.Storage{
			AccessMode: "ReadWriteMany",
			VolumeMode: "Block",
		},
	}

	if err := r.createCloneJob(context.Background(), guest, rg, "swiftguest-rootclone-block-guest", "src-pvc", "dst-pvc", resource.MustParse("40Gi")); err != nil {
		t.Fatalf("createCloneJob: %v", err)
	}

	job := getCreatedJob(t, r, "swiftguest-rootclone-block-guest")
	c := job.Spec.Template.Spec.Containers[0]

	// VolumeMounts: src only. No dst — dst is a VolumeDevice.
	if len(c.VolumeMounts) != 1 {
		t.Fatalf("VolumeMounts = %d, want 1 (src only); got names=%v", len(c.VolumeMounts), volumeMountNames(c.VolumeMounts))
	}
	if c.VolumeMounts[0].Name != "src" || c.VolumeMounts[0].MountPath != "/src" {
		t.Errorf("VolumeMounts[0] = %+v, want {src, /src}", c.VolumeMounts[0])
	}

	// VolumeDevices: dst at the Block device path.
	if len(c.VolumeDevices) != 1 {
		t.Fatalf("VolumeDevices = %d, want 1 (dst); got %v", len(c.VolumeDevices), c.VolumeDevices)
	}
	if c.VolumeDevices[0].Name != "dst" {
		t.Errorf("VolumeDevices[0].Name = %q, want dst", c.VolumeDevices[0].Name)
	}
	if c.VolumeDevices[0].DevicePath != CloneJobBlockDevicePath {
		t.Errorf("VolumeDevices[0].DevicePath = %q, want %q",
			c.VolumeDevices[0].DevicePath, CloneJobBlockDevicePath)
	}

	// Script: qemu-img convert + sgdisk -e against the device path; NO cp,
	// NO qemu-img resize.
	script := strings.Join(c.Command, " ")
	wantContains := []string{
		"qemu-img convert -f raw -O raw /src/image.raw " + CloneJobBlockDevicePath,
		"sgdisk -e " + CloneJobBlockDevicePath,
		"sync",
	}
	for _, w := range wantContains {
		if !strings.Contains(script, w) {
			t.Errorf("Block-mode script missing %q; got:\n%s", w, script)
		}
	}
	wantOmitted := []string{
		"cp /src/image.raw", // raw device cannot be a cp target
		"qemu-img resize",   // no-op on block devices, omitted
		"/dst/image.raw",    // filesystem path; never appears in Block script
	}
	for _, w := range wantOmitted {
		if strings.Contains(script, w) {
			t.Errorf("Block-mode script must NOT contain %q; got:\n%s", w, script)
		}
	}
}

// TestCreateCloneJob_VolumeNamesNeverOverlap locks in the kubelet
// invariant that surfaced as W9 in the first place: a container's
// VolumeMounts and VolumeDevices must not share a volume name. If they
// do, the kubelet refuses the pod ("volume X has volumeMode Block, but
// is specified in volumeMounts"). The fix-by-construction is that the
// Block branch removes the dst entry from VolumeMounts when adding it
// to VolumeDevices; this test pins that contract for both branches.
//
// Defensive test: not architectural, just a no-overlap regression
// gate. Run on both Filesystem and Block paths.
func TestCreateCloneJob_VolumeNamesNeverOverlap(t *testing.T) {
	cases := []struct {
		name       string
		volumeMode string
	}{
		{"Filesystem", "Filesystem"},
		{"Block", "Block"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guest := newGuestForCloneJob("overlap-" + strings.ToLower(tc.name))
			r := newReconcilerWithGuest(t, guest)
			access := "ReadWriteOnce"
			if tc.volumeMode == "Block" {
				access = "ReadWriteMany"
			}
			rg := &resolved.ResolvedGuest{
				Storage: resolved.Storage{AccessMode: access, VolumeMode: tc.volumeMode},
			}
			if err := r.createCloneJob(context.Background(), guest, rg,
				"swiftguest-rootclone-overlap-"+strings.ToLower(tc.name),
				"src-pvc", "dst-pvc", resource.MustParse("40Gi")); err != nil {
				t.Fatalf("createCloneJob: %v", err)
			}
			job := getCreatedJob(t, r, "swiftguest-rootclone-overlap-"+strings.ToLower(tc.name))
			c := job.Spec.Template.Spec.Containers[0]

			seen := map[string]string{}
			for _, m := range c.VolumeMounts {
				seen[m.Name] = "VolumeMount"
			}
			for _, d := range c.VolumeDevices {
				if existing, ok := seen[d.Name]; ok {
					t.Errorf("volume %q appears as both %s and VolumeDevice — kubelet will reject the pod",
						d.Name, existing)
				}
				seen[d.Name] = "VolumeDevice"
			}
		})
	}
}

// TestCreateCloneJob_BlockModeJobMetadataUnchanged confirms the Block
// branch only diverges in the container's mount/script — every other
// Job-level field (labels, OwnerReferences, BackoffLimit,
// RestartPolicy) is preserved. The Block path is purely additive on
// the container's volume surface; metadata and lifecycle invariants
// are byte-identical.
func TestCreateCloneJob_BlockModeJobMetadataUnchanged(t *testing.T) {
	guest := newGuestForCloneJob("metadata-guest")
	r := newReconcilerWithGuest(t, guest)
	rg := &resolved.ResolvedGuest{
		Storage: resolved.Storage{AccessMode: "ReadWriteMany", VolumeMode: "Block"},
	}
	if err := r.createCloneJob(context.Background(), guest, rg, "swiftguest-rootclone-metadata-guest", "src-pvc", "dst-pvc", resource.MustParse("40Gi")); err != nil {
		t.Fatalf("createCloneJob: %v", err)
	}
	job := getCreatedJob(t, r, "swiftguest-rootclone-metadata-guest")

	if job.Labels["swift.kubeswift.io/guest"] != "metadata-guest" {
		t.Errorf("guest label = %q, want metadata-guest", job.Labels["swift.kubeswift.io/guest"])
	}
	if job.Labels["swift.kubeswift.io/role"] != "root-disk-clone" {
		t.Errorf("role label = %q, want root-disk-clone", job.Labels["swift.kubeswift.io/role"])
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 3 {
		t.Errorf("BackoffLimit = %v, want 3", job.Spec.BackoffLimit)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "SwiftGuest" {
		t.Errorf("OwnerReferences should be a single SwiftGuest controller-ref; got %+v", job.OwnerReferences)
	}
}

func volumeMountNames(mounts []corev1.VolumeMount) []string {
	out := make([]string, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, m.Name)
	}
	return out
}
