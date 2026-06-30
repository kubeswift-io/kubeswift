package gateway

import (
	"context"
	"errors"

	connect "connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
)

// ListNodes lists a member cluster's nodes for the migrate target picker. It
// fans out to the one named cluster as the impersonated user; an unknown or
// unreachable cluster surfaces as a ClusterError, not a silent empty list.
func (s *ClusterService) ListNodes(ctx context.Context, req *connect.Request[kubeswiftv1.ListNodesRequest]) (*connect.Response[kubeswiftv1.ListNodesResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster := req.Msg.GetCluster()
	if cluster == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster is required"))
	}
	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		return nodesErr(cluster, err.Error()), nil
	}
	list, err := dyn.Resource(nodeGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nodesErr(cluster, err.Error()), nil
	}
	out := &kubeswiftv1.ListNodesResponse{}
	for i := range list.Items {
		n := &list.Items[i]
		out.Nodes = append(out.Nodes, &kubeswiftv1.Node{
			Cluster:     cluster,
			Name:        n.GetName(),
			Ready:       nodeReady(n),
			Schedulable: !nestedBool(n, "spec", "unschedulable"),
		})
	}
	return connect.NewResponse(out), nil
}

func nodeReady(n *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(n.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == "Ready" {
			return m["status"] == "True"
		}
	}
	return false
}

func nestedBool(n *unstructured.Unstructured, fields ...string) bool {
	b, _, _ := unstructured.NestedBool(n.Object, fields...)
	return b
}

func nodesErr(cluster, msg string) *connect.Response[kubeswiftv1.ListNodesResponse] {
	return connect.NewResponse(&kubeswiftv1.ListNodesResponse{
		Error: &kubeswiftv1.ClusterError{Cluster: cluster, Message: msg},
	})
}
