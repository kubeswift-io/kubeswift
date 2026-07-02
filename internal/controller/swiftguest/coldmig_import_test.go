package swiftguest

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// fullStateCloneSnap is an oci snapshot carrying status.oci.disk — a full-state
// (suspended-state) snapshot whose disk was chunked to the registry alongside
// its memory artifact by the includeDisk capture-then-terminate flow (PR 1b).
func fullStateCloneSnap() *snapshotv1alpha1.SwiftSnapshot {
	s := ociCloneSnap() // ns/snap, guestRef src, repo zot.svc:5000/vmsnap, insecure
	s.Status.OCI.Disk = &snapshotv1alpha1.OCIDiskArtifact{
		Reference:      "zot.svc:5000/vmsnap:ns-snap-disk",
		ManifestDigest: "sha256:disk123",
		PushedBytes:    4096,
	}
	return s
}

func fsRG() *resolved.ResolvedGuest {
	return &resolved.ResolvedGuest{
		RootDisk: resolved.RootDisk{Size: resource.MustParse("40Gi")},
		Storage:  resolved.Storage{AccessMode: "ReadWriteOnce", VolumeMode: "Filesystem"},
	}
}

func TestBuildDiskFromOCIJob_Filesystem(t *testing.T) {
	guest := cloneGuest()
	job := buildDiskFromOCIJob(guest, fullStateCloneSnap(), "oras:img", "miles", "j", "swiftguest-root-clone-a", "sha256:disk123", false)
	pod := job.Spec.Template.Spec
	if pod.NodeName != "miles" {
		t.Errorf("download Job must pin to the clone node; got %q", pod.NodeName)
	}
	c := pod.Containers[0]
	args := strings.Join(c.Args, " ")
	for _, want := range []string{
		"--mode=download-image",
		"--file=" + DisksRootPath + "/image.raw",
		"--repository=zot.svc:5000/vmsnap",
		"--digest=sha256:disk123",
		"--insecure",
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
		if m.Name == "rootdisk" && m.MountPath == DisksRootPath {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("Filesystem root must mount the clone PVC at %s; got %+v", DisksRootPath, c.VolumeMounts)
	}
	if len(pod.Volumes) == 0 || pod.Volumes[0].PersistentVolumeClaim == nil ||
		pod.Volumes[0].PersistentVolumeClaim.ClaimName != "swiftguest-root-clone-a" {
		t.Errorf("rootdisk volume not backed by the clone PVC: %+v", pod.Volumes)
	}
	if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
		t.Errorf("download container must RunAsUser 0 to write the raw disk")
	}
}

func TestBuildDiskFromOCIJob_Block(t *testing.T) {
	job := buildDiskFromOCIJob(cloneGuest(), fullStateCloneSnap(), "oras:img", "boba", "j", "swiftguest-root-clone-a", "sha256:disk123", true)
	c := job.Spec.Template.Spec.Containers[0]
	if !strings.Contains(strings.Join(c.Args, " "), "--file="+DiskRootDevicePath) {
		t.Errorf("Block root must download to the raw device; got %q", strings.Join(c.Args, " "))
	}
	if len(c.VolumeMounts) != 0 {
		t.Errorf("Block root must not filesystem-mount the PVC; got %+v", c.VolumeMounts)
	}
	var dev bool
	for _, d := range c.VolumeDevices {
		if d.Name == "rootdisk" && d.DevicePath == DiskRootDevicePath {
			dev = true
		}
	}
	if !dev {
		t.Errorf("Block root must attach the PVC as a device at %s; got %+v", DiskRootDevicePath, c.VolumeDevices)
	}
}

