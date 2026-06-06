package swiftsnapshot

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

// s3SnapReady returns an s3 snapshot carrying S3ObjectFinalizer + a populated
// status.S3 (i.e. it was uploaded), as it would be at deletion time.
func s3SnapReady(name, ns string) *snapshotv1alpha1.SwiftSnapshot {
	s := s3Snap(name, ns, nil)
	s.Finalizers = []string{S3ObjectFinalizer}
	s.Status.S3 = &snapshotv1alpha1.S3SnapshotStatus{Location: s3Location(s)}
	return s
}

func deleteJobWith(snap *snapshotv1alpha1.SwiftSnapshot, cond batchv1.JobConditionType) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: s3DeleteJobName(snap), Namespace: snap.Namespace}}
	if cond != "" {
		j.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue}}
	}
	return j
}

func hasFin(t *testing.T, c client.Client, snap *snapshotv1alpha1.SwiftSnapshot, fin string) bool {
	t.Helper()
	var got snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(snap), &got); err != nil {
		t.Fatal(err)
	}
	for _, f := range got.Finalizers {
		if f == fin {
			return true
		}
	}
	return false
}

func TestEnsureFinalizer_S3Backend_Adds(t *testing.T) {
	snap := s3Snap("snap1", "team-a", nil)
	r, c := newReconciler(t, snap)
	if err := r.ensureFinalizer(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if !hasFin(t, c, snap, S3ObjectFinalizer) {
		t.Error("s3 snapshot should get S3ObjectFinalizer")
	}
}

func TestBuildDeleteJob(t *testing.T) {
	snap := s3Snap("snap1", "team-a", nil)
	job := buildDeleteJob(snap, "ghcr.io/x/snapshot-s3:t")
	if job.Name != "snap1-s3-delete" {
		t.Errorf("name = %q", job.Name)
	}
	ct := job.Spec.Template.Spec.Containers[0]
	args := strings.Join(ct.Args, " ")
	for _, want := range []string{"--mode=delete", "--bucket=backups", "--key-prefix=kubeswift/team-a/snap1"} {
		if !strings.Contains(args, want) {
			t.Errorf("delete args missing %q: %v", want, ct.Args)
		}
	}
	// No volumes/mounts (pure S3), node-agnostic, non-root.
	if len(job.Spec.Template.Spec.Volumes) != 0 || len(ct.VolumeMounts) != 0 {
		t.Error("delete Job must mount nothing")
	}
	if job.Spec.Template.Spec.NodeName != "" {
		t.Error("delete Job must be node-agnostic")
	}
	if sc := ct.SecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 65534 || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("delete Job must run non-root as 65534; got %+v", ct.SecurityContext)
	}
	// Credentials come from the referenced Secret via env.
	if len(ct.Env) == 0 || ct.Env[0].ValueFrom == nil || ct.Env[0].ValueFrom.SecretKeyRef.Name != "s3-creds" {
		t.Errorf("delete Job must take creds from the Secret env; got %+v", ct.Env)
	}
}

func TestHandleS3Deletion_CreatesDeleteJob(t *testing.T) {
	snap := s3SnapReady("snap1", "team-a")
	r, c := newReconciler(t, snap)
	r.SnapshotS3Image = "img"
	done, err := r.handleS3Deletion(context.Background(), snap)
	if err != nil || done {
		t.Fatalf("first pass should create the delete Job + requeue; done=%v err=%v", done, err)
	}
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Name: s3DeleteJobName(snap), Namespace: snap.Namespace}, &job); err != nil {
		t.Fatalf("delete Job not created: %v", err)
	}
	if !hasFin(t, c, snap, S3ObjectFinalizer) {
		t.Error("finalizer must be retained while the delete Job runs")
	}
}

func TestHandleS3Deletion_JobComplete_RemovesFinalizer(t *testing.T) {
	snap := s3SnapReady("snap1", "team-a")
	r, c := newReconciler(t, snap, deleteJobWith(snap, batchv1.JobComplete))
	r.SnapshotS3Image = "img"
	done, err := r.handleS3Deletion(context.Background(), snap)
	if err != nil || !done {
		t.Fatalf("complete delete Job should remove the finalizer; done=%v err=%v", done, err)
	}
	if hasFin(t, c, snap, S3ObjectFinalizer) {
		t.Error("finalizer must be removed after the purge completes")
	}
}

func TestHandleS3Deletion_JobFailed_RetainsFinalizer(t *testing.T) {
	snap := s3SnapReady("snap1", "team-a")
	r, c := newReconciler(t, snap, deleteJobWith(snap, batchv1.JobFailed))
	r.SnapshotS3Image = "img"
	done, err := r.handleS3Deletion(context.Background(), snap)
	if err != nil || done {
		t.Fatalf("a failed delete Job must leave the finalizer for visibility; done=%v err=%v", done, err)
	}
	if !hasFin(t, c, snap, S3ObjectFinalizer) {
		t.Error("finalizer must be retained on a failed purge")
	}
}

func TestHandleS3Deletion_NothingToPurge_DropsFinalizer(t *testing.T) {
	// status.S3 nil (never uploaded) -> drop finalizer, no Job.
	snap := s3Snap("snap1", "team-a", nil)
	snap.Finalizers = []string{S3ObjectFinalizer}
	r, c := newReconciler(t, snap)
	r.SnapshotS3Image = "img"
	done, err := r.handleS3Deletion(context.Background(), snap)
	if err != nil || !done {
		t.Fatalf("never-uploaded snapshot should drop the finalizer; done=%v err=%v", done, err)
	}
	if hasFin(t, c, snap, S3ObjectFinalizer) {
		t.Error("finalizer should be dropped when there is nothing to purge")
	}
}

func TestHandleS3Deletion_NoImage_DropsFinalizer(t *testing.T) {
	// Can't purge without the image -> drop the finalizer rather than wedge
	// namespace deletion forever (the finalizer-trap lesson).
	snap := s3SnapReady("snap1", "team-a")
	r, c := newReconciler(t, snap)
	r.SnapshotS3Image = "" // unconfigured
	done, err := r.handleS3Deletion(context.Background(), snap)
	if err != nil || !done {
		t.Fatalf("missing image should drop the finalizer, not wedge; done=%v err=%v", done, err)
	}
	if hasFin(t, c, snap, S3ObjectFinalizer) {
		t.Error("finalizer should be dropped when the snapshot-s3 image is unconfigured")
	}
}

// TestHandleDeletion_DispatchesS3 verifies the top-level dispatcher routes an
// s3 snapshot to the S3 path (creates the delete Job).
func TestHandleDeletion_DispatchesS3(t *testing.T) {
	snap := s3SnapReady("snap1", "team-a")
	r, c := newReconciler(t, snap)
	r.SnapshotS3Image = "img"
	if _, err := r.handleDeletion(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Name: s3DeleteJobName(snap), Namespace: snap.Namespace}, &job); err != nil {
		t.Errorf("handleDeletion should route s3 backend to the delete-Job path: %v", err)
	}
}
