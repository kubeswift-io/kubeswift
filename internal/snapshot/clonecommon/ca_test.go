package clonecommon

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/oci"
	kscheme "github.com/kubeswift-io/kubeswift/internal/scheme"
)

// The controller sets the variable snapshot-oras reads; the two packages are
// kept apart, so this is what keeps the names equal.
func TestRegistryCAEnvMatchesSnapshotORAS(t *testing.T) {
	if RegistryCAEnv != oci.RegistryCAEnv {
		t.Fatalf("controller sets %q, snapshot-oras reads %q", RegistryCAEnv, oci.RegistryCAEnv)
	}
	if RegistryCAEnvVars("") != nil {
		t.Error("no bundle, no variable")
	}
}

const (
	oldCA = "-----BEGIN CERTIFICATE-----\nOLD\n-----END CERTIFICATE-----\n"
	newCA = "-----BEGIN CERTIFICATE-----\nNEW\n-----END CERTIFICATE-----\n"
)

func locatedSnap(source, recorded string) *snapshotv1alpha1.SwiftSnapshot {
	s := ociRefSnap("team-a", "s1", "", "")
	s.Spec.Backend.OCI = nil
	s.Status.Location = &snapshotv1alpha1.SnapshotLocation{Source: source, Repository: "registry.example.com/k/team-a/snapshots", Tag: "s1-12345678", CABundle: recorded}
	return s
}

// A restore, clone or deletion trusts the bundle recorded at capture plus
// the one the location holds now, so a CA rotated after the push still
// works, and a deleted location still leaves the recorded one.
func TestTransferCA(t *testing.T) {
	cluster := func(ca string) client.Object {
		return &storagev1alpha1.SwiftClusterStorageLocation{ObjectMeta: metav1.ObjectMeta{Name: "main"},
			Spec: storagev1alpha1.StorageLocationSpec{OCI: &storagev1alpha1.OCILocation{Repository: "registry.example.com/k", CABundle: ca}}}
	}
	nsLoc := func(ns, ca string) client.Object {
		return &storagev1alpha1.SwiftStorageLocation{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "team"},
			Spec: storagev1alpha1.StorageLocationSpec{OCI: &storagev1alpha1.OCILocation{Repository: "registry.example.com/t", CABundle: ca}}}
	}
	cases := map[string]struct {
		snap *snapshotv1alpha1.SwiftSnapshot
		objs []client.Object
		want []string
		not  []string
	}{
		"the location is gone":       {locatedSnap("SwiftClusterStorageLocation/main", oldCA), nil, []string{"OLD"}, []string{"NEW"}},
		"the location is unchanged":  {locatedSnap("SwiftClusterStorageLocation/main", oldCA), []client.Object{cluster(oldCA)}, []string{"OLD"}, nil},
		"the CA was rotated":         {locatedSnap("SwiftClusterStorageLocation/main", oldCA), []client.Object{cluster(newCA)}, []string{"OLD", "NEW"}, nil},
		"no CA recorded, one added":  {locatedSnap("SwiftClusterStorageLocation/main", ""), []client.Object{cluster(newCA)}, []string{"NEW"}, nil},
		"explicit backend":           {locatedSnap(snapshotv1alpha1.SnapshotLocationExplicit, ""), []client.Object{cluster(newCA)}, nil, []string{"NEW"}},
		"own namespace's location":   {locatedSnap("SwiftStorageLocation/team", ""), []client.Object{nsLoc("team-a", newCA)}, []string{"NEW"}, nil},
		"another namespace's is not": {locatedSnap("SwiftStorageLocation/team", ""), []client.Object{nsLoc("other", newCA)}, nil, []string{"NEW"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(kscheme.Scheme).WithObjects(tc.objs...).Build()
			got, err := TransferCA(context.Background(), c, tc.snap)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("bundle %q should hold %s", got, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("bundle %q should not hold %s", got, n)
				}
			}
			if strings.Count(got, "BEGIN CERTIFICATE") != len(tc.want) {
				t.Errorf("bundle %q holds %d certificates, want %d", got, strings.Count(got, "BEGIN CERTIFICATE"), len(tc.want))
			}
		})
	}
}

func TestBuildOCIDownloadJob_CABundle(t *testing.T) {
	p := OCIDownloadJobParams{Snapshot: locatedSnap("SwiftClusterStorageLocation/main", ""), Repository: "r", Tag: "t", Name: "j", Namespace: "team-a", CABundle: newCA}
	var found bool
	for _, e := range BuildOCIDownloadJob(p).Spec.Template.Spec.Containers[0].Env {
		found = found || (e.Name == RegistryCAEnv && e.Value == newCA)
	}
	if !found {
		t.Error("the download Job does not pass the CA bundle")
	}
	p.CABundle = ""
	for _, e := range BuildOCIDownloadJob(p).Spec.Template.Spec.Containers[0].Env {
		if e.Name == RegistryCAEnv {
			t.Error("no bundle, no variable")
		}
	}
}
