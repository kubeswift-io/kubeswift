package main

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
)

// locationFlagHelp is the --location help text shared by snapshot and
// schedule create.
const locationFlagHelp = "Storage location (oci or csi-volume-snapshot): NAME for a SwiftStorageLocation in the namespace, or cluster/NAME for a SwiftClusterStorageLocation. Omit to use the namespace's default, else the cluster's"

// parseLocationFlag maps --location to a locationRef: NAME is a
// SwiftStorageLocation in the snapshot's namespace, cluster/NAME a
// SwiftClusterStorageLocation.
func parseLocationFlag(v string) (*storagev1alpha1.StorageLocationRef, error) {
	if v == "" {
		return nil, nil
	}
	ref := &storagev1alpha1.StorageLocationRef{Kind: storagev1alpha1.KindSwiftStorageLocation, Name: v}
	if name, ok := strings.CutPrefix(v, "cluster/"); ok {
		ref = &storagev1alpha1.StorageLocationRef{Kind: storagev1alpha1.KindSwiftClusterStorageLocation, Name: name}
	}
	if errs := validation.IsDNS1123Subdomain(ref.Name); len(errs) > 0 {
		return nil, fmt.Errorf("--location %q: want NAME or cluster/NAME: %s", v, strings.Join(errs, "; "))
	}
	return ref, nil
}

// buildSnapshotBackend turns the --backend, --location, --vsclass and
// --hostpath flags into a snapshot backend. template is true for a
// schedule's template, which takes no --hostpath: every snapshot of the
// schedule would share the directory.
func buildSnapshotBackend(backendFlag, location, vsclass, hostpath string, template bool) (snapshotv1alpha1.SwiftSnapshotBackend, error) {
	var b snapshotv1alpha1.SwiftSnapshotBackend
	t, err := parseBackendFlag(backendFlag)
	if err != nil {
		return b, err
	}
	ref, err := parseLocationFlag(location)
	if err != nil {
		return b, err
	}
	b.Type = t
	switch t {
	case snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot:
		if hostpath != "" {
			return b, fmt.Errorf("--hostpath is only valid for --backend=local")
		}
		if ref != nil && vsclass != "" {
			return b, fmt.Errorf("--location and --vsclass are mutually exclusive: --vsclass names the class itself; --location takes the location's")
		}
		b.CSIVolumeSnapshot = &snapshotv1alpha1.CSIVolumeSnapshotBackend{VolumeSnapshotClassName: vsclass}
		b.LocationRef = ref
	case snapshotv1alpha1.SnapshotBackendLocal:
		if vsclass != "" {
			return b, fmt.Errorf("--vsclass is only valid for --backend=csi-volume-snapshot")
		}
		if ref != nil {
			return b, fmt.Errorf("--location is only valid for --backend=oci or csi-volume-snapshot")
		}
		if template {
			if hostpath != "" {
				return b, fmt.Errorf("--hostpath is no longer accepted: each scheduled local snapshot is captured into a directory derived from its name")
			}
		} else {
			b.Local = &snapshotv1alpha1.LocalBackend{HostPath: hostpath}
		}
	case snapshotv1alpha1.SnapshotBackendOCI:
		if vsclass != "" {
			return b, fmt.Errorf("--vsclass is only valid for --backend=csi-volume-snapshot")
		}
		if hostpath != "" {
			return b, fmt.Errorf("--hostpath is only valid for --backend=local")
		}
		// No oci block: the registry comes from the location, resolved by
		// the controller when the snapshot is created.
		b.LocationRef = ref
	}
	return b, nil
}

// describeLocationRef is how a create command reports where a snapshot goes.
func describeLocationRef(b snapshotv1alpha1.SwiftSnapshotBackend) string {
	switch {
	case b.LocationRef != nil:
		return b.LocationRef.Kind + "/" + b.LocationRef.Name
	case b.Type == snapshotv1alpha1.SnapshotBackendOCI && b.OCI == nil,
		b.Type == snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot && (b.CSIVolumeSnapshot == nil || b.CSIVolumeSnapshot.VolumeSnapshotClassName == ""):
		return "the default"
	}
	return ""
}
