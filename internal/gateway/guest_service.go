package gateway

import (
	"context"
	"errors"
	"sort"
	"sync"

	connect "connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	kubeswiftv1 "github.com/projectbeskar/kubeswift/gen/kubeswift/v1"
	"github.com/projectbeskar/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
)

// clientProvider is the subset of ClientPool the read plane needs: a per-member
// impersonating dynamic client and the set of registered members. Narrowing to
// an interface keeps GuestService unit-testable with a fake dynamic client.
type clientProvider interface {
	DynamicFor(cluster string, id Identity) (dynamic.Interface, error)
	Members() []string
}

// GuestService is the read plane. ListGuests / WatchGuests fan out across the
// selected member clusters, map each SwiftGuest to the flat proto row, and
// merge — stamping the cluster dimension (D2) on every row and surfacing a
// per-cluster error instead of failing the whole-fleet query (no silent
// failures). Every call impersonates the end user against members (D1).
type GuestService struct {
	kubeswiftv1connect.UnimplementedGuestServiceHandler

	pool clientProvider
	auth Authenticator
}

var _ kubeswiftv1connect.GuestServiceHandler = (*GuestService)(nil)

// NewGuestService wires the read plane to the client pool + the authenticator.
func NewGuestService(pool clientProvider, auth Authenticator) *GuestService {
	return &GuestService{pool: pool, auth: auth}
}

// ListGuests fans out to the selected clusters concurrently and merges the rows.
func (s *GuestService) ListGuests(ctx context.Context, req *connect.Request[kubeswiftv1.ListGuestsRequest]) (*connect.Response[kubeswiftv1.ListGuestsResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	clusters := s.targetClusters(req.Msg.GetClusters())
	resp := &kubeswiftv1.ListGuestsResponse{}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, cl := range clusters {
		wg.Add(1)
		go func(cl string) {
			defer wg.Done()
			guests, lerr := s.listOne(ctx, cl, id, req.Msg.GetNamespace(), req.Msg.GetPhase())
			mu.Lock()
			defer mu.Unlock()
			if lerr != nil {
				resp.Errors = append(resp.Errors, &kubeswiftv1.ClusterError{Cluster: cl, Message: lerr.Error()})
				return
			}
			resp.Guests = append(resp.Guests, guests...)
		}(cl)
	}
	wg.Wait()
	sortGuests(resp.Guests)
	return connect.NewResponse(resp), nil
}

func (s *GuestService) listOne(ctx context.Context, cluster string, id Identity, namespace, phase string) ([]*kubeswiftv1.Guest, error) {
	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		return nil, err
	}
	ul, err := guestResource(dyn, namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*kubeswiftv1.Guest, 0, len(ul.Items))
	for i := range ul.Items {
		g, cerr := toSwiftGuest(&ul.Items[i])
		if cerr != nil {
			continue
		}
		if phase != "" && string(g.Status.Phase) != phase {
			continue
		}
		out = append(out, guestToProto(cluster, g))
	}
	return out, nil
}

// WatchGuests multiplexes a per-cluster watch into one stream. A member whose
// watch cannot start (or errors) yields a BOOKMARK event carrying a
// ClusterError, so the UI shows a per-cluster problem rather than a dead stream.
func (s *GuestService) WatchGuests(ctx context.Context, req *connect.Request[kubeswiftv1.WatchGuestsRequest], stream *connect.ServerStream[kubeswiftv1.GuestEvent]) error {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return err
	}
	clusters := s.targetClusters(req.Msg.GetClusters())
	namespace := req.Msg.GetNamespace()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	events := make(chan *kubeswiftv1.GuestEvent, 256)
	var wg sync.WaitGroup
	for _, cl := range clusters {
		wg.Add(1)
		go func(cl string) {
			defer wg.Done()
			s.watchOne(ctx, cl, id, namespace, events)
		}(cl)
	}
	go func() { wg.Wait(); close(events) }()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

