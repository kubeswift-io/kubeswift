package swiftrestore

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

func s3ReadySnap(name, ns, sourceGuest string) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: sourceGuest},
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendS3,
				S3: &snapshotv1alpha1.S3Backend{
					Bucket:               "backups",
					Prefix:               "kubeswift",
					Region:               "us-east-1",
					CredentialsSecretRef: &snapshotv1alpha1.SecretObjectReference{Name: "s3-creds"},
				},
			},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{
			Phase:    snapshotv1alpha1.SwiftSnapshotPhaseReady,
			NodeName: "miles",
			S3:       &snapshotv1alpha1.S3SnapshotStatus{Location: "s3://backups/kubeswift/team-a/snap1/"},
		},
	}
}

func s3Restore(name, ns, snapName, target, targetNode string) *snapshotv1alpha1.SwiftRestore {
	return &snapshotv1alpha1.SwiftRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftRestoreSpec{
			SnapshotRef: snapshotv1alpha1.SwiftRestoreSnapshotRef{Name: snapName},
			TargetGuest: snapshotv1alpha1.SwiftRestoreTarget{Name: target, OverwriteExisting: true},
			TargetNode:  targetNode,
		},
	}
}

func TestS3Restore_Helpers(t *testing.T) {
	snap := s3ReadySnap("snap1", "team-a", "g1")
	restore := s3Restore("r1", "team-a", "snap1", "g1", "boba")
	if got := s3RestoreLocalDir(snap); got != "/var/lib/kubeswift/snapshots/team-a-snap1" {
		t.Errorf("localDir = %q", got)
	}
	if got := s3RestoreKeyPrefix(snap); got != "kubeswift/team-a/snap1" {
		t.Errorf("keyPrefix = %q", got)
	}
	if got := s3DownloadJobName(restore); got != "r1-s3-download" {
		t.Errorf("jobName = %q", got)
	}
}

func TestResolveS3RestoreNode(t *testing.T) {
	snap := s3ReadySnap("snap1", "ns", "g1")

	t.Run("targetNode wins", func(t *testing.T) {
		r, _ := newReconciler(t)
		node, failMsg, err := r.resolveS3RestoreNode(context.Background(), s3Restore("r", "ns", "snap1", "g1", "boba"), snap)
		if err != nil || failMsg != "" || node != "boba" {
			t.Fatalf("node=%q failMsg=%q err=%v", node, failMsg, err)
		}
	})

	t.Run("in-place falls back to guest node", func(t *testing.T) {
		guest := &swiftv1alpha1.SwiftGuest{
			ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "ns"},
			Status:     swiftv1alpha1.SwiftGuestStatus{NodeName: "miles"},
		}
		r, _ := newReconciler(t, guest)
		node, failMsg, err := r.resolveS3RestoreNode(context.Background(), s3Restore("r", "ns", "snap1", "g1", ""), snap)
		if err != nil || failMsg != "" || node != "miles" {
			t.Fatalf("node=%q failMsg=%q err=%v", node, failMsg, err)
		}
	})

	t.Run("clone without targetNode -> failMsg", func(t *testing.T) {
		r, _ := newReconciler(t)
		node, failMsg, err := r.resolveS3RestoreNode(context.Background(), s3Restore("r", "ns", "snap1", "clone-a", ""), snap)
		if err != nil || node != "" || !strings.Contains(failMsg, "requires spec.targetNode") {
			t.Fatalf("node=%q failMsg=%q err=%v", node, failMsg, err)
		}
	})

	t.Run("guest with no node yet -> failMsg", func(t *testing.T) {
		guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "ns"}}
		r, _ := newReconciler(t, guest)
		_, failMsg, err := r.resolveS3RestoreNode(context.Background(), s3Restore("r", "ns", "snap1", "g1", ""), snap)
		if err != nil || !strings.Contains(failMsg, "no assigned node yet") {
			t.Fatalf("failMsg=%q err=%v", failMsg, err)
		}
	})
}

func TestBuildDownloadJob(t *testing.T) {
	snap := s3ReadySnap("snap1", "team-a", "g1")
	snap.Spec.Backend.S3.Endpoint = "minio.svc:9000"
	snap.Spec.Backend.S3.ForcePathStyle = true
	snap.Spec.IncludeMemory = true
	job := buildDownloadJob(s3Restore("r1", "team-a", "snap1", "g1", "boba"), snap, "ghcr.io/x/snapshot-s3:t", "boba")
	pod := job.Spec.Template.Spec

	if pod.NodeName != "boba" {
		t.Errorf("download Job must pin to the resolved node; got %q", pod.NodeName)
	}
	if pod.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Errorf("restartPolicy = %q, want OnFailure", pod.RestartPolicy)
	}
	c := pod.Containers[0]

	// Cache mounted READ-WRITE (download writes artifacts), DirectoryOrCreate.
	vm := c.VolumeMounts[0]
	if vm.MountPath != s3DownloadMount || vm.ReadOnly {
		t.Errorf("cache must mount %s read-write; got %+v", s3DownloadMount, vm)
	}
	hp := pod.Volumes[0].VolumeSource.HostPath
	if hp == nil || hp.Path != "/var/lib/kubeswift/snapshots/team-a-snap1" || *hp.Type != corev1.HostPathDirectoryOrCreate {
		t.Errorf("hostPath = %+v (want DirectoryOrCreate at the cache dir)", hp)
	}

	// args carry download mode + derived prefix + flags.
	a := strings.Join(c.Args, " ")
	for _, want := range []string{"--mode=download", "--bucket=backups", "--key-prefix=kubeswift/team-a/snap1",
		"--region=us-east-1", "--endpoint=minio.svc:9000", "--path-style", "--include-memory"} {
		if !strings.Contains(a, want) {
			t.Errorf("args missing %q; got %q", want, a)
		}
	}

	// creds from the snapshot's Secret as AWS env.
	gotKeys := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			if e.ValueFrom.SecretKeyRef.Name != "s3-creds" {
				t.Errorf("env %s must source from the creds Secret; got %q", e.Name, e.ValueFrom.SecretKeyRef.Name)
			}
			gotKeys[e.Name] = e.ValueFrom.SecretKeyRef.Key
		}
	}
	if gotKeys["AWS_ACCESS_KEY_ID"] != "accessKeyId" || gotKeys["AWS_SECRET_ACCESS_KEY"] != "secretAccessKey" {
		t.Errorf("creds env mapping wrong: %+v", gotKeys)
	}

	// Runs as root (writes the root-owned hostPath) but otherwise hardened.
	sc := c.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 0 ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("download container must run as root with drop ALL / no-priv-esc / ro-rootfs; got %+v", sc)
	}
}

