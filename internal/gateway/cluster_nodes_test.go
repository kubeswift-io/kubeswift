package gateway

import (
	"context"
	"testing"

	connect "connectrpc.com/connect"
	"k8s.io/client-go/dynamic"

	kubeswiftv1 "github.com/projectbeskar/kubeswift/gen/kubeswift/v1"
)

func TestClusterService_ListNodes(t *testing.T) {
	boba := fakeDyn(uNode("worker-1", true, true), uNode("worker-2", true, false)) // worker-2 cordoned
	svc := NewClusterService(nil, "kubeswift-system", nil,
		&fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}}, NewInsecureAuthenticator())

	resp, err := svc.ListNodes(context.Background(), connect.NewRequest(&kubeswiftv1.ListNodesRequest{Cluster: "boba"}))
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if resp.Msg.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Msg.Error)
	}
	if len(resp.Msg.Nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(resp.Msg.Nodes))
	}
	sched := map[string]bool{}
	for _, n := range resp.Msg.Nodes {
		sched[n.Name] = n.Schedulable
	}
	if !sched["worker-1"] || sched["worker-2"] {
		t.Errorf("schedulable wrong (worker-2 is cordoned): %v", sched)
	}

	// An unknown cluster surfaces a ClusterError, not a fatal.
	bad, _ := svc.ListNodes(context.Background(), connect.NewRequest(&kubeswiftv1.ListNodesRequest{Cluster: "ghost"}))
	if bad.Msg.Error == nil {
		t.Error("unknown cluster should surface a ClusterError")
	}
}
