package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func TestBuildExportSnapshot_FullStateOCI(t *testing.T) {
	s := buildExportSnapshot("db-export", "team-a", "db", "ghcr.io/acme/vm-snapshots", "", false, "", "")
	if s.Spec.Backend.Type != snapshotv1alpha1.SnapshotBackendOCI {
		t.Errorf("backend = %q, want oci", s.Spec.Backend.Type)
	}
	if !s.Spec.IncludeMemory || !s.Spec.IncludeDisk {
		t.Errorf("export must be full-state (includeMemory && includeDisk); got mem=%v disk=%v",
			s.Spec.IncludeMemory, s.Spec.IncludeDisk)
	}
	if s.Spec.GuestRef.Name != "db" {
		t.Errorf("guestRef = %q, want db", s.Spec.GuestRef.Name)
	}
	if s.Spec.Backend.OCI == nil || s.Spec.Backend.OCI.Repository != "ghcr.io/acme/vm-snapshots" {
		t.Errorf("oci.repository not set: %+v", s.Spec.Backend.OCI)
	}
	if s.Spec.Backend.OCI.CredentialsSecretRef != nil || s.Spec.Backend.OCI.SigningKeySecretRef != nil {
		t.Errorf("no creds/sign secrets should be set when flags empty: %+v", s.Spec.Backend.OCI)
	}
	if s.Spec.Backend.OCI.Insecure {
		t.Errorf("insecure must default false")
	}
}

func TestBuildExportSnapshot_InsecureCredsSignTag(t *testing.T) {
	s := buildExportSnapshot("snap", "ns", "vm", "zot.svc:5000/snaps", "custom-tag", true, "regcreds", "cosign-key")
	oci := s.Spec.Backend.OCI
	if !oci.Insecure {
		t.Errorf("--insecure not propagated")
	}
	if oci.Tag != "custom-tag" {
		t.Errorf("tag = %q, want custom-tag", oci.Tag)
	}
	if oci.CredentialsSecretRef == nil || oci.CredentialsSecretRef.Name != "regcreds" {
		t.Errorf("credentials secret not set: %+v", oci.CredentialsSecretRef)
	}
	if oci.SigningKeySecretRef == nil || oci.SigningKeySecretRef.Name != "cosign-key" {
		t.Errorf("signing key secret not set: %+v", oci.SigningKeySecretRef)
	}
}

func TestBuildImportGuest_CloneFromSnapshot(t *testing.T) {
	g := buildImportGuest("db2", "team-a", "db-export", "boba", "ft-small")
	if g.Spec.CloneFromSnapshot == nil {
		t.Fatal("import guest must set cloneFromSnapshot")
	}
	if g.Spec.CloneFromSnapshot.SnapshotRef.Name != "db-export" {
		t.Errorf("snapshotRef = %q, want db-export", g.Spec.CloneFromSnapshot.SnapshotRef.Name)
	}
	if g.Spec.CloneFromSnapshot.TargetNode != "boba" {
		t.Errorf("targetNode = %q, want boba", g.Spec.CloneFromSnapshot.TargetNode)
	}
	// guestClassRef is required by the CRD/webhook even for a clone.
	if g.Spec.GuestClassRef.Name != "ft-small" {
		t.Errorf("guestClassRef = %q, want ft-small", g.Spec.GuestClassRef.Name)
	}
	// A clone must NOT set imageRef (mutually exclusive with cloneFromSnapshot).
	if g.Spec.ImageRef != nil {
		t.Errorf("import guest must not set imageRef; got %+v", g.Spec.ImageRef)
	}
	if g.Spec.RunPolicy != swiftv1alpha1.RunPolicyRunning {
		t.Errorf("runPolicy = %q, want Running", g.Spec.RunPolicy)
	}
}

func TestTerminalConditionMessage(t *testing.T) {
	msg := terminalConditionMessage([]metav1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse, Message: "OCI disk chunk Job failed: boom"},
	})
	if msg != "OCI disk chunk Job failed: boom" {
		t.Errorf("got %q", msg)
	}
	if got := terminalConditionMessage(nil); got == "" {
		t.Errorf("empty conditions should still yield a non-empty hint")
	}
}
