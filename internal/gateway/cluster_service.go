package gateway

import (
	"context"

	connect "connectrpc.com/connect"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/kubeswift-io/kubeswift/api/fleet/v1alpha1"
	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
	"github.com/kubeswift-io/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
)

// ClusterService serves the hub's fleet registry to the UI cluster selector.
// It reads fleet.kubeswift.io Cluster objects from the hub cache (List) and
// streams live changes via the shared ClusterWatcher (Watch). This is the only
// service that reads hub-local state; the guest read plane (PR C2) fans out to
// member clusters through the client pool.
type ClusterService struct {
	kubeswiftv1connect.UnimplementedClusterServiceHandler

	hub       client.Reader
	namespace string
	watcher   *ClusterWatcher
	// pool + auth power ListNodes, which fans out to a member (the migrate
	// target picker); ListClusters/WatchClusters use only the hub cache above.
	pool nodeProvider
	auth Authenticator
}

// nodeProvider is the slice of ClientPool ListNodes needs.
type nodeProvider interface {
	DynamicFor(cluster string, id Identity) (dynamic.Interface, error)
}

var _ kubeswiftv1connect.ClusterServiceHandler = (*ClusterService)(nil)

// NewClusterService wires the service to the hub cache + the shared watcher,
// plus the client pool + authenticator for ListNodes.
func NewClusterService(hub client.Reader, namespace string, watcher *ClusterWatcher, pool nodeProvider, auth Authenticator) *ClusterService {
	return &ClusterService{hub: hub, namespace: namespace, watcher: watcher, pool: pool, auth: auth}
}

// ListClusters returns every registered member cluster. The fleet is small, so
// P0 does not paginate (the page field is reserved for later scale).
func (s *ClusterService) ListClusters(ctx context.Context, _ *connect.Request[kubeswiftv1.ListClustersRequest]) (*connect.Response[kubeswiftv1.ListClustersResponse], error) {
	var list fleetv1alpha1.ClusterList
	if err := s.hub.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &kubeswiftv1.ListClustersResponse{}
	for i := range list.Items {
		resp.Clusters = append(resp.Clusters, clusterToProto(&list.Items[i]))
	}
	return connect.NewResponse(resp), nil
}

// WatchClusters streams an initial ADDED snapshot followed by live deltas.
// It subscribes BEFORE listing so no change is missed in the list→watch gap;
// any resulting duplicate is harmless because the UI upserts by cluster name.
func (s *ClusterService) WatchClusters(ctx context.Context, _ *connect.Request[kubeswiftv1.WatchClustersRequest], stream *connect.ServerStream[kubeswiftv1.ClusterEvent]) error {
	sub := s.watcher.subscribe()
	defer s.watcher.unsubscribe(sub)

	var list fleetv1alpha1.ClusterList
	if err := s.hub.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	for i := range list.Items {
		if err := stream.Send(&kubeswiftv1.ClusterEvent{
			Type:    kubeswiftv1.EventType_EVENT_TYPE_ADDED,
			Cluster: clusterToProto(&list.Items[i]),
		}); err != nil {
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-sub:
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}
