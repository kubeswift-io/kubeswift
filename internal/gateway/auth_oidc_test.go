package gateway

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	connect "connectrpc.com/connect"
)

func TestIdentityFromClaims_Keycloak(t *testing.T) {
	claims := map[string]interface{}{
		"preferred_username": "alice",
		"email":              "alice@example.com",
		"groups":             []interface{}{"kubeswift-operators", "team-a"},
	}
	id, err := identityFromClaims(claims, OIDCClaimConfig{UsernameClaim: "preferred_username", GroupsClaim: "groups"})
	if err != nil {
		t.Fatal(err)
	}
	if id.User != "alice" {
		t.Errorf("user = %q, want alice", id.User)
	}
	if len(id.Groups) != 2 || id.Groups[0] != "kubeswift-operators" || id.Groups[1] != "team-a" {
		t.Errorf("groups = %v", id.Groups)
	}
}

func TestIdentityFromClaims_EmailAndPrefixes(t *testing.T) {
	claims := map[string]interface{}{"email": "bob@example.com", "groups": []interface{}{"admins"}}
	id, err := identityFromClaims(claims, OIDCClaimConfig{
		UsernameClaim: "email", GroupsClaim: "groups",
		UsernamePrefix: "oidc:", GroupsPrefix: "oidc:",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id.User != "oidc:bob@example.com" {
		t.Errorf("user = %q, want oidc:bob@example.com", id.User)
	}
	if len(id.Groups) != 1 || id.Groups[0] != "oidc:admins" {
		t.Errorf("groups = %v", id.Groups)
	}
}

func TestIdentityFromClaims_MissingUsername(t *testing.T) {
	_, err := identityFromClaims(
		map[string]interface{}{"groups": []interface{}{"x"}},
		OIDCClaimConfig{UsernameClaim: "email", GroupsClaim: "groups"},
	)
	if err == nil {
		t.Fatal("want an error when the username claim is absent")
	}
}

func TestStringSliceClaim_Variants(t *testing.T) {
	cases := []struct {
		name string
		v    interface{}
		want int
	}{
		{"array", []interface{}{"a", "b"}, 2},
		{"single-string", "solo", 1},
		{"absent", nil, 0},
		{"empty-string", "", 0},
		{"mixed-drops-nonstrings-and-empties", []interface{}{"a", 3, ""}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := map[string]interface{}{}
			if c.v != nil {
				claims["groups"] = c.v
			}
			if got := stringSliceClaim(claims, "groups"); len(got) != c.want {
				t.Errorf("got %v (len %d), want len %d", got, len(got), c.want)
			}
		})
	}
}

func TestNewOIDCAuthenticator_AppliesDefaults(t *testing.T) {
	a := NewOIDCAuthenticator("https://issuer.example", "kubeswift", "", OIDCClaimConfig{})
	oa, ok := a.(*oidcAuthenticator)
	if !ok {
		t.Fatal("NewOIDCAuthenticator did not return an *oidcAuthenticator")
	}
	if oa.claims.UsernameClaim != "email" || oa.claims.GroupsClaim != "groups" {
		t.Errorf("defaults not applied: %+v", oa.claims)
	}
}

// A missing bearer token is rejected before any IdP round-trip, so this needs no
// live issuer.
func TestOIDCAuthenticator_MissingToken(t *testing.T) {
	a := NewOIDCAuthenticator("https://issuer.example", "kubeswift", "", OIDCClaimConfig{})
	_, err := a.Authenticate(context.Background(), http.Header{})
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("want Unauthenticated for a missing token, got %v", err)
	}
}

func TestHTTPClientWithCA(t *testing.T) {
	if _, err := httpClientWithCA("/no/such/oidc-ca.pem"); err == nil {
		t.Error("expected error for a missing CA file")
	}
	f := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(f, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := httpClientWithCA(f); err == nil {
		t.Error("expected error for a file with no PEM certificates")
	}
}
