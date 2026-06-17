package swiftmigration

import (
	"testing"

	"github.com/projectbeskar/kubeswift/internal/controller/swiftguest"
)

// A kube-ovn primary-on-NAD src pod carries the per-provider mac_address
// annotation (stamped by the SwiftGuest controller). The dst pod must preserve
// the MAC (LSP identity), pin the guest's CURRENT IP, and carry migrationJobName
// (so kube-ovn lets the dst share the src's still-held static IP across cutover).
func TestMergeAnnotationsForDst_KubeOVNIdentityPreservedAndIPKept(t *testing.T) {
	provider := "ovn-l2.ovn-val.ovn"
	macKey := swiftguest.KubeOVNMACAnnotationKey(provider)
	ipKey := swiftguest.KubeOVNIPAnnotationKey(provider)
	src := map[string]string{
		swiftguest.MultusAnnotationKey: "ovn-val/ovn-l2",
		macKey:                         "52:54:00:c4:0d:90",
	}

	out := mergeAnnotationsForDst(src, true /*mtls*/, "10.20.0.4", "ovn-vm-live")

	if out[macKey] != "52:54:00:c4:0d:90" {
		t.Errorf("dst must preserve the kube-ovn mac_address (LSP identity); got %q", out[macKey])
	}
	if out[ipKey] != "10.20.0.4" {
		t.Errorf("dst must pin the guest's current IP at %s; got %q", ipKey, out[ipKey])
	}
	if out[swiftguest.MigrationJobNameAnnotation] != "ovn-vm-live" {
		t.Errorf("dst must carry migrationJobName so kube-ovn shares the src IP; got %q", out[swiftguest.MigrationJobNameAnnotation])
	}
	if out[swiftguest.MultusAnnotationKey] != "ovn-val/ovn-l2" {
		t.Errorf("dst must still preserve the Multus networks annotation")
	}
}

// A plain (node-local or non-kube-ovn-NAD) guest has no kube-ovn mac annotation
// on the src pod -> the dst must NOT get a migrationJobName or any ip pin (the
// kube-ovn path is inert), and the plaintext ack gate is preserved.
func TestMergeAnnotationsForDst_NonKubeOVN_Inert(t *testing.T) {
	src := map[string]string{swiftguest.MultusAnnotationKey: "ns/bridge-nad"}

	out := mergeAnnotationsForDst(src, false /*plaintext*/, "10.244.1.5", "m1")

	if _, ok := out[swiftguest.MigrationJobNameAnnotation]; ok {
		t.Errorf("non-kube-ovn dst must not carry migrationJobName")
	}
	if out[AnnotationMigrationPhase2Ack] != AnnotationMigrationPhase2AckValue {
		t.Errorf("plaintext dst should keep the phase2 ack gate")
	}
}

// IP not yet recorded in status -> preserve MAC + set migrationJobName but skip
// the ip pin (kube-ovn allocates; the controller records it; a later pod pins it).
func TestMergeAnnotationsForDst_KubeOVN_NoIPWhenStatusEmpty(t *testing.T) {
	provider := "ovn-l2.ns.ovn"
	macKey := swiftguest.KubeOVNMACAnnotationKey(provider)
	src := map[string]string{macKey: "52:54:00:aa:bb:cc"}

	out := mergeAnnotationsForDst(src, true, "" /*no IP yet*/, "m2")

	if out[macKey] == "" {
		t.Errorf("MAC must be preserved even before the IP is known")
	}
	if _, ok := out[swiftguest.KubeOVNIPAnnotationKey(provider)]; ok {
		t.Errorf("no ip pin should be set when the guest IP is unknown")
	}
	if out[swiftguest.MigrationJobNameAnnotation] != "m2" {
		t.Errorf("migrationJobName should still be set")
	}
}
