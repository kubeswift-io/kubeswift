package storagelocation

import (
	"strings"
	"testing"

	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
)

// The cache lists objects in no particular order; the message must not
// depend on it.
func TestAmbiguousSortsNames(t *testing.T) {
	in := []string{"zeta", "alpha", "mid"}
	w := ambiguous(storagev1alpha1.KindSwiftClusterStorageLocation, in, "the cluster")
	if w.Reason != ReasonAmbiguous || !strings.Contains(w.Message, "alpha, mid, zeta") {
		t.Errorf("ambiguous = %+v", w)
	}
	if in[0] != "zeta" {
		t.Error("the caller's slice must not be reordered")
	}
}

func TestCredentialsSecret(t *testing.T) {
	for _, c := range []struct {
		o    storagev1alpha1.OCILocation
		want string
	}{
		{storagev1alpha1.OCILocation{}, storagev1alpha1.DefaultCredentialsSecretName},
		{storagev1alpha1.OCILocation{CredentialsSecretName: "regcreds"}, "regcreds"},
		{storagev1alpha1.OCILocation{Anonymous: true}, ""},
	} {
		if got := CredentialsSecret(&c.o); got != c.want {
			t.Errorf("CredentialsSecret(%+v) = %q, want %q", c.o, got, c.want)
		}
	}
}

func TestSnapshotRepository(t *testing.T) {
	spec := storagev1alpha1.StorageLocationSpec{OCI: &storagev1alpha1.OCILocation{Repository: "registry.example.com/kubeswift/"}}
	if got := SnapshotRepository(&Resolved{Kind: storagev1alpha1.KindSwiftClusterStorageLocation, Spec: spec}, "team-a"); got != "registry.example.com/kubeswift/team-a/snapshots" {
		t.Errorf("cluster location: %q", got)
	}
	if got := SnapshotRepository(&Resolved{Kind: storagev1alpha1.KindSwiftStorageLocation, Spec: spec}, "team-a"); got != "registry.example.com/kubeswift/snapshots" {
		t.Errorf("namespace location: %q", got)
	}
}
