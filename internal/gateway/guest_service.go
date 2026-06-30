package gateway

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	connect "connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
	"github.com/kubeswift-io/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
	"github.com/kubeswift-io/kubeswift/internal/actions"
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
		return nil, mapAccessErr(err) // RBAC 403 -> PermissionDenied, not Internal
	}
	g, err := toSwiftGuest(u)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kubeswiftv1.GetGuestDetailResponse{
		Guest:   guestToProto(ref.GetCluster(), g),
		Spec:    guestSpecToProto(g),
		Network: guestNetworkToProto(g),
	}), nil
}

// guestNetworkToProto surfaces the service-exposure + egress view (the Networking
// drawer section): spec.network (binding/ports) joined with the status the
// controller + swiftletd report (programmed ports, Service, egress reachability,
// conditions). Returns nil when the guest declares no spec.network.
func guestNetworkToProto(g *swiftv1alpha1.SwiftGuest) *kubeswiftv1.GuestNetwork {
	if g.Spec.Network == nil {
		return nil
	}
	n := &kubeswiftv1.GuestNetwork{
		Binding:         g.Spec.Network.Binding,
		PortsProgrammed: meta.IsStatusConditionTrue(g.Status.Conditions, swiftv1alpha1.ConditionPortsProgrammed),
		ServiceReady:    meta.IsStatusConditionTrue(g.Status.Conditions, swiftv1alpha1.ConditionServiceReady),
		EgressReady:     meta.IsStatusConditionTrue(g.Status.Conditions, swiftv1alpha1.ConditionEgressReady),
	}
	if g.Status.Network != nil {
		n.Egress = g.Status.Network.Egress
		if g.Status.Network.ServiceRef != nil {
			n.ServiceRef = g.Status.Network.ServiceRef.Name
		}
	}
	// Programmed set = the ports the controller echoed into status.exposedPorts.
	programmed := map[string]bool{}
	if g.Status.Network != nil {
		for _, ep := range g.Status.Network.ExposedPorts {
			programmed[portKey(ep.Name, ep.Port)] = true
		}
	}
	for _, p := range g.Spec.Network.Ports {
		n.Ports = append(n.Ports, &kubeswiftv1.GuestNetworkPort{
			Name:       p.Name,
			Port:       p.Port,
			TargetPort: p.TargetPort,
			Protocol:   string(p.Protocol),
			Expose:     p.Expose,
			Programmed: programmed[portKey(p.Name, p.Port)],
		})
	}
	return n
}

func portKey(name string, port int32) string {
	return name + "/" + strconv.Itoa(int(port))
}

// guestSpecToProto surfaces the structured boot source + config so the UI can
// clone a guest (pre-fill the Create-VM wizard).
func guestSpecToProto(g *swiftv1alpha1.SwiftGuest) *kubeswiftv1.GuestSpec {
	s := &kubeswiftv1.GuestSpec{
		GuestClassRef: g.Spec.GuestClassRef.Name,
		KernelCmdline: g.Spec.KernelCmdline,
		RunPolicy:     string(g.Spec.RunPolicy),
		OsType:        string(g.Spec.OSType),
	}
	if g.Spec.ImageRef != nil {
		s.ImageRef = g.Spec.ImageRef.Name
	}
	if g.Spec.KernelRef != nil {
		s.KernelRef = g.Spec.KernelRef.Name
	}
	if g.Spec.SeedProfileRef != nil {
		s.SeedProfileRef = g.Spec.SeedProfileRef.Name
	}
	if g.Spec.GPUProfileRef != nil {
		s.GpuProfileRef = g.Spec.GPUProfileRef.Name
	}
	if g.Spec.CloneFromSnapshot != nil {
		s.CloneSnapshotRef = g.Spec.CloneFromSnapshot.SnapshotRef.Name
	}
	return s
}

