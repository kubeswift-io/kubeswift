package gateway

import (
	"testing"

	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
)

func TestSplitProcedure(t *testing.T) {
	for _, tc := range []struct {
		in, svc, method string
	}{
		{"/kubeswift.v1.GuestService/DeleteGuest", "GuestService", "DeleteGuest"},
		{"/kubeswift.v1.ResourceService/ApplyResource", "ResourceService", "ApplyResource"},
		{"garbage", "", "garbage"},
	} {
		svc, m := splitProcedure(tc.in)
		if svc != tc.svc || m != tc.method {
			t.Errorf("splitProcedure(%q) = (%q,%q), want (%q,%q)", tc.in, svc, m, tc.svc, tc.method)
		}
	}
}

// The classification is the whole point of the interceptor: a method that
// changes state and is NOT recognised as mutating becomes an invisible
// deletion, which is the exact failure this file exists to prevent.
func TestIsMutating(t *testing.T) {
	mutations := []string{
		"DeleteGuest", "DeleteResource", "ApplyResource", "StartGuest",
		"StopGuest", "MigrateGuest", "DeleteRole", "CreateRoleBinding",
	}
	for _, m := range mutations {
		if !isMutating(m) {
			t.Errorf("%s must be classified as mutating", m)
		}
	}

	reads := []string{"ListGuests", "GetGuestDetail", "WatchGuests", "CanI", "ListResourceKinds"}
	for _, m := range reads {
		if isMutating(m) {
			t.Errorf("%s should be a read", m)
		}
	}

	// An RPC nobody has classified must default to mutating, so that forgetting
	// costs a noisy log line instead of another unattributable deletion.
	for _, unknown := range []string{"FrobnicateGuest", "", "Reap"} {
		if !isMutating(unknown) {
			t.Errorf("unknown method %q must default to mutating", unknown)
		}
	}
}

func TestRequestTargetFollowsRef(t *testing.T) {
	req := &kubeswiftv1.DeleteGuestRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "dev", Namespace: "field-testing", Name: "ft-vm"},
	}
	c, ns, n := requestTarget(req)
	if c != "dev" || ns != "field-testing" || n != "ft-vm" {
		t.Fatalf("requestTarget = (%q,%q,%q), want (dev,field-testing,ft-vm)", c, ns, n)
	}
}

func TestRequestTargetUnknownShapeIsEmptyNotGuessed(t *testing.T) {
	// A request with no ref and no flat fields must yield "" so the log omits
	// the keys, rather than inventing a target.
	c, ns, n := requestTarget(&kubeswiftv1.ListGuestsRequest{})
	if c != "" || n != "" {
		t.Fatalf("expected empty target for a fieldless request, got (%q,%q,%q)", c, ns, n)
	}
}

func TestRequestTargetNonProtoIsSafe(t *testing.T) {
	if c, ns, n := requestTarget("not a proto"); c != "" || ns != "" || n != "" {
		t.Fatalf("non-proto message must not panic or invent fields, got (%q,%q,%q)", c, ns, n)
	}
}
