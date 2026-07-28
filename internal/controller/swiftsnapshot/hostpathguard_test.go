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
	} {
		if err := checkLocalHostPath(localSnap(hp)); err == nil {
			t.Errorf("accepted hostPath %q", hp)
		}
	}
}

func TestCheckLocalHostPath_AcceptsThePermittedPrefix(t *testing.T) {
	ok := swiftsnapshotwebhook.LocalBackendHostPathPrefix + "tenant-s1"
	if err := checkLocalHostPath(localSnap(ok)); err != nil {
		t.Errorf("rejected a legitimate path %q: %v", ok, err)
	}
}

func TestCheckLocalHostPath_RequiresTheFields(t *testing.T) {
	if err := checkLocalHostPath(localSnap("")); err == nil {
		t.Error("accepted an empty hostPath")
	}
	s := localSnap("")
	s.Spec.Backend.Local = nil
	if err := checkLocalHostPath(s); err == nil {
		t.Error("accepted backend.type=local with no backend.local")
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
