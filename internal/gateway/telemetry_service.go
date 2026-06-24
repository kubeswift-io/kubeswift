package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	connect "connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	kubeswiftv1 "github.com/projectbeskar/kubeswift/gen/kubeswift/v1"
	"github.com/projectbeskar/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
)

// metricsProvider is the subset of ClientPool the telemetry plane needs: the
// member's impersonating client (to resolve the guest's launcher pod, which
// also authorizes the caller) and the member's Prometheus base URL.
type metricsProvider interface {
	DynamicFor(cluster string, id Identity) (dynamic.Interface, error)
	PrometheusEndpoint(cluster string) string
}

// TelemetryService serves per-VM range metrics. It resolves the guest's current
// launcher pod (by the swift.kubeswift.io/guest label — robust to the
// <guest>-mig-<uid> rename) and range-queries the member cluster's Prometheus
// (cAdvisor). The launcher cgroup IS the VM — Cloud Hypervisor runs in it.
//
// It queries by the resolved pod name rather than joining kube-state-metrics on
// the guest label, so it needs no KSM metric-labels allowlist. The trade-off is
// that history does not span a live migration's pod rename; that continuity is a
// v2 upgrade (KSM allowlist + label join).
type TelemetryService struct {
	kubeswiftv1connect.UnimplementedTelemetryServiceHandler
	pool metricsProvider
	auth Authenticator
	http *http.Client
}

func NewTelemetryService(pool metricsProvider, auth Authenticator) *TelemetryService {
	return &TelemetryService{pool: pool, auth: auth, http: &http.Client{Timeout: 15 * time.Second}}
}

// promSeries pairs a metric kind/unit with the PromQL that computes it.
type promSeries struct {
	kind, unit, query string
}

func (s *TelemetryService) GetGuestMetrics(ctx context.Context, req *connect.Request[kubeswiftv1.GetGuestMetricsRequest]) (*connect.Response[kubeswiftv1.GetGuestMetricsResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ref := req.Msg.GetRef()
	if ref == nil || ref.GetCluster() == "" || ref.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ref.cluster and ref.name are required"))
	}

	endpoint := strings.TrimRight(s.pool.PrometheusEndpoint(ref.GetCluster()), "/")
	if endpoint == "" {
		// No silent empty chart — say why telemetry is unavailable.
		return clusterMetricsErr(ref.GetCluster(), "no prometheusEndpoint registered for this cluster"), nil
	}

	// Resolve the guest's current launcher pod(s) by label (authz via the
	// impersonating client — the caller can only chart pods it may list).
	dyn, err := s.pool.DynamicFor(ref.GetCluster(), id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	pods, err := dyn.Resource(podGVR).Namespace(ref.GetNamespace()).
		List(ctx, metav1.ListOptions{LabelSelector: guestPodLabel + "=" + ref.GetName()})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if len(pods.Items) == 0 {
		// Stopped guest — no pod, no series. Not an error.
		return connect.NewResponse(&kubeswiftv1.GetGuestMetricsResponse{}), nil
	}
	names := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		names = append(names, pods.Items[i].GetName())
	}
	podRe := strings.Join(names, "|")
	ns := ref.GetNamespace()
	const rateWin = "2m"

	window := time.Duration(orDefaultInt32(req.Msg.GetWindowSeconds(), 900)) * time.Second
	step := time.Duration(orDefaultInt32(req.Msg.GetStepSeconds(), 30)) * time.Second
	end := time.Now()
	start := end.Add(-window)

	defs := []promSeries{
		{"cpu_cores", "cores", fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s",container!=""}[%s]))`, ns, podRe, rateWin)},
		{"memory_bytes", "bytes", fmt.Sprintf(`sum(container_memory_working_set_bytes{namespace="%s",pod=~"%s",container!=""})`, ns, podRe)},
		{"net_rx_bps", "bytes/sec", fmt.Sprintf(`sum(rate(container_network_receive_bytes_total{namespace="%s",pod=~"%s"}[%s]))`, ns, podRe, rateWin)},
		{"net_tx_bps", "bytes/sec", fmt.Sprintf(`sum(rate(container_network_transmit_bytes_total{namespace="%s",pod=~"%s"}[%s]))`, ns, podRe, rateWin)},
	}

	out := &kubeswiftv1.GetGuestMetricsResponse{}
	for _, d := range defs {
		pts, err := s.queryRange(ctx, endpoint, d.query, start, end, step)
		if err != nil {
			return clusterMetricsErr(ref.GetCluster(), fmt.Sprintf("prometheus query failed: %v", err)), nil
		}
		out.Series = append(out.Series, &kubeswiftv1.MetricSeries{Kind: d.kind, Unit: d.unit, Points: pts})
	}
	return connect.NewResponse(out), nil
}

// queryRange runs one Prometheus query_range and flattens the (single,
// aggregated) result series into points.
func (s *TelemetryService) queryRange(ctx context.Context, endpoint, query string, start, end time.Time, step time.Duration) ([]*kubeswiftv1.MetricPoint, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.Itoa(int(step.Seconds())))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned %s", resp.Status)
	}
	var pr promRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("prometheus status %q", pr.Status)
	}
	var pts []*kubeswiftv1.MetricPoint
	for _, res := range pr.Data.Result {
		for _, v := range res.Values {
			if len(v) != 2 {
				continue
			}
			tsF, ok := v[0].(float64)
			if !ok {
				continue
			}
			valS, ok := v[1].(string)
			if !ok {
				continue
			}
			f, err := strconv.ParseFloat(valS, 64)
			if err != nil {
				continue // NaN / Inf — skip the point, don't fail the chart
			}
			pts = append(pts, &kubeswiftv1.MetricPoint{Ts: timestamppb.New(time.Unix(int64(tsF), 0)), Value: f})
		}
	}
	return pts, nil
}