func TestBuildDiskFromOCIJob_Creds(t *testing.T) {
	snap := fullStateCloneSnap()
	snap.Spec.Backend.OCI.CredentialsSecretRef = &snapshotv1alpha1.SecretObjectReference{Name: "reg-creds"}
	job := buildDiskFromOCIJob(cloneGuest(), snap, "oras:img", "miles", "j", "swiftguest-root-clone-a", "sha256:disk123", false)
	c := job.Spec.Template.Spec.Containers[0]
	var dockerCfg bool
	for _, e := range c.Env {
		if e.Name == "DOCKER_CONFIG" && e.Value == "/oras-auth" {
			dockerCfg = true
		}
	}
	if !dockerCfg {
		t.Errorf("DOCKER_CONFIG env missing on the download Job")
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

// A cloneFromSnapshot whose snapshot has NO status.oci.disk is a normal
// (memory-only) clone — maybeRootDiskFromOCI must decline so EnsureRootDiskClone
// falls through to the base-image clone path.
func TestMaybeRootDiskFromOCI_NotFullState(t *testing.T) {
	g := cloneGuest()
	g.Spec.CloneFromSnapshot.TargetNode = "boba"
	r, _ := newCloneReconciler(t, g, ociCloneSnap()) // no status.oci.disk
	r.SnapshotORASImage = "img"
	handled, res, err := r.maybeRootDiskFromOCI(context.Background(), g, fsRG())
	if handled || res != nil || err != nil {
		t.Fatalf("non-full-state clone must not be handled; handled=%v res=%v err=%v", handled, res, err)
	}
}

// A guest with no cloneFromSnapshot at all is declined immediately.
func TestMaybeRootDiskFromOCI_NoClone(t *testing.T) {
	g := cloneGuest()
	g.Spec.CloneFromSnapshot = nil
	r, _ := newCloneReconciler(t)
	handled, _, err := r.maybeRootDiskFromOCI(context.Background(), g, fsRG())
	if handled || err != nil {
		t.Fatalf("a non-clone guest must not be handled; handled=%v err=%v", handled, err)
	}
}

func TestMaybeRootDiskFromOCI_MaterializesDiskThenReady(t *testing.T) {
	ctx := context.Background()
	g := cloneGuest()
	g.Spec.CloneFromSnapshot.TargetNode = "boba"
	r, c := newCloneReconciler(t, g, fullStateCloneSnap())
	r.SnapshotORASImage = "img"
	cloneName := RootDiskCloneName(g.Name)

	// Pass 1: creates the RestoreSeeded clone PVC + requeues.
	handled, res, err := r.maybeRootDiskFromOCI(ctx, g, fsRG())
	if !handled || res != nil || err == nil {
		t.Fatalf("pass 1 should create the PVC + requeue; handled=%v res=%v err=%v", handled, res, err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Name: cloneName, Namespace: "ns"}, &pvc); err != nil {
		t.Fatalf("clone PVC not created: %v", err)
	}
	if pvc.Labels[RestoreSeededLabel] != "true" {
		t.Errorf("clone PVC must be RestoreSeeded so EnsureRootDiskClone skips the copy path; labels=%+v", pvc.Labels)
	}
	if len(pvc.OwnerReferences) == 0 || pvc.OwnerReferences[0].Name != g.Name {
		t.Errorf("clone PVC must be owned by the guest; ownerRefs=%+v", pvc.OwnerReferences)
	}

	// Bind the PVC (the provisioner would); pass 2 creates the download Job.
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(ctx, &pvc); err != nil {
		t.Fatal(err)
	}
	handled, res, err = r.maybeRootDiskFromOCI(ctx, g, fsRG())
	if !handled || res != nil || err == nil {
		t.Fatalf("pass 2 should create the download Job + requeue; handled=%v res=%v err=%v", handled, res, err)
	}
	var job batchv1.Job
	jobName := cloneName + "-oci-disk-dl"
	if err := c.Get(ctx, client.ObjectKey{Name: jobName, Namespace: "ns"}, &job); err != nil {
		t.Fatalf("download Job not created: %v", err)
	}
	if job.Spec.Template.Spec.NodeName != "boba" {
		t.Errorf("download Job must pin to targetNode boba; got %q", job.Spec.Template.Spec.NodeName)
	}
	if !strings.Contains(strings.Join(job.Spec.Template.Spec.Containers[0].Args, " "), "--digest=sha256:disk123") {
		t.Errorf("download Job must pull the disk artifact by digest; args=%v", job.Spec.Template.Spec.Containers[0].Args)
	}

	// Complete the Job; pass 3 returns the Bound clone as the root disk.
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, &job); err != nil {
		t.Fatal(err)
	}
	handled, res, err = r.maybeRootDiskFromOCI(ctx, g, fsRG())
	if !handled || err != nil || res == nil {
		t.Fatalf("pass 3 should return the materialized disk; handled=%v res=%v err=%v", handled, res, err)
	}
	if res.PVCName != cloneName {
		t.Errorf("result PVC = %q, want %q", res.PVCName, cloneName)
	}
	if res.NeedsGrowInit {
		t.Errorf("a full-state disk is byte-exact; it must NOT be grow-init'd")
	}
}

// fullStateWithDataDisk is a v1.1 full-state snapshot: a root disk PLUS one Block
// data disk artifact + its captured shape.
func fullStateWithDataDisk() *snapshotv1alpha1.SwiftSnapshot {
	s := fullStateCloneSnap()
	s.Status.OCI.DataDisks = []snapshotv1alpha1.OCIDataDiskArtifact{
		{Name: "scratch", Reference: "zot.svc:5000/vmsnap:ns-snap-disk-scratch", ManifestDigest: "sha256:dd1", PushedBytes: 8192},
	}
	s.Status.GuestSpec = &snapshotv1alpha1.CapturedGuestSpec{
		HasDataDisks: true,
		DataDisks:    []snapshotv1alpha1.CapturedDataDisk{{Name: "scratch", Size: "20Gi", Block: true}},
	}
	return s
}

// A full-state clone whose source had a data disk must materialize that disk into
// its own RestoreSeeded PVC and OVERRIDE rg.DataDisks so the launcher attaches the
// clone-owned PVC (not the source's). The root disk is only reported done once the
// data disk is Bound + its download Job Complete.
func TestMaybeRootDiskFromOCI_MaterializesDataDisks(t *testing.T) {
	ctx := context.Background()
	g := cloneGuest()
	g.Spec.CloneFromSnapshot.TargetNode = "boba"
	r, c := newCloneReconciler(t, g, fullStateWithDataDisk())
	r.SnapshotORASImage = "img"
	cloneName := RootDiskCloneName(g.Name)
	dataPVC := resolved.BlankDataDiskPVCName(g.Name, "scratch")

	bind := func(name string) {
		var p corev1.PersistentVolumeClaim
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: "ns"}, &p); err != nil {
			return
		}
		p.Status.Phase = corev1.ClaimBound
		_ = c.Status().Update(ctx, &p)
	}
	completeJob := func(name string) {
		var j batchv1.Job
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: "ns"}, &j); err != nil {
			return
		}
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		_ = c.Status().Update(ctx, &j)
	}

	rg := fsRG()
	// Iterate the reconcile, binding/completing PVCs+Jobs as they appear, until
	// the root disk is reported ready (or we give up).
	var res *RootDiskCloneResult
	for i := 0; i < 8 && res == nil; i++ {
		_, r2, _ := r.maybeRootDiskFromOCI(ctx, g, rg)
		res = r2
		bind(cloneName)
		bind(dataPVC)
		completeJob(cloneName + "-oci-disk-dl")
		completeJob(dataPVC + "-oci-dl")
	}
	if res == nil {
		t.Fatalf("root disk never reported ready with a data disk in flight")
	}

	// The data-disk PVC must exist, be RestoreSeeded + guest-owned, Block, 20Gi.
	var ddpvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Name: dataPVC, Namespace: "ns"}, &ddpvc); err != nil {
		t.Fatalf("data-disk clone PVC not created: %v", err)
	}
	if ddpvc.Labels[RestoreSeededLabel] != "true" || ddpvc.Labels["swift.kubeswift.io/role"] != "data-disk" {
		t.Errorf("data-disk PVC labels wrong: %+v", ddpvc.Labels)
	}
	if ddpvc.Spec.VolumeMode == nil || *ddpvc.Spec.VolumeMode != corev1.PersistentVolumeBlock {
		t.Errorf("captured Block data disk must clone as Block; got %+v", ddpvc.Spec.VolumeMode)
	}
	if got := ddpvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "20Gi" {
		t.Errorf("data-disk size = %q, want 20Gi", got.String())
	}

	// rg.DataDisks must be OVERRIDDEN to the clone-owned PVC, under the source name.
	if len(rg.DataDisks) != 1 {
		t.Fatalf("rg.DataDisks not overridden: %+v", rg.DataDisks)
	}
	d := rg.DataDisks[0]
	if d.Name != "scratch" || d.PVCName != dataPVC || !d.Block || !d.Ready {
		t.Errorf("overridden data disk wrong: %+v (want name=scratch pvc=%s block ready)", d, dataPVC)
	}
}
