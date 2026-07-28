package gateway

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func wsReq(target string, protos ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	// Subprotocols are only parsed on a request that looks like an upgrade.
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	for _, p := range protos {
		r.Header.Add("Sec-WebSocket-Protocol", p)
	}
	return r
}

func TestWSAuthHeader_SubprotocolBearer(t *testing.T) {
	tok := "eyJhbGciOi.PAYLOAD.SIG"
	enc := base64.RawURLEncoding.EncodeToString([]byte(tok))
	r := wsReq("/console?cluster=c&namespace=n&name=g", WSBearerPrefix+enc, WSProtocol)

	hdr, deprecated := wsAuthHeader(r)
	if got, want := hdr.Get("Authorization"), "Bearer "+tok; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if deprecated {
		t.Error("subprotocol bearer must not be reported as deprecated")
	}
}

func TestWSAuthHeader_TokenNeverNeedsTheQueryString(t *testing.T) {
	// The whole point: a client using the subprotocol form puts NOTHING
	// sensitive in the URL, so no access log on the path can capture it.
	tok := "secret-token"
	enc := base64.RawURLEncoding.EncodeToString([]byte(tok))
	r := wsReq("/sandbox-exec?cluster=c&namespace=n&name=s", WSBearerPrefix+enc, WSProtocol)

	if r.URL.RawQuery == "" {
		t.Fatal("test setup: expected a query string")
	}
	if got := r.URL.String(); strings.Contains(got, tok) {
		t.Fatalf("token leaked into the URL: %q", got)
	}
	hdr, _ := wsAuthHeader(r)
	if hdr.Get("Authorization") == "" {
		t.Fatal("token not recovered from the subprotocol")
	}
}

func TestWSAuthHeader_PaddedBase64StillAccepted(t *testing.T) {
	tok := "abcd" // encodes to a padded value under URLEncoding
	r := wsReq("/console", WSBearerPrefix+base64.URLEncoding.EncodeToString([]byte(tok)), WSProtocol)
	hdr, _ := wsAuthHeader(r)
	if got, want := hdr.Get("Authorization"), "Bearer "+tok; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestWSAuthHeader_QueryFallbackFlaggedDeprecated(t *testing.T) {
	// A UI released before this change still works, but the caller is told so
	// it can warn — the query form is what leaks into logs.
	r := wsReq("/console?token=legacy-token")
	hdr, deprecated := wsAuthHeader(r)
	if got, want := hdr.Get("Authorization"), "Bearer legacy-token"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if !deprecated {
		t.Error("?token= must be reported as deprecated")
	}
}

func TestWSAuthHeader_SubprotocolBeatsQuery(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString([]byte("header-token"))
	r := wsReq("/console?token=query-token", WSBearerPrefix+enc, WSProtocol)
	hdr, deprecated := wsAuthHeader(r)
	if got, want := hdr.Get("Authorization"), "Bearer header-token"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if deprecated {
		t.Error("should not warn when the header form was used")
	}
}

func TestWSAuthHeader_NoCredential(t *testing.T) {
	hdr, deprecated := wsAuthHeader(wsReq("/console"))
	if hdr.Get("Authorization") != "" {
		t.Error("invented a credential")
	}
	if deprecated {
		t.Error("nothing supplied is not a deprecated form")
	}
}

func TestWSAuthHeader_GarbageSubprotocolIsIgnored(t *testing.T) {
	r := wsReq("/console", WSBearerPrefix+"!!!not-base64!!!", WSProtocol)
	hdr, _ := wsAuthHeader(r)
	if hdr.Get("Authorization") != "" {
		t.Error("undecodable bearer must not produce an Authorization header")
	}
}

// --- origin policy ---

func originReq(origin, host string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+"/console", nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func TestOriginPolicy_NoOriginIsANonBrowserClient(t *testing.T) {
	// swiftctl / curl send no Origin. The bearer is still required.
	for _, mode := range []string{"oidc", "insecure"} {
		p := NewOriginPolicy("https://ui.example.com", mode)
		if !p.Allow(originReq("", "gw.example.com")) {
			t.Errorf("mode %s: rejected a request with no Origin", mode)
		}
	}
}

func TestOriginPolicy_SameOriginAlwaysAllowed(t *testing.T) {
	p := NewOriginPolicy("https://ui.example.com", "insecure")
	if !p.Allow(originReq("https://gw.example.com", "gw.example.com")) {
		t.Error("same-origin upgrade rejected")
	}
}

func TestOriginPolicy_ExplicitAllowlist(t *testing.T) {
	p := NewOriginPolicy("https://ui.example.com, https://other.example.com", "oidc")
	if !p.Allow(originReq("https://other.example.com", "gw.example.com")) {
		t.Error("listed origin rejected")
	}
	if p.Allow(originReq("https://evil.example.com", "gw.example.com")) {
		t.Error("unlisted origin accepted despite an explicit allowlist")
	}
}

func TestOriginPolicy_InsecureModeRefusesWildcard(t *testing.T) {
	// THE case this exists for. auth-mode=insecure performs no authentication,
	// so any page the operator visits could otherwise open a console to a VM or
	// a sandbox exec shell. Origin is the only control left.
	p := NewOriginPolicy("*", "insecure")
	if p.Allow(originReq("https://evil.example.com", "gw.example.com")) {
		t.Fatal("cross-origin upgrade accepted with auth-mode=insecure and CORS=*")
	}
}

func TestOriginPolicy_WildcardHonouredWhenAuthenticated(t *testing.T) {
	// With a real auth mode the bearer is the control and the attacker's page
	// cannot read it, so "*" stays honoured — no behaviour change for existing
	// oidc/token deployments.
	p := NewOriginPolicy("*", "oidc")
	if !p.Allow(originReq("https://anything.example.com", "gw.example.com")) {
		t.Error("wildcard not honoured under auth-mode=oidc")
	}
}
