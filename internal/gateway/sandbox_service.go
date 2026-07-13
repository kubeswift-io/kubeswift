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

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
	"github.com/kubeswift-io/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
)

// SandboxService is the read plane for the MicroVM inventory (SwiftSandbox +
// SwiftSandboxPool). List/Watch fan out across the selected members and merge,
// stamping the cluster dimension (D2) and surfacing a per-cluster error instead
// of failing the whole-fleet query (no silent failures). Every call impersonates
// the end user against members (D1). The write RPCs (Create/Delete) stay on the
// Unimplemented base until A3.
type SandboxService struct {
	kubeswiftv1connect.UnimplementedSandboxServiceHandler

	pool clientProvider
	auth Authenticator
}

var _ kubeswiftv1connect.SandboxServiceHandler = (*SandboxService)(nil)

// NewSandboxService wires the read plane to the client pool + the authenticator.
func NewSandboxService(pool clientProvider, auth Authenticator) *SandboxService {
	return &SandboxService{pool: pool, auth: auth}
}

// ListSandboxes fans out to the selected clusters concurrently and merges rows.
func (s *SandboxService) ListSandboxes(ctx context.Context, req *connect.Request[kubeswiftv1.ListSandboxesRequest]) (*connect.Response[kubeswiftv1.ListSandboxesResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	resp := &kubeswiftv1.ListSandboxesResponse{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, cl := range s.targetClusters(req.Msg.GetClusters()) {
		wg.Add(1)
		go func(cl string) {
			defer wg.Done()
			rows, lerr := s.listSandboxesOne(ctx, cl, id, req.Msg.GetNamespace(), req.Msg.GetPhase())
			mu.Lock()
			defer mu.Unlock()
			if lerr != nil {
				resp.Errors = append(resp.Errors, &kubeswiftv1.ClusterError{Cluster: cl, Message: lerr.Error()})
				return
			}
			resp.Sandboxes = append(resp.Sandboxes, rows...)
		}(cl)
	}
	wg.Wait()
	sort.Slice(resp.Sandboxes, func(i, j int) bool { return refLess(resp.Sandboxes[i].GetRef(), resp.Sandboxes[j].GetRef()) })
	return connect.NewResponse(resp), nil
}

func (s *SandboxService) listSandboxesOne(ctx context.Context, cluster string, id Identity, namespace, phase string) ([]*kubeswiftv1.Sandbox, error) {
	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		return nil, err
	}
	ul, err := sandboxResource(dyn, namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*kubeswiftv1.Sandbox, 0, len(ul.Items))
	for i := range ul.Items {
		sb, cerr := toSwiftSandbox(&ul.Items[i])
		if cerr != nil {
			continue
		}
		if phase != "" && string(sb.Status.Phase) != phase {
			continue
		}
		out = append(out, sandboxToProto(cluster, sb))
	}
	return out, nil
}

// ListSandboxPools fans out over the warm pools.
func (s *SandboxService) ListSandboxPools(ctx context.Context, req *connect.Request[kubeswiftv1.ListSandboxPoolsRequest]) (*connect.Response[kubeswiftv1.ListSandboxPoolsResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	resp := &kubeswiftv1.ListSandboxPoolsResponse{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, cl := range s.targetClusters(req.Msg.GetClusters()) {
		wg.Add(1)
		go func(cl string) {
			defer wg.Done()
			rows, lerr := s.listPoolsOne(ctx, cl, id, req.Msg.GetNamespace())
			mu.Lock()
			defer mu.Unlock()
			if lerr != nil {
				resp.Errors = append(resp.Errors, &kubeswiftv1.ClusterError{Cluster: cl, Message: lerr.Error()})
				return
			}
			resp.Pools = append(resp.Pools, rows...)
		}(cl)
	}
	wg.Wait()
	sort.Slice(resp.Pools, func(i, j int) bool { return refLess(resp.Pools[i].GetRef(), resp.Pools[j].GetRef()) })
	return connect.NewResponse(resp), nil
}

func (s *SandboxService) listPoolsOne(ctx context.Context, cluster string, id Identity, namespace string) ([]*kubeswiftv1.SandboxPool, error) {
	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		return nil, err
	}
	ul, err := sandboxPoolResource(dyn, namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*kubeswiftv1.SandboxPool, 0, len(ul.Items))
	for i := range ul.Items {
		p, cerr := toSwiftSandboxPool(&ul.Items[i])
		if cerr != nil {
			continue
		}
		out = append(out, sandboxPoolToProto(cluster, p))
	}
	return out, nil
}

// WatchSandboxes multiplexes a per-cluster watch into one stream. A member whose
// watch cannot start yields a BOOKMARK event carrying a ClusterError (a
// per-cluster problem, not a dead stream) — sandboxes are ephemeral and their
// phase moves fast, so live updates keep the inventory current.
func (s *SandboxService) WatchSandboxes(ctx context.Context, req *connect.Request[kubeswiftv1.WatchSandboxesRequest], stream *connect.ServerStream[kubeswiftv1.SandboxEvent]) error {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return err
	}
	namespace := req.Msg.GetNamespace()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	events := make(chan *kubeswiftv1.SandboxEvent, 256)
	var wg sync.WaitGroup
	for _, cl := range s.targetClusters(req.Msg.GetClusters()) {
		wg.Add(1)
		go func(cl string) {
			defer wg.Done()
			s.watchSandboxesOne(ctx, cl, id, namespace, events)
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

func (s *SandboxService) watchSandboxesOne(ctx context.Context, cluster string, id Identity, namespace string, out chan<- *kubeswiftv1.SandboxEvent) {
	sendErr := func(err error) {
		select {
		case out <- &kubeswiftv1.SandboxEvent{
			Type:  kubeswiftv1.EventType_EVENT_TYPE_BOOKMARK,
			Error: &kubeswiftv1.ClusterError{Cluster: cluster, Message: err.Error()},
		}:
		case <-ctx.Done():
		}
	}
	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		sendErr(err)
		return
	}
	w, err := sandboxResource(dyn, namespace).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		sendErr(err)
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
			if ev := sandboxWatchEventToProto(cluster, e); ev != nil {
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// GetSandboxDetail returns the flat sandbox + its structured spec (for the
// drawer + Clone).
func (s *SandboxService) GetSandboxDetail(ctx context.Context, req *connect.Request[kubeswiftv1.GetSandboxDetailRequest]) (*connect.Response[kubeswiftv1.GetSandboxDetailResponse], error) {
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
	u, err := dyn.Resource(swiftSandboxGVR).Namespace(ref.GetNamespace()).Get(ctx, ref.GetName(), metav1.GetOptions{})
	if err != nil {
		return nil, mapAccessErr(err)
	}
	sb, err := toSwiftSandbox(u)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kubeswiftv1.GetSandboxDetailResponse{
		Sandbox: sandboxToProto(ref.GetCluster(), sb),
		Spec:    sandboxSpecToProto(sb),
	}), nil
}

// GetSandboxPoolDetail returns one warm pool.
func (s *SandboxService) GetSandboxPoolDetail(ctx context.Context, req *connect.Request[kubeswiftv1.GetSandboxPoolDetailRequest]) (*connect.Response[kubeswiftv1.GetSandboxPoolDetailResponse], error) {
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
	u, err := dyn.Resource(swiftSandboxPoolGVR).Namespace(ref.GetNamespace()).Get(ctx, ref.GetName(), metav1.GetOptions{})
	if err != nil {
		return nil, mapAccessErr(err)
	}
	p, err := toSwiftSandboxPool(u)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kubeswiftv1.GetSandboxPoolDetailResponse{
		Pool: sandboxPoolToProto(ref.GetCluster(), p),
	}), nil
}

// targetClusters resolves the selector against the registered members (mirrors
// the guest/migration read planes — per-service by convention).
func (s *SandboxService) targetClusters(sel *kubeswiftv1.ClusterSelector) []string {
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

func sandboxResource(dyn dynamic.Interface, namespace string) dynamic.ResourceInterface {
	if namespace == "" {
		return dyn.Resource(swiftSandboxGVR)
	}
	return dyn.Resource(swiftSandboxGVR).Namespace(namespace)
}

func sandboxPoolResource(dyn dynamic.Interface, namespace string) dynamic.ResourceInterface {
	if namespace == "" {
		return dyn.Resource(swiftSandboxPoolGVR)
	}
	return dyn.Resource(swiftSandboxPoolGVR).Namespace(namespace)
}

func toSwiftSandbox(u *unstructured.Unstructured) (*sandboxv1alpha1.SwiftSandbox, error) {
	var sb sandboxv1alpha1.SwiftSandbox
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

func toSwiftSandboxPool(u *unstructured.Unstructured) (*sandboxv1alpha1.SwiftSandboxPool, error) {
	var p sandboxv1alpha1.SwiftSandboxPool
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func sandboxWatchEventToProto(cluster string, e watch.Event) *kubeswiftv1.SandboxEvent {
	var t kubeswiftv1.EventType
	switch e.Type {
	case watch.Added:
		t = kubeswiftv1.EventType_EVENT_TYPE_ADDED
	case watch.Modified:
		t = kubeswiftv1.EventType_EVENT_TYPE_MODIFIED
	case watch.Deleted:
		t = kubeswiftv1.EventType_EVENT_TYPE_DELETED
	default: // Bookmark / Error from a member watch: not a sandbox row
		return nil
	}
	u, ok := e.Object.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	sb, err := toSwiftSandbox(u)
	if err != nil {
		return nil
	}
	return &kubeswiftv1.SandboxEvent{Type: t, Sandbox: sandboxToProto(cluster, sb)}
}

// refLess orders rows by (cluster, namespace, name) — the stable inventory sort
// shared by the sandbox list responses.
func refLess(a, b *kubeswiftv1.ObjectRef) bool {
	if a.GetCluster() != b.GetCluster() {
		return a.GetCluster() < b.GetCluster()
	}
	if a.GetNamespace() != b.GetNamespace() {
		return a.GetNamespace() < b.GetNamespace()
	}
	return a.GetName() < b.GetName()
}
