package gateway

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	connect "connectrpc.com/connect"
	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDCClaimConfig maps OIDC ID-token claims to the impersonated Kubernetes
// identity. Defaults suit Keycloak/Dex (a human-readable username + a groups
// array). The prefixes mirror the apiserver's --oidc-username-prefix /
// --oidc-groups-prefix, so the impersonated subject can match however the
// member clusters bind their OIDC users in RBAC.
type OIDCClaimConfig struct {
	UsernameClaim  string // e.g. "email" or "preferred_username"
	GroupsClaim    string // e.g. "groups"
	UsernamePrefix string // optional, prepended to the username (e.g. "oidc:")
	GroupsPrefix   string // optional, prepended to each group
}

// oidcAuthenticator validates the request's bearer token as an OIDC ID token
// against a configured issuer (gateway-side OIDC, decision A1) and maps its
// claims to the impersonated user+groups. mTLS-free: it trusts tokens the IdP
// signed, not the API server's OIDC config — so it works even when the member
// API servers are not OIDC-wired.
//
// The provider/verifier is built lazily and cached, so a transient IdP outage
// at gateway startup does not permanently wedge auth: the verifier stays unbuilt
// and the next request retries the discovery.
type oidcAuthenticator struct {
	issuerURL string
	clientID  string
	claims    OIDCClaimConfig

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
}

// NewOIDCAuthenticator builds a gateway-side OIDC authenticator. issuerURL is
// the IdP's OIDC issuer (its discovery doc + JWKS are fetched lazily on first
// use); clientID is the audience the ID token must carry.
func NewOIDCAuthenticator(issuerURL, clientID string, claims OIDCClaimConfig) Authenticator {
	if claims.UsernameClaim == "" {
		claims.UsernameClaim = "email"
	}
	if claims.GroupsClaim == "" {
		claims.GroupsClaim = "groups"
	}
	return &oidcAuthenticator{issuerURL: issuerURL, clientID: clientID, claims: claims}
}

func (a *oidcAuthenticator) getVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verifier != nil {
		return a.verifier, nil
	}
	provider, err := oidc.NewProvider(ctx, a.issuerURL)
	if err != nil {
		return nil, err
	}
	a.verifier = provider.Verifier(&oidc.Config{ClientID: a.clientID})
	return a.verifier, nil
}

func (a *oidcAuthenticator) Authenticate(ctx context.Context, h http.Header) (Identity, error) {
	raw := bearerToken(h)
	if raw == "" {
		return Identity{}, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("missing bearer token"))
	}
	verifier, err := a.getVerifier(ctx)
	if err != nil {
		// Issuer discovery failed (IdP unreachable / not ready). Retryable — the
		// verifier is left unbuilt so a later request rebuilds it.
		return Identity{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("oidc provider not ready: %w", err))
	}
	idToken, err := verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("oidc token rejected: %w", err))
	}
	var claims map[string]interface{}
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, connect.NewError(connect.CodeInternal, err)
	}
	id, err := identityFromClaims(claims, a.claims)
	if err != nil {
		return Identity{}, connect.NewError(connect.CodeUnauthenticated, err)
	}
	return id, nil
}

// identityFromClaims maps a verified token's claims to the impersonated identity.
// Factored out so the claim/prefix logic is unit-testable without a live IdP —
// the Verify/JWKS path is go-oidc's (exercised at the cluster against Keycloak).
func identityFromClaims(claims map[string]interface{}, cfg OIDCClaimConfig) (Identity, error) {
	user := stringClaim(claims, cfg.UsernameClaim)
	if user == "" {
		return Identity{}, fmt.Errorf("token has no %q claim for the username", cfg.UsernameClaim)
	}
	id := Identity{User: cfg.UsernamePrefix + user}
	for _, g := range stringSliceClaim(claims, cfg.GroupsClaim) {
		id.Groups = append(id.Groups, cfg.GroupsPrefix+g)
	}
	return id, nil
}

func stringClaim(claims map[string]interface{}, key string) string {
	if key == "" {
		return ""
	}
	s, _ := claims[key].(string)
	return s
}

// stringSliceClaim reads a groups-style claim: a JSON array of strings (the
// common Keycloak/Dex shape) or a single string. Missing or other types yield
// nil (a user with no groups is valid — they just get no group bindings).
func stringSliceClaim(claims map[string]interface{}, key string) []string {
	if key == "" {
		return nil
	}
	switch v := claims[key].(type) {
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}