// promRangeResponse is the subset of Prometheus' query_range JSON we consume.
type promRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][]interface{}   `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func orDefaultInt32(v, def int32) int32 {
	if v <= 0 {
		return def
	}
	return v
}

func clusterMetricsErr(cluster, msg string) *connect.Response[kubeswiftv1.GetGuestMetricsResponse] {
	return connect.NewResponse(&kubeswiftv1.GetGuestMetricsResponse{
		Error: &kubeswiftv1.ClusterError{Cluster: cluster, Message: msg},
	})
}

// GetNodeMetrics range-queries a node's utilization. It resolves the node's
// InternalIP (node-exporter's `instance` is <ip>:9100), then queries
// node-exporter for CPU/mem/net and DCGM for GPU. GPU yields no points where
// DCGM isn't installed — a graceful empty series, not an error. The CPU/mem/GPU
// series are 0–1 utilization ratios; net is bytes/sec.
func (s *TelemetryService) GetNodeMetrics(ctx context.Context, req *connect.Request[kubeswiftv1.GetNodeMetricsRequest]) (*connect.Response[kubeswiftv1.GetNodeMetricsResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster, node := req.Msg.GetCluster(), req.Msg.GetNode()
	if cluster == "" || node == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster and node are required"))
	}
	endpoint := strings.TrimRight(s.pool.PrometheusEndpoint(cluster), "/")
	if endpoint == "" {
		return nodeMetricsErr(cluster, "no prometheusEndpoint registered for this cluster"), nil
	}
	// Resolve the node's InternalIP as the impersonated user (authz: the caller
	// can only chart a node it may get).
	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	nodeObj, err := dyn.Resource(nodeGVR).Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return nil, mapAccessErr(err)
	}
	ip := nodeInternalIP(nodeObj)
	if ip == "" {
		return nodeMetricsErr(cluster, "node has no InternalIP"), nil
	}
	inst := regexp.QuoteMeta(ip) + ":.*"
	nodeRe := regexp.QuoteMeta(node)

	const rateWin = "2m"
	window := time.Duration(orDefaultInt32(req.Msg.GetWindowSeconds(), 900)) * time.Second
	step := time.Duration(orDefaultInt32(req.Msg.GetStepSeconds(), 30)) * time.Second
	end := time.Now()
	start := end.Add(-window)

	defs := []promSeries{
		{"cpu_util", "ratio", fmt.Sprintf(`1 - avg(rate(node_cpu_seconds_total{mode="idle",instance=~"%s"}[%s]))`, inst, rateWin)},
		{"mem_util", "ratio", fmt.Sprintf(`1 - sum(node_memory_MemAvailable_bytes{instance=~"%s"}) / sum(node_memory_MemTotal_bytes{instance=~"%s"})`, inst, inst)},
		{"net_rx_bps", "bytes/sec", fmt.Sprintf(`sum(rate(node_network_receive_bytes_total{instance=~"%s",device!="lo"}[%s]))`, inst, rateWin)},
		{"net_tx_bps", "bytes/sec", fmt.Sprintf(`sum(rate(node_network_transmit_bytes_total{instance=~"%s",device!="lo"}[%s]))`, inst, rateWin)},
		// DCGM util is 0–100 per GPU; average across the node's GPUs ÷ 100.
		// dcgm-exporter labels by Hostname (the node) and/or kubernetes_node —
		// match either so it works with the standalone exporter or the GPU Operator.
		{"gpu_util", "ratio", fmt.Sprintf(`avg(DCGM_FI_DEV_GPU_UTIL{Hostname=~"%s.*"} or DCGM_FI_DEV_GPU_UTIL{kubernetes_node="%s"}) / 100`, nodeRe, node)},
	}

	out := &kubeswiftv1.GetNodeMetricsResponse{}
	for _, d := range defs {
		pts, err := s.queryRange(ctx, endpoint, d.query, start, end, step)
		if err != nil {
			return nodeMetricsErr(cluster, fmt.Sprintf("prometheus query failed: %v", err)), nil
		}
		out.Series = append(out.Series, &kubeswiftv1.MetricSeries{Kind: d.kind, Unit: d.unit, Points: pts})
	}
	return connect.NewResponse(out), nil
}

func nodeInternalIP(n *unstructured.Unstructured) string {
	addrs, _, _ := unstructured.NestedSlice(n.Object, "status", "addresses")
	for _, a := range addrs {
		if m, ok := a.(map[string]interface{}); ok && m["type"] == "InternalIP" {
			ip, _ := m["address"].(string)
			return ip
		}
	}
	return ""
}

func nodeMetricsErr(cluster, msg string) *connect.Response[kubeswiftv1.GetNodeMetricsResponse] {
	return connect.NewResponse(&kubeswiftv1.GetNodeMetricsResponse{
		Error: &kubeswiftv1.ClusterError{Cluster: cluster, Message: msg},
	})
}
