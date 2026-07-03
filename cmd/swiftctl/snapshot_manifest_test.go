package main

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

func manifestTestSnap() *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "db-export",
			Namespace:       "team-a",
			UID:             types.UID("uid-123"),
			ResourceVersion: "42",
			Finalizers:      []string{"snapshot.kubeswift.io/s3-objects"},
			Labels:          map[string]string{"demo": "coldmig"},
		},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef:      snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "db"},
			IncludeMemory: true,
			IncludeDisk:   true,
			// Delete is the field default — the copy must NOT inherit it.
			DeletionPolicy: snapshotv1alpha1.SnapshotDeletionPolicyDelete,
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendOCI,
				OCI:  &snapshotv1alpha1.OCIBackend{Repository: "hub.example.com/vm-snapshots"},
			},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{
			Phase: snapshotv1alpha1.SwiftSnapshotPhaseReady,
			OCI: &snapshotv1alpha1.OCISnapshotStatus{
				Reference:      "hub.example.com/vm-snapshots:team-a-db-export",
				ManifestDigest: "sha256:mem123",
				Disk:           &snapshotv1alpha1.OCIDiskArtifact{ManifestDigest: "sha256:disk123"},
			},
			GuestSpec: &snapshotv1alpha1.CapturedGuestSpec{
				CPU: "2", MemoryMi: 2048,
				Storage: &snapshotv1alpha1.CapturedStorage{AccessMode: "ReadWriteOnce", VolumeMode: "Filesystem"},
			},
		},
	}
}

func TestPortableSnapshotManifest_StripsClusterLocalMetadata(t *testing.T) {
	out := portableSnapshotManifest(manifestTestSnap(), false)
	if out.UID != "" || out.ResourceVersion != "" || len(out.Finalizers) != 0 {
		t.Errorf("cluster-local metadata must be stripped; got uid=%q rv=%q finalizers=%v",
			out.UID, out.ResourceVersion, out.Finalizers)
	}
	if out.APIVersion != "snapshot.kubeswift.io/v1alpha1" || out.Kind != "SwiftSnapshot" {
		t.Errorf("TypeMeta must be set for a standalone manifest; got %s/%s", out.APIVersion, out.Kind)
	}
	if out.Labels["demo"] != "coldmig" {
		t.Errorf("labels must be preserved; got %v", out.Labels)
	}
	// Status carried verbatim — the whole point of the manifest.
	if out.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseReady ||
		out.Status.OCI == nil || out.Status.OCI.Disk == nil ||
		out.Status.OCI.Disk.ManifestDigest != "sha256:disk123" ||
		out.Status.GuestSpec == nil || out.Status.GuestSpec.Storage == nil {
		t.Errorf("status (oci refs + captured surface) must be carried; got %+v", out.Status)
	}
}

func TestPortableSnapshotManifest_DeletionPolicyRetainForTheCopy(t *testing.T) {
	// Default: the copy is rewritten to Retain — deleting a Delete-policy copy
	// in the target cluster would purge registry artifacts other clusters use.
	out := portableSnapshotManifest(manifestTestSnap(), false)
	if out.Spec.DeletionPolicy != snapshotv1alpha1.SnapshotDeletionPolicyRetain {
		t.Errorf("copy deletionPolicy = %q, want Retain", out.Spec.DeletionPolicy)
	}
	// --keep-deletion-policy preserves the original.
	kept := portableSnapshotManifest(manifestTestSnap(), true)
	if kept.Spec.DeletionPolicy != snapshotv1alpha1.SnapshotDeletionPolicyDelete {
		t.Errorf("kept deletionPolicy = %q, want the original Delete", kept.Spec.DeletionPolicy)
	}
}

func manifestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	gv := schema.GroupVersion{Group: snapshotv1alpha1.GroupName, Version: snapshotv1alpha1.Version}
	s.AddKnownTypes(gv, &snapshotv1alpha1.SwiftSnapshot{}, &snapshotv1alpha1.SwiftSnapshotList{})
	metav1.AddToGroupVersion(s, gv)
	return s
}

func TestImportSnapshotManifest_CreatesSpecAndTransplantsStatus(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(manifestScheme(t)).
		WithStatusSubresource(&snapshotv1alpha1.SwiftSnapshot{}).
		Build()
	manifest := portableSnapshotManifest(manifestTestSnap(), false)
	if err := importSnapshotManifest(context.Background(), c, manifest); err != nil {
		t.Fatalf("importSnapshotManifest: %v", err)
	}
	var got snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: "db-export", Namespace: "team-a"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Backend.OCI == nil || got.Spec.Backend.OCI.Repository != "hub.example.com/vm-snapshots" {
		t.Errorf("spec not created: %+v", got.Spec.Backend)
	}
	// The status subresource transplant is the load-bearing step.
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseReady ||
		got.Status.OCI == nil || got.Status.OCI.ManifestDigest != "sha256:mem123" ||
		got.Status.OCI.Disk == nil || got.Status.OCI.Disk.ManifestDigest != "sha256:disk123" ||
		got.Status.GuestSpec == nil {
		t.Errorf("status not transplanted: %+v", got.Status)
	}
}

func TestImportSnapshotManifest_AlreadyExistsFailsLoudly(t *testing.T) {
	existing := manifestTestSnap()
	c := fake.NewClientBuilder().
		WithScheme(manifestScheme(t)).
		WithStatusSubresource(&snapshotv1alpha1.SwiftSnapshot{}).
		WithObjects(existing).
		Build()
	err := importSnapshotManifest(context.Background(), c, portableSnapshotManifest(manifestTestSnap(), false))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("importing over an existing snapshot must fail loudly; err=%v", err)
	}
}
