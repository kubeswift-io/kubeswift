package swiftsnapshot

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// ociSnapPushed is a full-state oci snapshot as it is at deletion time: its
// memory, disk and one data disk pushed, carrying the cleanup finalizer.
func ociSnapPushed() *snapshotv1alpha1.SwiftSnapshot {
	s := ociSnap(nil)
	s.UID = "snap-uid"
	s.Finalizers = []string{OCIArtifactFinalizer}
	s.Status.OCI = &snapshotv1alpha1.OCISnapshotStatus{
		Reference: "zot.svc:5000/vm-snapshots:team-a-snap1",
		Disk:      &snapshotv1alpha1.OCIDiskArtifact{Reference: "zot.svc:5000/vm-snapshots:team-a-snap1-disk"},
		DataDisks: []snapshotv1alpha1.OCIDataDiskArtifact{{Name: "data", Reference: "zot.svc:5000/vm-snapshots:team-a-snap1-data"}},
	}
	return s
}

func ociDeleteJobWith(snap *snapshotv1alpha1.SwiftSnapshot, cond batchv1.JobConditionType) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: ociDeleteJobName(snap), Namespace: snap.Namespace}}
	j.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue}}
	return j
}

func TestEnsureFinalizer_OCIBackend_Adds(t *testing.T) {
	snap := ociSnap(nil)
	r, c := newReconciler(t, snap)
	if err := r.ensureFinalizer(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if !hasFin(t, c, snap, OCIArtifactFinalizer) {
		t.Error("oci snapshot should get OCIArtifactFinalizer: deletionPolicy Delete was never honored")
	}
}

// Every pushed artifact is deleted, each by its own tag.
func TestHandleOCIDeletion_DeletesEveryPushedArtifact(t *testing.T) {
	snap := ociSnapPushed()
	r, c := newReconciler(t, snap)
	r.SnapshotORASImage = "ghcr.io/x/snapshot-oras:t"
	done, err := r.handleOCIDeletion(context.Background(), snap)
	if err != nil || done {
		t.Fatalf("first pass should create the delete Job; done=%v err=%v", done, err)
	}
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Name: ociDeleteJobName(snap), Namespace: snap.Namespace}, &job); err != nil {
		t.Fatalf("delete Job not created: %v", err)
	}
	var tags []string
	for _, ct := range job.Spec.Template.Spec.Containers {
		args := strings.Join(ct.Args, " ")
		if !strings.Contains(args, "--mode=delete") || !strings.Contains(args, "--repository=zot.svc:5000/vm-snapshots") {
			t.Errorf("container %s args = %v", ct.Name, ct.Args)
		}
		for _, a := range ct.Args {
			if tag, ok := strings.CutPrefix(a, "--tag="); ok {
				tags = append(tags, tag)
			}
		}
	}
	want := "team-a-snap1,team-a-snap1-disk,team-a-snap1-data"
	if strings.Join(tags, ",") != want {
		t.Errorf("tags deleted = %v, want %s", tags, want)
	}
	if !hasFin(t, c, snap, OCIArtifactFinalizer) {
		t.Error("finalizer must stay while the delete runs")
	}
}

func TestHandleOCIDeletion_CompleteOrFailedReleasesTheSnapshot(t *testing.T) {
	for _, cond := range []batchv1.JobConditionType{batchv1.JobComplete, batchv1.JobFailed} {
		snap := ociSnapPushed()
		r, c := newReconciler(t, snap, ociDeleteJobWith(snap, cond))
		r.SnapshotORASImage = "img"
		done, err := r.handleOCIDeletion(context.Background(), snap)
		if err != nil || !done {
			t.Fatalf("%s: done=%v err=%v", cond, done, err)
		}
		// A registry that refuses deletes must not hold the snapshot (and its
		// namespace) Terminating forever.
		if hasFin(t, c, snap, OCIArtifactFinalizer) {
			t.Errorf("%s: finalizer should be removed", cond)
		}
	}
}

func TestHandleOCIDeletion_RetainDeletesNothing(t *testing.T) {
	snap := ociSnapPushed()
	snap.Spec.DeletionPolicy = snapshotv1alpha1.SnapshotDeletionPolicyRetain
	r, c := newReconciler(t, snap)
	r.SnapshotORASImage = "img"
	if done, err := r.handleOCIDeletion(context.Background(), snap); err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Error("Retain must not delete the artifact")
	}
}

// The capture node keeps a full copy of the guest's RAM under the snapshot
// directory; it was never removed. It is cleaned before the finalizer goes.
func TestS3Deletion_CleansTheCaptureNodeCopyFirst(t *testing.T) {
	snap := s3SnapReady("snap1", "team-a")
	snap.Status.NodeName = "worker-1"
	r, c := newReconciler(t, snap, deleteJobWith(snap, batchv1.JobComplete))
	r.SnapshotS3Image = "img"
	ctx := context.Background()

	if done, err := r.handleS3Deletion(ctx, snap); err != nil || done {
		t.Fatalf("first pass should start the node cleanup; done=%v err=%v", done, err)
	}
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Name: cleanupPodName(snap), Namespace: snap.Namespace}, &pod); err != nil {
		t.Fatalf("capture-node cleanup pod not created: %v", err)
	}
	if pod.Spec.NodeName != "worker-1" || strings.Join(pod.Spec.Containers[0].Args, " ") != HostPathBaseMount+"/team-a-snap1" {
		t.Errorf("cleanup pod node=%s args=%v", pod.Spec.NodeName, pod.Spec.Containers[0].Args)
	}
	if !hasFin(t, c, snap, S3ObjectFinalizer) {
		t.Fatal("finalizer must stay until the node copy is gone")
	}

	pod.Status.Phase = corev1.PodSucceeded
	if err := c.Status().Update(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	if done, err := r.handleS3Deletion(ctx, snap); err != nil || !done {
		t.Fatalf("after the node cleanup and the purge, deletion is done; done=%v err=%v", done, err)
	}
}

func TestSplitOCIReference(t *testing.T) {
	for ref, want := range map[string]ociArtifact{
		"zot.svc:5000/vm/snaps:t1": {"zot.svc:5000/vm/snaps", "t1"},
		"ghcr.io/org/repo:v-2":     {"ghcr.io/org/repo", "v-2"},
	} {
		got, ok := splitOCIReference(ref)
		if !ok || got != want {
			t.Errorf("%s -> %+v (ok=%v), want %+v", ref, got, ok, want)
		}
	}
	for _, bad := range []string{"", "zot.svc:5000/vm/snaps", "repo:"} {
		if _, ok := splitOCIReference(bad); ok {
			t.Errorf("%q should not split", bad)
		}
	}
}