// DeleteGuest deletes one SwiftGuest as the impersonated user. RBAC gates it; a
// denial surfaces (never a silent no-op).
func (s *GuestService) DeleteGuest(ctx context.Context, req *connect.Request[kubeswiftv1.DeleteGuestRequest]) (*connect.Response[kubeswiftv1.DeleteGuestResponse], error) {
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
	if err := guestResource(dyn, ref.GetNamespace()).Delete(ctx, ref.GetName(), metav1.DeleteOptions{}); err != nil {
		code := connect.CodeInternal
		switch {
		case apierrors.IsNotFound(err):
			code = connect.CodeNotFound
		case apierrors.IsForbidden(err) || apierrors.IsInvalid(err):
			code = connect.CodeFailedPrecondition
		}
		return nil, connect.NewError(code, err)
	}
	return connect.NewResponse(&kubeswiftv1.DeleteGuestResponse{}), nil
}

// StartGuest sets the guest's runPolicy to Running; StopGuest sets it to
// Stopped and deletes the launcher pod. Both run as the impersonated user
// against the member cluster (decision D1) and delegate to internal/actions,
// the single implementation shared with swiftctl — so a stop that forgets the
// pod delete (PR #267) cannot diverge between the two surfaces again. The live
// Watch reflects the resulting phase.
func (s *GuestService) StartGuest(ctx context.Context, req *connect.Request[kubeswiftv1.GuestActionRequest]) (*connect.Response[kubeswiftv1.GuestActionResponse], error) {
	return s.guestAction(ctx, req, actions.Start)
}

func (s *GuestService) StopGuest(ctx context.Context, req *connect.Request[kubeswiftv1.GuestActionRequest]) (*connect.Response[kubeswiftv1.GuestActionResponse], error) {
	return s.guestAction(ctx, req, actions.Stop)
}

// guestAction authenticates, validates the ref, resolves the impersonating
// dynamic client, runs the shared action, and maps the patched guest to proto.
// action is actions.Start or actions.Stop (same signature).
func (s *GuestService) guestAction(ctx context.Context, req *connect.Request[kubeswiftv1.GuestActionRequest],
	action func(context.Context, dynamic.Interface, string, string) (*unstructured.Unstructured, error),
) (*connect.Response[kubeswiftv1.GuestActionResponse], error) {
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
	u, err := action(ctx, dyn, ref.GetNamespace(), ref.GetName())
	if err != nil {
		// Start/Stop patch runPolicy (no policy webhook denies it), so a 403 here
		// is an RBAC denial -> PermissionDenied, and NotFound -> NotFound.
		return nil, mapAccessErr(err)
	}
	g, err := toSwiftGuest(u)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kubeswiftv1.GuestActionResponse{Guest: guestToProto(ref.GetCluster(), g)}), nil
}

// MigrateGuest creates a SwiftMigration to move the guest to targetNode, as the
// impersonated user. The migration then progresses on the member; the UI
// watches the guest's phase (a dedicated migrations view is a later add).
func (s *GuestService) MigrateGuest(ctx context.Context, req *connect.Request[kubeswiftv1.MigrateGuestRequest]) (*connect.Response[kubeswiftv1.MigrateGuestResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ref := req.Msg.GetRef()
	if ref == nil || ref.GetCluster() == "" || ref.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ref.cluster and ref.name are required"))
	}
	if req.Msg.GetTargetNode() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("target_node is required"))
	}
	dyn, err := s.pool.DynamicFor(ref.GetCluster(), id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	created, err := actions.Migrate(ctx, dyn, actions.MigrateParams{
		Namespace:     ref.GetNamespace(),
		GuestName:     ref.GetName(),
		TargetNode:    req.Msg.GetTargetNode(),
		Mode:          req.Msg.GetMode(), // empty resolves to auto in actions.Migrate
		AllowIPChange: req.Msg.GetAllowIpChange(),
		Reason:        "initiated from the KubeSwift UI",
		GenerateName:  ref.GetName() + "-mig-",
	})
	if err != nil {
		// A webhook admission denial (e.g. allowIPChange required for a
		// cross-node move, or live+VFIO) surfaces here with its reason — never
		// a silent failure.
		code := connect.CodeInternal
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			code = connect.CodeFailedPrecondition
		}
		return nil, connect.NewError(code, err)
	}
	return connect.NewResponse(&kubeswiftv1.MigrateGuestResponse{
		Migration: &kubeswiftv1.ObjectRef{
			Cluster:   ref.GetCluster(),
			Namespace: ref.GetNamespace(),
			Name:      created.GetName(),
		},
	}), nil
}

