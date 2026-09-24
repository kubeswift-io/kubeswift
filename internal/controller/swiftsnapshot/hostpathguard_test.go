package swiftsnapshot

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftsnapshotwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsnapshot"
)

func localSnap(hp string) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "s1"},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type:  snapshotv1alpha1.SnapshotBackendLocal,
				Local: &snapshotv1alpha1.LocalBackend{HostPath: hp},
			},
		},
	}
}

func TestCheckLocalHostPath_RejectsEscapes(t *testing.T) {
	// spec.backend.local.hostPath is mounted as a hostPath volume into a Job
	// that runs on the node. webhook.enabled defaults to FALSE, so without this
	// controller-side check a tenant could name any path on the host.
	for _, hp := range []string{
		"/",
		"/etc/kubernetes/pki",
		"/var/lib/kubeswift/snapshots/../../../etc",
		"/var/lib/kubeswift/snapshots/..",
		"/root",
		"/var/lib/kubeswift/snapshots/",         // the shared root: rm here wipes every namespace's snapshots on the node
		"/var/lib/kubeswift/snapshots/*",        // glob
		"/var/lib/kubeswift/snapshots/a;rm -rf", // shell metacharacter
		"/var/lib/kubeswift/snapshots/a/b",      // nested path
	} {
		if err := checkLocalHostPath(localSnap(hp)); err == nil {
			t.Errorf("accepted hostPath %q", hp)
		}
	}
}

func TestCheckLocalHostPath_AcceptsTheDerivedDir(t *testing.T) {
	for _, ok := range []string{
		swiftsnapshotwebhook.LocalBackendHostPathPrefix + "tenant_s1",
		swiftsnapshotwebhook.LocalBackendHostPathPrefix + "tenant_s1/",
		"",
	} {
		if err := checkLocalHostPath(localSnap(ok)); err != nil {
			t.Errorf("rejected %q: %v", ok, err)
		}
	}
	s := localSnap("")
	s.Spec.Backend.Local = nil
	if err := checkLocalHostPath(s); err != nil {
		t.Errorf("rejected backend.type=local with no backend.local: %v", err)
	}
}

// A single safe segment is not enough any more: it could be another tenant's
// directory, which the capture empties and a delete removes.
func TestCheckLocalHostPath_RejectsAnotherDirectory(t *testing.T) {
	for _, hp := range []string{
		swiftsnapshotwebhook.LocalBackendHostPathPrefix + "victim_db",
		swiftsnapshotwebhook.LocalBackendHostPathPrefix + "tenant-s1", // the old <ns>-<name> form
		swiftsnapshotwebhook.LocalBackendHostPathPrefix + "shared",
	} {
		if err := checkLocalHostPath(localSnap(hp)); err == nil {
			t.Errorf("accepted %q", hp)
		}
	}
}

// Once a capture has begun its directory is the recorded one and the spec's
// hostPath is not read, so a snapshot an earlier version captured into the
// directory its author chose is not failed after the upgrade.
func TestCheckLocalHostPath_CapturedSnapshotKeepsWorking(t *testing.T) {
	s := localSnap(swiftsnapshotwebhook.LocalBackendHostPathPrefix + "mine")
	s.Status.NodeName = "n1"
	s.Status.MemorySnapshot = &snapshotv1alpha1.MemorySnapshotRef{Handle: swiftsnapshotwebhook.LocalBackendHostPathPrefix + "mine"}
	if err := checkLocalHostPath(s); err != nil {
		t.Errorf("failed a captured snapshot: %v", err)
	}
}

func TestCheckLocalHostPath_IgnoresOtherBackends(t *testing.T) {
	// csi / s3 / oci never mount a host path — the guard must not reject them.
	for _, bt := range []snapshotv1alpha1.SnapshotBackendType{
		snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot,
	} {
		s := localSnap("")
		s.Spec.Backend.Type = bt
		s.Spec.Backend.Local = nil
		if err := checkLocalHostPath(s); err != nil {
			t.Errorf("backend %q wrongly rejected: %v", bt, err)
		}
	}
}
