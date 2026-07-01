package swiftrestore

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

func ociSnapForRestore(withCreds bool) *snapshotv1alpha1.SwiftSnapshot {
	oci := &snapshotv1alpha1.OCIBackend{Repository: "zot.svc:5000/vm-snapshots", Insecure: true}
	if withCreds {
		oci.CredentialsSecretRef = &snapshotv1alpha1.SecretObjectReference{Name: "reg-creds"}
	}
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap1", Namespace: "team-a"},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{Type: snapshotv1alpha1.SnapshotBackendOCI, OCI: oci},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{
			OCI: &snapshotv1alpha1.OCISnapshotStatus{
				Reference:      "zot.svc:5000/vm-snapshots:team-a-snap1",
				ManifestDigest: "sha256:abc123",
			},
		},
	}
}

func ociRestore() *snapshotv1alpha1.SwiftRestore {
	return &snapshotv1alpha1.SwiftRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "team-a"},
		Spec: snapshotv1alpha1.SwiftRestoreSpec{
			SnapshotRef: snapshotv1alpha1.SwiftRestoreSnapshotRef{Name: "snap1"},
			TargetGuest: snapshotv1alpha1.SwiftRestoreTarget{Name: "g1"},
			TargetNode:  "miles",
		},
	}
}

func TestOCIRestoreTag(t *testing.T) {
	if got := ociRestoreTag(ociSnapForRestore(false)); got != "team-a-snap1" {
		t.Errorf("default tag = %q, want team-a-snap1", got)
	}
	s := ociSnapForRestore(false)
	s.Spec.Backend.OCI.Tag = "prod-9"
	if got := ociRestoreTag(s); got != "prod-9" {
		t.Errorf("explicit tag = %q", got)
	}
}

func TestBuildOCIDownloadJob(t *testing.T) {
	job := buildOCIDownloadJob(ociRestore(), ociSnapForRestore(true), "img:tag", "miles")
	if job.Name != "r1-oci-download" || job.Namespace != "team-a" {
		t.Errorf("job meta = %s/%s", job.Namespace, job.Name)
	}
	pod := job.Spec.Template.Spec
	if pod.NodeName != "miles" {
		t.Errorf("not pinned to restore node: %q", pod.NodeName)
	}
	c := pod.Containers[0]
	args := strings.Join(c.Args, " ")
	for _, want := range []string{
		"--mode=download", "--dir=/snap",
		"--repository=zot.svc:5000/vm-snapshots", "--tag=team-a-snap1",
		"--digest=sha256:abc123", "--insecure",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q; got %q", want, args)
		}
	}
	// Runs as root to write the kubelet-created root-owned cache hostPath.
	if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
		t.Errorf("download container must RunAsUser 0")
	}
	// dockerconfigjson creds → DOCKER_CONFIG + oras-auth volume.
	var hasDockerCfg, authVol bool
	for _, e := range c.Env {
		if e.Name == "DOCKER_CONFIG" {
			hasDockerCfg = true
		}
	}
	for _, v := range pod.Volumes {
		if v.Name == "oras-auth" && v.Secret != nil && v.Secret.SecretName == "reg-creds" {
			authVol = true
		}
	}
	if !hasDockerCfg || !authVol {
		t.Errorf("credentialed download must set DOCKER_CONFIG (%v) + mount oras-auth (%v)", hasDockerCfg, authVol)
	}
}

func TestBuildOCIDownloadJob_Anonymous(t *testing.T) {
	job := buildOCIDownloadJob(ociRestore(), ociSnapForRestore(false), "img:tag", "miles")
	pod := job.Spec.Template.Spec
	for _, e := range pod.Containers[0].Env {
		if e.Name == "DOCKER_CONFIG" {
			t.Errorf("anonymous download must not set DOCKER_CONFIG")
		}
	}
	for _, v := range pod.Volumes {
		if v.Name == "oras-auth" {
			t.Errorf("anonymous download must not mount a credential volume")
		}
	}
}

// TestHandlePendingOCI_MissingStatus: an oci restore of a snapshot with no
// status.oci fails loudly (no silent boot from an incomplete push).
func TestHandlePendingOCI_MissingStatus(t *testing.T) {
	r := &SwiftRestoreReconciler{}
	snap := ociSnapForRestore(false)
	snap.Status.OCI = nil // push not completed
	status := &snapshotv1alpha1.SwiftRestoreStatus{}
	advanced, _, err := r.handlePendingOCI(context.Background(), ociRestore(), snap, status)
	if err != nil || !advanced {
		t.Fatalf("advanced=%v err=%v", advanced, err)
	}
	if status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
		t.Errorf("phase = %q, want Failed", status.Phase)
	}
}

// TestIsTierBRestore_IncludesOCI guards the phase-dispatch classifier: oci is a
// memory-snapshot backend and MUST route to the Tier B (CH --restore) restore
// path, not the CSI disk path. A missing oci here sends the restore to
// handleRestoring -> findRootDisk -> "no root disk" on a memory snapshot's empty
// status.disks (the PR 2c cluster-validation bug this asserts against).
func TestIsTierBRestore_IncludesOCI(t *testing.T) {
	cases := []struct {
		backend snapshotv1alpha1.SnapshotBackendType
		want    bool
	}{
		{snapshotv1alpha1.SnapshotBackendLocal, true},
		{snapshotv1alpha1.SnapshotBackendS3, true},
		{snapshotv1alpha1.SnapshotBackendOCI, true},
		{snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot, false},
	}
	for _, c := range cases {
		snap := &snapshotv1alpha1.SwiftSnapshot{
			Spec: snapshotv1alpha1.SwiftSnapshotSpec{
				Backend: snapshotv1alpha1.SwiftSnapshotBackend{Type: c.backend},
			},
		}
		if got := IsTierBRestore(snap); got != c.want {
			t.Errorf("IsTierBRestore(%s) = %v, want %v", c.backend, got, c.want)
		}
	}
}
