package swiftsnapshot

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func TestOCIDiskTagAndReference(t *testing.T) {
	s := ociSnap(nil) // team-a/snap1, repo zot.svc:5000/vm-snapshots
	if got := ociDiskTag(s); got != "team-a-snap1-disk" {
		t.Errorf("disk tag = %q, want team-a-snap1-disk", got)
	}
	if got := ociDiskReference(s); got != "zot.svc:5000/vm-snapshots:team-a-snap1-disk" {
		t.Errorf("disk reference = %q", got)
	}
}

func TestBuildDiskChunkJob_Filesystem(t *testing.T) {
	job := buildDiskChunkJob(ociSnap(nil), "oras:img", "miles", false)
	pod := job.Spec.Template.Spec
	if pod.NodeName != "miles" {
		t.Errorf("disk chunk Job must be pinned to the capture node; got %q", pod.NodeName)
	}
	c := pod.Containers[0]
	args := strings.Join(c.Args, " ")
	for _, want := range []string{
		"--mode=upload-image",
		"--file=/rootdisk/image.raw",
		"--repository=zot.svc:5000/vm-snapshots",
		"--tag=team-a-snap1-disk",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q; got %q", want, args)
		}
	}
	// Filesystem root: mounted, not a raw device.
	if len(c.VolumeDevices) != 0 {
		t.Errorf("Filesystem root must not use volumeDevices; got %+v", c.VolumeDevices)
	}
	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.Name == "rootdisk" && m.MountPath == "/rootdisk" {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("Filesystem root must mount the PVC at /rootdisk; got %+v", c.VolumeMounts)
	}
	// Volume from the guest's root PVC.
	if len(pod.Volumes) == 0 || pod.Volumes[0].PersistentVolumeClaim == nil ||
		pod.Volumes[0].PersistentVolumeClaim.ClaimName != "swiftguest-root-g1" {
		t.Errorf("rootdisk volume not backed by swiftguest-root-g1: %+v", pod.Volumes)
	}
	if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
		t.Errorf("disk chunk container must RunAsUser 0 to read the raw disk")
	}
}

func TestBuildDiskChunkJob_Block(t *testing.T) {
	job := buildDiskChunkJob(ociSnap(nil), "oras:img", "boba", true)
	c := job.Spec.Template.Spec.Containers[0]
	if !strings.Contains(strings.Join(c.Args, " "), "--file=/dev/kubeswift-root") {
		t.Errorf("Block root must chunk the raw device; got %q", strings.Join(c.Args, " "))
	}
	if len(c.VolumeMounts) != 0 {
		t.Errorf("Block root must not filesystem-mount the PVC; got %+v", c.VolumeMounts)
	}
	var dev bool
	for _, d := range c.VolumeDevices {
		if d.Name == "rootdisk" && d.DevicePath == "/dev/kubeswift-root" {
			dev = true
		}
	}
	if !dev {
		t.Errorf("Block root must attach the PVC as a device at /dev/kubeswift-root; got %+v", c.VolumeDevices)
	}
}

func TestBuildDiskChunkJob_InsecureAndCreds(t *testing.T) {
	snap := ociSnap(func(o *snapshotv1alpha1.OCIBackend) {
		o.Insecure = true
		o.CredentialsSecretRef = &snapshotv1alpha1.SecretObjectReference{Name: "reg-creds"}
	})
	job := buildDiskChunkJob(snap, "oras:img", "miles", false)
	c := job.Spec.Template.Spec.Containers[0]
	if !strings.Contains(strings.Join(c.Args, " "), "--insecure") {
		t.Errorf("--insecure must be present; got %q", strings.Join(c.Args, " "))
	}
	var dockerCfg bool
	for _, e := range c.Env {
		if e.Name == "DOCKER_CONFIG" && e.Value == ociAuthMount {
			dockerCfg = true
		}
	}
	if !dockerCfg {
		t.Errorf("DOCKER_CONFIG env missing on the disk chunk Job")
	}
	var authVol bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "oras-auth" && v.Secret != nil && v.Secret.SecretName == "reg-creds" {
			authVol = true
		}
	}
	if !authVol {
		t.Errorf("oras-auth credential volume missing")
	}
}

// stopSourceGuest is capture-then-terminate step 0: flip the source runPolicy to
// Stopped BEFORE the launcher Delete so the SwiftGuest controller doesn't
// resurrect it (split-brain / disk-coherency-race fix). Must be idempotent and
// tolerant of a source that is already stopped or already gone.
func TestStopSourceGuest_PatchesRunningToStopped(t *testing.T) {
	g := makeGuest("team-a", "g1")
	g.Spec.RunPolicy = swiftv1alpha1.RunPolicyRunning
	r, c := newReconciler(t, g)
	if err := r.stopSourceGuest(context.Background(), "team-a", "g1"); err != nil {
		t.Fatalf("stopSourceGuest: %v", err)
	}
	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "team-a"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.RunPolicy != swiftv1alpha1.RunPolicyStopped {
		t.Errorf("source runPolicy = %q, want Stopped (else the launcher resurrects)", got.Spec.RunPolicy)
	}
}

func TestStopSourceGuest_AlreadyStopped_NoOp(t *testing.T) {
	g := makeGuest("team-a", "g1")
	g.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	r, _ := newReconciler(t, g)
	if err := r.stopSourceGuest(context.Background(), "team-a", "g1"); err != nil {
		t.Fatalf("idempotent re-entry on an already-Stopped source must not error: %v", err)
	}
}

func TestStopSourceGuest_SourceGone_NoOp(t *testing.T) {
	r, _ := newReconciler(t) // no source guest
	if err := r.stopSourceGuest(context.Background(), "team-a", "nonexistent"); err != nil {
		t.Fatalf("a missing source must be a no-op, not an error: %v", err)
	}
}

// The pod-level VolumeMode default (nil) is treated as Filesystem by the Job
// builder path; assert the enum sentinel we compare against.
func TestRootDiskBlockSentinel(t *testing.T) {
	if corev1.PersistentVolumeBlock != "Block" {
		t.Fatalf("unexpected Block volumeMode sentinel %q", corev1.PersistentVolumeBlock)
	}
}
