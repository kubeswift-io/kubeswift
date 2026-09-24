package gateway_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	connect "connectrpc.com/connect"

	"github.com/kubeswift-io/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
	"github.com/kubeswift-io/kubeswift/internal/gateway"
)

// A Connect handler built with the gateway's read cap must refuse an oversized
// request body rather than buffering it. Connect reads (and would gunzip) the
// whole message before dispatch — and before auth — so without the cap a small
// gzip body could inflate to gigabytes and OOM the gateway. The Unimplemented
// handler needs no dependencies; the point is that the cap rejects the message
// before the handler runs.
func TestConnectReadMaxBytes_RejectsOversizedRequest(t *testing.T) {
	path, h := kubeswiftv1connect.NewConsoleServiceHandler(
		kubeswiftv1connect.UnimplementedConsoleServiceHandler{},
		connect.WithReadMaxBytes(gateway.MaxRequestBytes),
	)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// A body well over the cap. Valid JSON envelope, padded past MaxRequestBytes.
	body := `{"guestRef":"` + strings.Repeat("A", gateway.MaxRequestBytes+(1<<20)) + `"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+path+"/OpenConsole", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("oversized request returned 200; want a rejection. body=%s", rb)
	}
	// Connect maps the over-limit read to resource_exhausted; assert we did not
	// instead reach the handler (which would answer unimplemented).
	if strings.Contains(string(rb), "unimplemented") {
		t.Errorf("oversized request reached the handler (got unimplemented); the read cap did not fire. body=%s", rb)
	}
	if !strings.Contains(string(rb), "resource_exhausted") {
		t.Errorf("expected a resource_exhausted rejection, got status=%d body=%s", resp.StatusCode, rb)
	}
}