// CreateGuest builds a SwiftGuest from the wizard's structured input and
// server-side-applies it as the impersonated user. The admission webhook is the
// authority on spec validity (boot-source exclusivity, gpu+kernel, osType, …); a
// denial surfaces as FailedPrecondition, never a silent create.
func (s *GuestService) CreateGuest(ctx context.Context, req *connect.Request[kubeswiftv1.CreateGuestRequest]) (*connect.Response[kubeswiftv1.CreateGuestResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetCluster() == "" || m.GetNamespace() == "" || m.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster, namespace, and name are required"))
	}
	if m.GetGuestClassRef() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("guest_class_ref is required"))
	}
	// Exactly one boot source — a clear gateway error ahead of the webhook.
	boot := 0
	for _, v := range []string{m.GetImageRef(), m.GetKernelRef(), m.GetCloneSnapshotRef()} {
		if v != "" {
			boot++
		}
	}
	if boot != 1 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("exactly one boot source is required: image_ref, kernel_ref, or clone_snapshot_ref"))
	}

	dyn, err := s.pool.DynamicFor(m.GetCluster(), id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	obj := buildSwiftGuest(m)
	// Create (not apply) — a "create a new VM" must fail loudly on a name clash
	// rather than silently overwrite an existing guest's spec.
	if _, err := guestResource(dyn, m.GetNamespace()).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		// A name clash, a webhook denial (boot-source / gpu / osType rules), or an
		// RBAC refusal surfaces here with its reason — never a silent create.
		code := connect.CodeInternal
		switch {
		case apierrors.IsAlreadyExists(err):
			code = connect.CodeAlreadyExists
		case apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
			code = connect.CodeFailedPrecondition
		}
		return nil, connect.NewError(code, err)
	}
	return connect.NewResponse(&kubeswiftv1.CreateGuestResponse{
		Ref: &kubeswiftv1.ObjectRef{Cluster: m.GetCluster(), Namespace: m.GetNamespace(), Name: m.GetName()},
	}), nil
}

// buildSwiftGuest constructs the SwiftGuest unstructured object from the wizard
// request — only the set fields are written; CRD defaults + the webhook handle
// the rest.
func buildSwiftGuest(m *kubeswiftv1.CreateGuestRequest) *unstructured.Unstructured {
	ref := func(name string) map[string]interface{} { return map[string]interface{}{"name": name} }
	spec := map[string]interface{}{"guestClassRef": ref(m.GetGuestClassRef())}

	switch {
	case m.GetImageRef() != "":
		spec["imageRef"] = ref(m.GetImageRef())
	case m.GetKernelRef() != "":
		spec["kernelRef"] = ref(m.GetKernelRef())
		if c := m.GetKernelCmdline(); c != "" {
			spec["kernelCmdline"] = c
		}
	case m.GetCloneSnapshotRef() != "":
		clone := map[string]interface{}{"snapshotRef": ref(m.GetCloneSnapshotRef())}
		if t := m.GetCloneTargetNode(); t != "" {
			clone["targetNode"] = t
		}
		spec["cloneFromSnapshot"] = clone
	}

	if v := m.GetSeedProfileRef(); v != "" {
		spec["seedProfileRef"] = ref(v)
	}
	if v := m.GetGpuProfileRef(); v != "" {
		spec["gpuProfileRef"] = ref(v)
	}
	if v := m.GetRunPolicy(); v != "" {
		spec["runPolicy"] = v
	}
	if v := m.GetOsType(); v != "" {
		spec["osType"] = v
	}
	if v := m.GetNodeName(); v != "" {
		spec["nodeName"] = v
	}
	if ports := m.GetPorts(); len(ports) > 0 {
		out := make([]interface{}, 0, len(ports))
		for _, p := range ports {
			port := map[string]interface{}{"port": int64(p.GetPort())}
			if p.GetName() != "" {
				port["name"] = p.GetName()
			}
			if p.GetTargetPort() != 0 {
				port["targetPort"] = int64(p.GetTargetPort())
			}
			if p.GetProtocol() != "" {
				port["protocol"] = p.GetProtocol()
			}
			if p.GetExpose() != "" {
				port["expose"] = p.GetExpose()
			}
			out = append(out, port)
		}
		spec["network"] = map[string]interface{}{"ports": out}
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "swift.kubeswift.io/v1alpha1",
		"kind":       "SwiftGuest",
		"metadata":   map[string]interface{}{"name": m.GetName(), "namespace": m.GetNamespace()},
		"spec":       spec,
	}}
}

var eventsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"}

// GetGuestEvents returns the Kubernetes Events involving the guest and its
// launcher pod (the "why won't my VM boot" surface), newest first, as the
// impersonated user. The launcher pod is named after the guest (so one query by
// name catches both the SwiftGuest and Pod events); a migrated guest's pod is
// renamed, so its podRef is queried too.
func (s *GuestService) GetGuestEvents(ctx context.Context, req *connect.Request[kubeswiftv1.GetGuestEventsRequest]) (*connect.Response[kubeswiftv1.GetGuestEventsResponse], error) {
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
	ns := ref.GetNamespace()

	// Resolve the launcher pod name (renamed after a live migration).
	queryNames := []string{ref.GetName()}
	if gu, err := dyn.Resource(swiftGuestGVR).Namespace(ns).Get(ctx, ref.GetName(), metav1.GetOptions{}); err == nil {
		if p, ok, _ := unstructured.NestedString(gu.Object, "status", "podRef", "name"); ok && p != "" && p != ref.GetName() {
			queryNames = append(queryNames, p)
		}
	}

	seen := map[string]bool{}
	out := &kubeswiftv1.GetGuestEventsResponse{}
	for i, qn := range queryNames {
		list, err := dyn.Resource(eventsGVR).Namespace(ns).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.name=" + qn,
		})
		if err != nil {
			if i == 0 {
				return nil, mapAccessErr(err) // surface RBAC/connectivity, never a silent empty
			}
			continue // best-effort on the secondary (pod) query
		}
		for j := range list.Items {
			e := &list.Items[j]
			if uid, _, _ := unstructured.NestedString(e.Object, "metadata", "uid"); uid != "" {
				if seen[uid] {
					continue
				}
				seen[uid] = true
			}
			out.Events = append(out.Events, mapEvent(e))
		}
	}
	sort.Slice(out.Events, func(i, j int) bool {
		return out.Events[i].GetLastSeen().AsTime().After(out.Events[j].GetLastSeen().AsTime())
	})
	return connect.NewResponse(out), nil
}

func mapEvent(e *unstructured.Unstructured) *kubeswiftv1.GuestEventEntry {
	str := func(f ...string) string { v, _, _ := unstructured.NestedString(e.Object, f...); return v }
	count, _, _ := unstructured.NestedInt64(e.Object, "count")
	entry := &kubeswiftv1.GuestEventEntry{
		Type:    str("type"),
		Reason:  str("reason"),
		Message: str("message"),
		Count:   int32(count),
		Object:  str("involvedObject", "kind") + "/" + str("involvedObject", "name"),
	}
	for _, f := range []string{"lastTimestamp", "eventTime"} {
		if ts := str(f); ts != "" {
			if t, perr := time.Parse(time.RFC3339, ts); perr == nil {
				entry.LastSeen = timestamppb.New(t)
				break
			}
		}
	}
	if entry.LastSeen == nil {
		if ts := str("metadata", "creationTimestamp"); ts != "" {
			if t, perr := time.Parse(time.RFC3339, ts); perr == nil {
				entry.LastSeen = timestamppb.New(t)
			}
		}
	}
	return entry
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