func (s *GuestService) watchOne(ctx context.Context, cluster string, id Identity, namespace string, out chan<- *kubeswiftv1.GuestEvent) {
	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		sendClusterErr(ctx, out, cluster, err)
		return
	}
	w, err := guestResource(dyn, namespace).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		sendClusterErr(ctx, out, cluster, err)
		return
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-w.ResultChan():
			if !ok {
				return
			}
			if ev := watchEventToProto(cluster, e); ev != nil {
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// GetGuestDetail returns the flat guest in P0; P1 enriches it with the launcher
// pod, recent Events, GPU, network, and storage in this one call.
func (s *GuestService) GetGuestDetail(ctx context.Context, req *connect.Request[kubeswiftv1.GetGuestDetailRequest]) (*connect.Response[kubeswiftv1.GetGuestDetailResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ref := req.Msg.GetRef()
	if ref == nil || ref.GetCluster() == "" || ref.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ref.cluster and ref.name are required"))
	}
	dyn, err := s.pool.DynamicFor(ref.GetCluster(), id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	u, err := dyn.Resource(swiftGuestGVR).Namespace(ref.GetNamespace()).Get(ctx, ref.GetName(), metav1.GetOptions{})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	g, err := toSwiftGuest(u)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kubeswiftv1.GetGuestDetailResponse{Guest: guestToProto(ref.GetCluster(), g)}), nil
}

// targetClusters resolves the selector against the registered members. An empty
// or all-clusters selector targets the whole fleet.
func (s *GuestService) targetClusters(sel *kubeswiftv1.ClusterSelector) []string {
	all := s.pool.Members()
	sort.Strings(all)
	if sel == nil || sel.GetAllClusters() || len(sel.GetClusters()) == 0 {
		return all
	}
	registered := make(map[string]bool, len(all))
	for _, m := range all {
		registered[m] = true
	}
	var out []string
	for _, c := range sel.GetClusters() {
		if registered[c] {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func guestResource(dyn dynamic.Interface, namespace string) dynamic.ResourceInterface {
	if namespace == "" {
		return dyn.Resource(swiftGuestGVR)
	}
	return dyn.Resource(swiftGuestGVR).Namespace(namespace)
}

func toSwiftGuest(u *unstructured.Unstructured) (*swiftv1alpha1.SwiftGuest, error) {
	var g swiftv1alpha1.SwiftGuest
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

func watchEventToProto(cluster string, e watch.Event) *kubeswiftv1.GuestEvent {
	var t kubeswiftv1.EventType
	switch e.Type {
	case watch.Added:
		t = kubeswiftv1.EventType_EVENT_TYPE_ADDED
	case watch.Modified:
		t = kubeswiftv1.EventType_EVENT_TYPE_MODIFIED
	case watch.Deleted:
		t = kubeswiftv1.EventType_EVENT_TYPE_DELETED
	default: // Bookmark / Error from a member watch: not forwarded as a guest row
		return nil
	}
	u, ok := e.Object.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	g, err := toSwiftGuest(u)
	if err != nil {
		return nil
	}
	return &kubeswiftv1.GuestEvent{Type: t, Guest: guestToProto(cluster, g)}
}

func sendClusterErr(ctx context.Context, out chan<- *kubeswiftv1.GuestEvent, cluster string, err error) {
	select {
	case out <- &kubeswiftv1.GuestEvent{
		Type:  kubeswiftv1.EventType_EVENT_TYPE_BOOKMARK,
		Error: &kubeswiftv1.ClusterError{Cluster: cluster, Message: err.Error()},
	}:
	case <-ctx.Done():
	}
}

func sortGuests(gs []*kubeswiftv1.Guest) {
	sort.Slice(gs, func(i, j int) bool {
		a, b := gs[i].GetRef(), gs[j].GetRef()
		if a.GetCluster() != b.GetCluster() {
			return a.GetCluster() < b.GetCluster()
		}
		if a.GetNamespace() != b.GetNamespace() {
			return a.GetNamespace() < b.GetNamespace()
		}
		return a.GetName() < b.GetName()
	})
}
