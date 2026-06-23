package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	connect "connectrpc.com/connect"
	"k8s.io/client-go/dynamic"

	kubeswiftv1 "github.com/projectbeskar/kubeswift/gen/kubeswift/v1"
)

// fakeProm serves a Prometheus query_range matrix with two points.
func fakeProm() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1700000000,"0.5"],[1700000030,"0.7"]]}]}}`))
	}))
}

func TestTelemetryService_GetGuestMetrics(t *testing.T) {
	prom := fakeProm()
	defer prom.Close()
	boba := fakeDyn(uGuest("default", "vm-a", "Running"), uPod("default", "vm-a", "vm-a"))
	prov := &fakeProvider{
		clients: map[string]dynamic.Interface{"boba": boba},
		prom:    map[string]string{"boba": prom.URL},
	}
	svc := NewTelemetryService(prov, NewInsecureAuthenticator())

	resp, err := svc.GetGuestMetrics(context.Background(), connect.NewRequest(&kubeswiftv1.GetGuestMetricsRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "vm-a"},
	}))
	if err != nil {
		t.Fatalf("GetGuestMetrics: %v", err)
	}
	if resp.Msg.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Msg.Error)
	}
	if len(resp.Msg.Series) != 4 {
		t.Fatalf("want 4 series (cpu/mem/rx/tx), got %d", len(resp.Msg.Series))
	}
	kinds := map[string]bool{}
	for _, s := range resp.Msg.Series {
		kinds[s.Kind] = true
		if len(s.Points) != 2 {
			t.Errorf("series %q: want 2 points, got %d", s.Kind, len(s.Points))
		}
	}
	for _, k := range []string{"cpu_cores", "memory_bytes", "net_rx_bps", "net_tx_bps"} {
		if !kinds[k] {
			t.Errorf("missing series %q", k)
		}
	}
}

func TestTelemetryService_NoEndpoint(t *testing.T) {
	boba := fakeDyn(uGuest("default", "vm-a", "Running"), uPod("default", "vm-a", "vm-a"))
	prov := &fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}} // no prom endpoint
	svc := NewTelemetryService(prov, NewInsecureAuthenticator())

	resp, err := svc.GetGuestMetrics(context.Background(), connect.NewRequest(&kubeswiftv1.GetGuestMetricsRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "vm-a"},
	}))
	if err != nil {
		t.Fatalf("GetGuestMetrics: %v", err)
	}
	// No silent empty chart: an unconfigured endpoint surfaces as a cluster error.
	if resp.Msg.Error == nil || resp.Msg.Error.Cluster != "boba" {
		t.Errorf("want a cluster error for the missing endpoint, got %+v", resp.Msg.Error)
	}
}

func TestTelemetryService_StoppedGuestNoPod(t *testing.T) {
	prom := fakeProm()
	defer prom.Close()
	boba := fakeDyn(uGuest("default", "vm-a", "Stopped")) // no launcher pod
	prov := &fakeProvider{
		clients: map[string]dynamic.Interface{"boba": boba},
		prom:    map[string]string{"boba": prom.URL},
	}
	svc := NewTelemetryService(prov, NewInsecureAuthenticator())

	resp, err := svc.GetGuestMetrics(context.Background(), connect.NewRequest(&kubeswiftv1.GetGuestMetricsRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "vm-a"},
	}))
	if err != nil {
		t.Fatalf("GetGuestMetrics: %v", err)
	}
	if len(resp.Msg.Series) != 0 || resp.Msg.Error != nil {
		t.Errorf("stopped guest should yield empty series + no error, got series=%d err=%v", len(resp.Msg.Series), resp.Msg.Error)
	}
}
