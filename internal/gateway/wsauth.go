package gateway

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
)

// This file is the shared auth/origin policy for the three raw-WebSocket planes
// (/console, /sandbox-logs, /sandbox-exec). They are not Connect RPCs, so they
// do not go through the interceptor chain and have to do this themselves.

const (
	// WSBearerPrefix carries the bearer token in a WebSocket SUBPROTOCOL rather
	// than a query parameter.
	//
	// The problem it solves: a browser cannot set an Authorization header on a
	// WebSocket, so the token used to ride ?token=. A query string is echoed by
	// every access log on the path — the UI's own nginx, and any ingress in
	// front of it — which put a live, replayable ID token in front of anyone
	// holding pods/log, plus every log shipper downstream. Subprotocols are sent
	// as a header (Sec-WebSocket-Protocol), so they are not part of the URL and
	// are not logged.
	//
	// The shape mirrors the Kubernetes apiserver's own convention for
	// `kubectl exec` over WebSocket. The value must be a valid HTTP token, so it
	// is base64url WITHOUT padding: '-' and '_' are legal token characters,
	// while '=' and '/' are not.
	WSBearerPrefix = "base64url.bearer.authorization.kubeswift.io."

	// WSProtocol is the subprotocol the server echoes back. A browser fails the
	// connection if it offers subprotocols and the server selects none, so a
	// client offering the bearer above MUST also offer this one.
	WSProtocol = "kubeswift.io"
)

// wsAuthHeader extracts the caller's bearer for a WebSocket upgrade and returns
// it as an http.Header suitable for Authenticate.
//
// Order of preference:
//
//  1. Sec-WebSocket-Protocol bearer — header-borne, never logged. What the UI sends.
//  2. Authorization — non-browser clients (swiftctl, curl) can set it directly.
//  3. ?token= — DEPRECATED, still accepted so a UI released before this change
//     keeps working against an upgraded gateway (the two are versioned
//     separately). deprecated is true in that case so the caller can warn.
func wsAuthHeader(r *http.Request) (hdr http.Header, deprecated bool) {
	hdr = http.Header{}

	for _, proto := range websocket.Subprotocols(r) {
		if !strings.HasPrefix(proto, WSBearerPrefix) {
			continue
		}
		enc := strings.TrimPrefix(proto, WSBearerPrefix)
		// RawURLEncoding = base64url, no padding. Fall back to the padded form
		// rather than dropping the credential on a client that adds '='.
		tok, err := base64.RawURLEncoding.DecodeString(enc)
		if err != nil {
			if tok, err = base64.URLEncoding.DecodeString(enc); err != nil {
				continue
			}
		}
		if len(tok) > 0 {
			hdr.Set("Authorization", "Bearer "+string(tok))
			return hdr, false
		}
	}

	if a := r.Header.Get("Authorization"); a != "" {
		hdr.Set("Authorization", a)
		return hdr, false
	}

	if tok := r.URL.Query().Get("token"); tok != "" {
		hdr.Set("Authorization", "Bearer "+tok)
		return hdr, true
	}

	return hdr, false
}

// wsUpgrader builds the upgrader for a raw-WS plane.
//
// Subprotocols is set so that gorilla echoes WSProtocol back when the client
// offers it. The bearer subprotocol is deliberately NOT in this list — it must
// never be reflected into the response.
func wsUpgrader(originAllowed func(*http.Request) bool) websocket.Upgrader {
	return websocket.Upgrader{
		Subprotocols: []string{WSProtocol},
		CheckOrigin:  originAllowed,
	}
}

// OriginPolicy decides whether a cross-origin WebSocket upgrade is acceptable.
//
// The old behaviour was an unconditional `return true`, justified in a comment
// by "the bearer token (not a cookie) is the auth". That reasoning holds for
// auth-mode=oidc and auth-mode=token: an attacker's page cannot read the token
// (localStorage is origin-scoped), so it cannot open an authenticated socket,
// and Origin buys little.
//
// It does NOT hold for auth-mode=insecure, where there is no authentication at
// all: any page the operator visits could open a console to any VM, or a
// sandbox exec shell, on a gateway their browser can reach. Origin is the only
// control left, so in that mode cross-origin is refused unless the operator
// listed the origin explicitly.
type OriginPolicy struct {
	allowed  map[string]bool
	wildcard bool
	// strict is true for auth-mode=insecure: never honour "*".
	strict bool
}

// NewOriginPolicy builds a policy from --cors-allow-origin and --auth-mode.
func NewOriginPolicy(corsAllowOrigin, authMode string) *OriginPolicy {
	p := &OriginPolicy{allowed: map[string]bool{}, strict: authMode == "insecure"}
	for _, o := range strings.Split(corsAllowOrigin, ",") {
		o = strings.ToLower(strings.TrimSpace(o))
		switch {
		case o == "":
		case o == "*":
			p.wildcard = true
		default:
			p.allowed[o] = true
		}
	}
	return p
}

// Allow reports whether the upgrade may proceed. Exported shape matches
// websocket.Upgrader.CheckOrigin.
func (p *OriginPolicy) Allow(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Not a browser: swiftctl, curl, a test. There is no origin to police,
		// and the bearer is still required.
		return true
	}
	if p.allowed[strings.ToLower(origin)] {
		return true
	}
	// Same-origin is always fine — this is the UI served from the gateway's own
	// host, or through the UI's same-origin nginx proxy.
	if u, err := url.Parse(origin); err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host) {
		return true
	}
	return p.wildcard && !p.strict
}
