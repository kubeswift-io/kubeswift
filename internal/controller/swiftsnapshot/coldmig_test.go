package swiftsnapshot

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
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

// The pod-level VolumeMode default (nil) is treated as Filesystem by the Job
// builder path; assert the enum sentinel we compare against.
func TestRootDiskBlockSentinel(t *testing.T) {
	if corev1.PersistentVolumeBlock != "Block" {
		t.Fatalf("unexpected Block volumeMode sentinel %q", corev1.PersistentVolumeBlock)
	}
}
