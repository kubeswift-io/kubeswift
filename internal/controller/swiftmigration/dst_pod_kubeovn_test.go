package swiftmigration

import (
	"testing"

	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
)

// mergeAnnotationsForDst merges the OVN backend's pre-computed dst annotations
// (resolved from the guest by the migration controller via
// swiftguest.OVNMigrationDstAnnotations — the per-CNI derivation itself is tested
// in the swiftguest package) alongside the preserved Multus networks annotation.
// For a kube-ovn guest that map is the LSP identity MAC + the IP pin +
// migrationJobName.
func TestMergeAnnotationsForDst_MergesOVNDstAnnotations(t *testing.T) {
	provider := "ovn-l2.ovn-val.ovn"
	macKey := swiftguest.KubeOVNMACAnnotationKey(provider)
	ipKey := swiftguest.KubeOVNIPAnnotationKey(provider)
	src := map[string]string{swiftguest.MultusAnnotationKey: "ovn-val/ovn-l2"}
	ovnDst := map[string]string{
		macKey:                                "52:54:00:c4:0d:90",
		ipKey:                                 "10.20.0.4",
		swiftguest.MigrationJobNameAnnotation: "ovn-vm-live",
	}

	out := mergeAnnotationsForDst(src, true /*mtls*/, ovnDst)

	if out[macKey] != "52:54:00:c4:0d:90" {
		t.Errorf("dst must carry the OVN backend's mac_address (LSP identity); got %q", out[macKey])
	}
	if out[ipKey] != "10.20.0.4" {
		t.Errorf("dst must carry the OVN backend's IP pin; got %q", out[ipKey])
	}
	if out[swiftguest.MigrationJobNameAnnotation] != "ovn-vm-live" {
		t.Errorf("dst must carry migrationJobName; got %q", out[swiftguest.MigrationJobNameAnnotation])
	}
	if out[swiftguest.MultusAnnotationKey] != "ovn-val/ovn-l2" {
		t.Errorf("dst must still preserve the Multus networks annotation")
	}
	if _, ok := out[AnnotationMigrationPhase2Ack]; ok {
		t.Errorf("mtls dst should NOT carry the plaintext ack")
	}
}

// A plain (node-local or non-OVN-NAD) guest yields an empty OVN dst-annotation map
// -> the dst must NOT get a migrationJobName or any pin (the OVN path is inert),
// and the plaintext ack gate is preserved.
func TestMergeAnnotationsForDst_NonOVN_Inert(t *testing.T) {
	src := map[string]string{swiftguest.MultusAnnotationKey: "ns/bridge-nad"}

	out := mergeAnnotationsForDst(src, false /*plaintext*/, nil)

	if _, ok := out[swiftguest.MigrationJobNameAnnotation]; ok {
		t.Errorf("non-OVN dst must not carry migrationJobName")
	}
	if out[AnnotationMigrationPhase2Ack] != AnnotationMigrationPhase2AckValue {
		t.Errorf("plaintext dst should keep the phase2 ack gate")
	}
	if out[swiftguest.MultusAnnotationKey] != "ns/bridge-nad" {
		t.Errorf("dst must still preserve the Multus networks annotation")
	}
}
