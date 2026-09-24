package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func corsReq(method, origin, host string) *http.Request {
	r := httptest.NewRequest(method, "http://"+host+"/kubeswift.v1.GuestService/DeleteGuest", nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// With auth-mode=insecure there is no token, so "*" let any page the
// operator visited drive every RPC -- mutating ones included. A browser
// request from an origin the policy does not allow is refused, preflight too,
// and an allowed one gets its own origin back, never "*".
func TestWithCORS_InsecureModeRefusesForeignOrigins(t *testing.T) {
	reached := false
	h := withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }),
		"*", NewOriginPolicy("*", "insecure"))

	for _, method := range []string{http.MethodOptions, http.MethodPost} {
		reached = false
		w := httptest.NewRecorder()
		h.ServeHTTP(w, corsReq(method, "https://evil.example.com", "gw.example.com"))
		if w.Code != http.StatusForbidden || reached {
			t.Errorf("%s from a foreign origin: code=%d reached=%v, want 403 and not served", method, w.Code, reached)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" {
			t.Errorf("%s: Access-Control-Allow-Origin = *", method)
		}
	}

	// A non-browser client (no Origin) and a loopback same-origin UI still work.
	for _, origin := range []string{"", "http://localhost:8080"} {
		reached = false
		w := httptest.NewRecorder()
		h.ServeHTTP(w, corsReq(http.MethodPost, origin, "localhost:8080"))
		if !reached {
			t.Errorf("origin %q was refused", origin)
		}
		if origin != "" && w.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("ACAO = %q, want the request's own origin", w.Header().Get("Access-Control-Allow-Origin"))
		}
	}
}

// With real authentication the wildcard stays: the bearer is the control.
func TestWithCORS_WildcardKeptWithAuth(t *testing.T) {
	h := withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), "*", NewOriginPolicy("*", "oidc"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, corsReq(http.MethodPost, "https://ui.example.com", "gw.example.com"))
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO = %q, want * under oidc", got)
	}
}
