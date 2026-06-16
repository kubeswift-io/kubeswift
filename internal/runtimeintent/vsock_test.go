package runtimeintent

import "testing"

func TestDeriveVsockCID(t *testing.T) {
	a := DeriveVsockCID("ns", "guest-a")
	// deterministic
	if a != DeriveVsockCID("ns", "guest-a") {
		t.Fatal("DeriveVsockCID not deterministic")
	}
	// in valid range (>= 3, reserved CIDs 0-2 excluded)
	if a < 3 {
		t.Fatalf("CID %d < 3 (reserved range)", a)
	}
	// distinct guests get distinct CIDs (different name and different namespace)
	if DeriveVsockCID("ns", "guest-a") == DeriveVsockCID("ns", "guest-b") {
		t.Error("expected distinct CIDs for distinct names")
	}
	if DeriveVsockCID("ns1", "g") == DeriveVsockCID("ns2", "g") {
		t.Error("expected distinct CIDs for distinct namespaces")
	}
}
