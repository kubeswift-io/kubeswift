package main

import (
	"strings"
	"testing"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
)

func TestParseLocationFlag(t *testing.T) {
	cases := map[string]*storagev1alpha1.StorageLocationRef{
		"":                 nil,
		"team":             {Kind: storagev1alpha1.KindSwiftStorageLocation, Name: "team"},
		"cluster/registry": {Kind: storagev1alpha1.KindSwiftClusterStorageLocation, Name: "registry"},
	}
	for in, want := range cases {
		got, err := parseLocationFlag(in)
		if err != nil || (want == nil) != (got == nil) || (got != nil && *got != *want) {
			t.Errorf("parseLocationFlag(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"Team", "cluster/", "a/b"} {
		if _, err := parseLocationFlag(bad); err == nil {
			t.Errorf("parseLocationFlag(%q) accepted", bad)
		}
	}
}

func TestBuildSnapshotBackend(t *testing.T) {
	// oci with no location: no oci block, the defaults apply.
	b, err := buildSnapshotBackend("oci", "", "", "", false)
	if err != nil || b.Type != snapshotv1alpha1.SnapshotBackendOCI || b.OCI != nil || b.LocationRef != nil {
		t.Errorf("oci, defaults: %+v %v", b, err)
	}
	if describeLocationRef(b) != "the default" {
		t.Errorf("describe = %q", describeLocationRef(b))
	}
	// oci with a cluster location.
	b, err = buildSnapshotBackend("oci", "cluster/registry", "", "", true)
	if err != nil || b.LocationRef == nil || b.LocationRef.Kind != storagev1alpha1.KindSwiftClusterStorageLocation {
		t.Errorf("oci, cluster location: %+v %v", b, err)
	}
	// csi taking a namespace location's class.
	b, err = buildSnapshotBackend("csi-volume-snapshot", "team", "", "", false)
	if err != nil || b.LocationRef == nil || b.LocationRef.Name != "team" {
		t.Errorf("csi, location: %+v %v", b, err)
	}
	// An explicit oci block (a manifest) is not "the default".
	if d := describeLocationRef(snapshotv1alpha1.SwiftSnapshotBackend{Type: snapshotv1alpha1.SnapshotBackendOCI, OCI: &snapshotv1alpha1.OCIBackend{Repository: "r"}}); d != "" {
		t.Errorf("explicit oci described as %q", d)
	}

	bad := map[string][5]string{
		"location and vsclass":   {"csi-volume-snapshot", "team", "fast", "", "mutually exclusive"},
		"location on local":      {"local", "team", "", "", "--location is only valid"},
		"vsclass on oci":         {"oci", "", "fast", "", "--vsclass is only valid"},
		"hostpath on oci":        {"oci", "", "", "/var/lib/kubeswift/snapshots/x", "--hostpath is only valid"},
		"unknown backend":        {"s4", "", "", "", "want csi-volume-snapshot, local or oci"},
		"hostpath in a template": {"local", "", "", "/var/lib/kubeswift/snapshots/x", "no longer accepted"},
	}
	for name, c := range bad {
		template := name == "hostpath in a template"
		if _, err := buildSnapshotBackend(c[0], c[1], c[2], c[3], template); err == nil || !strings.Contains(err.Error(), c[4]) {
			t.Errorf("%s: err = %v, want %q", name, err, c[4])
		}
	}
}
