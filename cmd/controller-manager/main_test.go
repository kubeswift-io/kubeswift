package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// volumeSnapshotCRDsInstalled gates the snapshot controllers' Owns(VolumeSnapshot)
// watch so a cluster without the external-snapshotter CRDs runs the core VM
// runtime instead of fatally exiting (the P3 install-blocker finding).
func TestVolumeSnapshotCRDsInstalled(t *testing.T) {
	// Present: discovery advertises VolumeSnapshot in snapshot.storage.k8s.io/v1.
	present := fake.NewSimpleClientset()
	present.Resources = []*metav1.APIResourceList{{
		GroupVersion: "snapshot.storage.k8s.io/v1",
		APIResources: []metav1.APIResource{{Name: "volumesnapshots", Kind: "VolumeSnapshot"}},
	}}
	if !volumeSnapshotCRDsInstalled(present) {
		t.Errorf("want true when the VolumeSnapshot CRD is advertised by discovery")
	}

	// Absent: no snapshot group-version → ServerResourcesForGroupVersion errors → false.
	absent := fake.NewSimpleClientset()
	if volumeSnapshotCRDsInstalled(absent) {
		t.Errorf("want false when the VolumeSnapshot CRD is absent")
	}

	// Group-version present but only another kind in it → still false.
	otherKind := fake.NewSimpleClientset()
	otherKind.Resources = []*metav1.APIResourceList{{
		GroupVersion: "snapshot.storage.k8s.io/v1",
		APIResources: []metav1.APIResource{{Name: "volumesnapshotclasses", Kind: "VolumeSnapshotClass"}},
	}}
	if volumeSnapshotCRDsInstalled(otherKind) {
		t.Errorf("want false when the group-version lacks the VolumeSnapshot kind")
	}
}
