package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/dynamic"
)

// TestConsoleHandler_Preflight covers the HTTP pre-flight that runs before the
// WebSocket upgrade — so failures are readable HTTP errors, not a dropped
// socket. The exec bridge itself is validated on-cluster.
func TestConsoleHandler_Preflight(t *testing.T) {
	boba := fakeDyn(uGuest("default", "vm-a", "Running")) // a guest, but no launcher pod
	h := NewConsoleHandler(&fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}}, NewInsecureAuthenticator())

	cases := []struct {
		name, target string
		want         int
	}{
		{"missing params", "/console?cluster=boba", http.StatusBadRequest},
		{"unknown cluster", "/console?cluster=ghost&namespace=default&name=vm-a", http.StatusNotFound},
		{"no launcher pod", "/console?cluster=boba&namespace=default&name=vm-a", http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.target, nil))
			if w.Code != tc.want {
				t.Errorf("want %d, got %d (%s)", tc.want, w.Code, w.Body.String())
			}
		})
	}
}