func TestHandlePendingS3(t *testing.T) {
	t.Run("uploaded + targetNode -> Downloading + Job", func(t *testing.T) {
		snap := s3ReadySnap("snap1", "ns", "g1")
		r, c := newReconciler(t, snap)
		r.SnapshotS3Image = "img"
		restore := s3Restore("r1", "ns", "snap1", "clone-a", "boba")
		status := restore.Status.DeepCopy()
		advanced, _, err := r.handlePendingS3(context.Background(), restore, snap, status)
		if err != nil || !advanced {
			t.Fatalf("advanced=%v err=%v", advanced, err)
		}
		if status.Phase != snapshotv1alpha1.SwiftRestorePhaseDownloading {
			t.Errorf("phase = %s, want Downloading", status.Phase)
		}
		var job batchv1.Job
		if err := c.Get(context.Background(), client.ObjectKey{Name: "r1-s3-download", Namespace: "ns"}, &job); err != nil {
			t.Fatalf("download Job not created: %v", err)
		}
	})

	t.Run("not uploaded -> Failed", func(t *testing.T) {
		snap := s3ReadySnap("snap1", "ns", "g1")
		snap.Status.S3 = nil
		r, _ := newReconciler(t, snap)
		r.SnapshotS3Image = "img"
		status := &snapshotv1alpha1.SwiftRestoreStatus{}
		advanced, _, err := r.handlePendingS3(context.Background(), s3Restore("r1", "ns", "snap1", "g1", "boba"), snap, status)
		if err != nil || !advanced || status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
			t.Fatalf("advanced=%v phase=%s err=%v", advanced, status.Phase, err)
		}
	})

	t.Run("clone without targetNode -> Failed", func(t *testing.T) {
		snap := s3ReadySnap("snap1", "ns", "g1")
		r, _ := newReconciler(t, snap)
		r.SnapshotS3Image = "img"
		status := &snapshotv1alpha1.SwiftRestoreStatus{}
		advanced, _, err := r.handlePendingS3(context.Background(), s3Restore("r1", "ns", "snap1", "clone-a", ""), snap, status)
		if err != nil || !advanced || status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
			t.Fatalf("advanced=%v phase=%s err=%v", advanced, status.Phase, err)
		}
	})
}

func downloadJobWith(restore *snapshotv1alpha1.SwiftRestore, cond batchv1.JobConditionType) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: s3DownloadJobName(restore), Namespace: restore.Namespace}}
	if cond != "" {
		j.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, Message: "boom"}}
	}
	return j
}

func TestHandleDownloading(t *testing.T) {
	snap := s3ReadySnap("snap1", "ns", "g1")
	restore := s3Restore("r1", "ns", "snap1", "clone-a", "boba")

	t.Run("failed -> errMsg", func(t *testing.T) {
		r, _ := newReconciler(t, snap, downloadJobWith(restore, batchv1.JobFailed))
		_, _, errMsg, err := r.handleDownloading(context.Background(), restore, &snapshotv1alpha1.SwiftRestoreStatus{})
		if err != nil || !strings.Contains(errMsg, "download Job failed") {
			t.Fatalf("errMsg=%q err=%v", errMsg, err)
		}
	})

	t.Run("running -> requeue", func(t *testing.T) {
		r, _ := newReconciler(t, snap, downloadJobWith(restore, ""))
		advanced, requeue, errMsg, err := r.handleDownloading(context.Background(), restore, &snapshotv1alpha1.SwiftRestoreStatus{})
		if err != nil || advanced || errMsg != "" || requeue == 0 {
			t.Fatalf("advanced=%v requeue=%v errMsg=%q err=%v", advanced, requeue, errMsg, err)
		}
	})

	t.Run("job missing -> recreate", func(t *testing.T) {
		r, c := newReconciler(t, snap)
		r.SnapshotS3Image = "img"
		advanced, _, errMsg, err := r.handleDownloading(context.Background(), restore, &snapshotv1alpha1.SwiftRestoreStatus{})
		if err != nil || advanced || errMsg != "" {
			t.Fatalf("advanced=%v errMsg=%q err=%v", advanced, errMsg, err)
		}
		var job batchv1.Job
		if err := c.Get(context.Background(), client.ObjectKey{Name: s3DownloadJobName(restore), Namespace: "ns"}, &job); err != nil {
			t.Fatalf("download Job not recreated: %v", err)
		}
	})
}
