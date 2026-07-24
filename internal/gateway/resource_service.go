package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	connect "connectrpc.com/connect"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
	"github.com/kubeswift-io/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ResourceService is the read-only cluster explorer (decisions E1–E5): a single
// generic list-by-kind plane over the impersonating dynamic client, driven by a
// server-owned catalog. It is per-cluster (the UI browses one member at a time)
// and read-only by design — VM lifecycle stays the typed Start/Stop/Migrate
// path. Every call impersonates the end user (D1).
type ResourceService struct {
	kubeswiftv1connect.UnimplementedResourceServiceHandler

	pool clientProvider
	auth Authenticator
}

var _ kubeswiftv1connect.ResourceServiceHandler = (*ResourceService)(nil)

// NewResourceService wires the explorer to the client pool + the authenticator.
func NewResourceService(pool clientProvider, auth Authenticator) *ResourceService {
	return &ResourceService{pool: pool, auth: auth}
}

// ListResourceKinds returns the browsable catalog the UI renders its nav from.
func (s *ResourceService) ListResourceKinds(ctx context.Context, req *connect.Request[kubeswiftv1.ListResourceKindsRequest]) (*connect.Response[kubeswiftv1.ListResourceKindsResponse], error) {
	if _, err := s.auth.Authenticate(ctx, req.Header()); err != nil {
		return nil, err
	}
	out := &kubeswiftv1.ListResourceKindsResponse{}
	for i := range resourceCatalog {
		k := &resourceCatalog[i]
		out.Kinds = append(out.Kinds, &kubeswiftv1.ResourceKind{
			Key:         k.key,
			DisplayName: k.displayName,
			Group:       k.gvr.Group,
			Version:     k.gvr.Version,
			Resource:    k.gvr.Resource,
			Namespaced:  k.namespaced,
			Category:    k.category,
			Columns:     k.columns,
		})
	}
	return connect.NewResponse(out), nil
}

// ListResources lists one catalog kind on one member as the impersonated user.
// A kind absent on the member (CRD not installed), an RBAC denial, or an
// unreachable cluster all surface as a ClusterError, never a silent empty list.
func (s *ResourceService) ListResources(ctx context.Context, req *connect.Request[kubeswiftv1.ListResourcesRequest]) (*connect.Response[kubeswiftv1.ListResourcesResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster := req.Msg.GetCluster()
	if cluster == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster is required"))
	}
	kind := lookupKind(req.Msg.GetKind())
	if kind == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown kind %q", req.Msg.GetKind()))
	}
	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		return resourcesErr(cluster, err.Error()), nil
	}

	var ri dynamic.ResourceInterface = dyn.Resource(kind.gvr)
	if kind.namespaced && req.Msg.GetNamespace() != "" {
		ri = dyn.Resource(kind.gvr).Namespace(req.Msg.GetNamespace())
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return resourcesErr(cluster, err.Error()), nil
	}

	out := &kubeswiftv1.ListResourcesResponse{}
	for i := range list.Items {
		item := &list.Items[i]
		r := &kubeswiftv1.Resource{
			Ref:     &kubeswiftv1.ObjectRef{Cluster: cluster, Namespace: item.GetNamespace(), Name: item.GetName()},
			Kind:    kind.key,
			Columns: kind.project(item),
		}
		if ts := item.GetCreationTimestamp(); !ts.IsZero() {
			r.CreatedAt = timestamppb.New(ts.Time)
		}
		out.Resources = append(out.Resources, r)
	}
	sortResources(out.Resources)
	return connect.NewResponse(out), nil
}

// GetResource returns one object's full content (YAML for the editor + JSON for
// the UI to read fields from), as the impersonated user. managedFields are
// stripped. Feeds the detail drawer and the YAML editor.
func (s *ResourceService) GetResource(ctx context.Context, req *connect.Request[kubeswiftv1.GetResourceRequest]) (*connect.Response[kubeswiftv1.GetResourceResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster, name := req.Msg.GetCluster(), req.Msg.GetName()
	if cluster == "" || name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster and name are required"))
	}
	kind := lookupKind(req.Msg.GetKind())
	if kind == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown kind %q", req.Msg.GetKind()))
	}
	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	var ri dynamic.ResourceInterface = dyn.Resource(kind.gvr)
	if kind.namespaced {
		ri = dyn.Resource(kind.gvr).Namespace(req.Msg.GetNamespace())
	}
	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, mapAccessErr(err)
	}
	unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
	redactSecret(obj, kind) // E4: never expose Secret values, even via Get
	jsonBytes, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	yamlBytes, err := yaml.Marshal(obj.Object)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kubeswiftv1.GetResourceResponse{Yaml: string(yamlBytes), Json: string(jsonBytes)}), nil
}

// ApplyResource creates-or-updates one object from edited YAML via server-side
// apply, as the impersonated user — RBAC is the only gate (a forbidden surfaces
// as a permission error). The kind's GVR comes from the catalog; the object's
// identity comes from the YAML. Server-managed metadata (resourceVersion, uid,
// managedFields, creationTimestamp, generation) and status are stripped so the
// apply doesn't conflict on read-only fields. SSA is field-ownership-based, so a
// Secret edited without its (redacted) data leaves the values untouched.
func (s *ResourceService) ApplyResource(ctx context.Context, req *connect.Request[kubeswiftv1.ApplyResourceRequest]) (*connect.Response[kubeswiftv1.ApplyResourceResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster := req.Msg.GetCluster()
	if cluster == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster is required"))
	}
	kind := lookupKind(req.Msg.GetKind())
	if kind == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown kind %q", req.Msg.GetKind()))
	}
	if strings.TrimSpace(req.Msg.GetYaml()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("yaml is required"))
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(req.Msg.GetYaml()), &obj.Object); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid YAML: %w", err))
	}
	name := obj.GetName()
	if name == "" || obj.GetAPIVersion() == "" || obj.GetKind() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the YAML must set apiVersion, kind, and metadata.name"))
	}
	ns := obj.GetNamespace()
	if ns == "" {
		ns = req.Msg.GetNamespace()
	}
	if kind.namespaced && ns == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("namespace is required for %s", kind.key))
	}
	for _, f := range [][]string{
		{"metadata", "managedFields"}, {"metadata", "resourceVersion"}, {"metadata", "uid"},
		{"metadata", "creationTimestamp"}, {"metadata", "generation"}, {"status"},
	} {
		unstructured.RemoveNestedField(obj.Object, f...)
	}

	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	var ri dynamic.ResourceInterface = dyn.Resource(kind.gvr)
	if kind.namespaced {
		obj.SetNamespace(ns)
		ri = dyn.Resource(kind.gvr).Namespace(ns)
	}
	applied, err := ri.Apply(ctx, name, obj, metav1.ApplyOptions{FieldManager: "kubeswift-ui", Force: true})
	if err != nil {
		return nil, mapAccessErr(err)
	}
	unstructured.RemoveNestedField(applied.Object, "metadata", "managedFields")
	redactSecret(applied, kind)
	jsonBytes, err := json.Marshal(applied.Object)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	yamlBytes, err := yaml.Marshal(applied.Object)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kubeswiftv1.ApplyResourceResponse{Yaml: string(yamlBytes), Json: string(jsonBytes)}), nil
}

// DeleteResource deletes one object as the impersonated user. RBAC gates it; a
// denial surfaces as a permission error (never a silent no-op).
func (s *ResourceService) DeleteResource(ctx context.Context, req *connect.Request[kubeswiftv1.DeleteResourceRequest]) (*connect.Response[kubeswiftv1.DeleteResourceResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster, name := req.Msg.GetCluster(), req.Msg.GetName()
	if cluster == "" || name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster and name are required"))
	}
	kind := lookupKind(req.Msg.GetKind())
	if kind == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown kind %q", req.Msg.GetKind()))
	}
	if kind.namespaced && req.Msg.GetNamespace() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("namespace is required for %s", kind.key))
	}
	dyn, err := s.pool.DynamicFor(cluster, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	var ri dynamic.ResourceInterface = dyn.Resource(kind.gvr)
	if kind.namespaced {
		ri = dyn.Resource(kind.gvr).Namespace(req.Msg.GetNamespace())
	}
	if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return nil, mapAccessErr(err)
	}
	return connect.NewResponse(&kubeswiftv1.DeleteResourceResponse{}), nil
}

// redactSecret strips Secret values so the gateway never exposes them (E4),
// even via GetResource / ApplyResource which return the full object.
func redactSecret(obj *unstructured.Unstructured, kind *resourceKind) {
	if kind.gvr.Group == "" && kind.gvr.Resource == "secrets" {
		unstructured.RemoveNestedField(obj.Object, "data")
		unstructured.RemoveNestedField(obj.Object, "stringData")
	}
}

func resourcesErr(cluster, msg string) *connect.Response[kubeswiftv1.ListResourcesResponse] {
	return connect.NewResponse(&kubeswiftv1.ListResourcesResponse{
		Error: &kubeswiftv1.ClusterError{Cluster: cluster, Message: msg},
	})
}

func sortResources(rs []*kubeswiftv1.Resource) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i].GetRef(), rs[j].GetRef()
		if a.GetNamespace() != b.GetNamespace() {
			return a.GetNamespace() < b.GetNamespace()
		}
		return a.GetName() < b.GetName()
	})
}

// restConfigProvider is the subset of the pool CanI needs: an impersonated
// rest.Config to build a typed client for SelfSubjectAccessReview. The concrete
// *ClientPool satisfies it; kept narrow so the shared clientProvider interface
// (and its test fakes) are untouched.
type restConfigProvider interface {
	RestConfigFor(cluster string, id Identity) (*rest.Config, error)
}

// CanI answers, per (kind, verb, namespace) check, whether the impersonated user
// may perform it — via a SelfSubjectAccessReview run through the user's own
// (impersonated) client, so the apiserver is the sole decider. This lets the UI
// hide actions the user can't take instead of firing them and surfacing a
// denial. Decisions are positionally aligned with the request; an unknown kind
// or a review error yields allowed=false (fail-closed).
func (s *ResourceService) CanI(ctx context.Context, req *connect.Request[kubeswiftv1.CanIRequest]) (*connect.Response[kubeswiftv1.CanIResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster := req.Msg.GetCluster()
	if cluster == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster is required"))
	}
	rcp, ok := s.pool.(restConfigProvider)
	if !ok {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("access review not supported"))
	}
	cfg, err := rcp.RestConfigFor(cluster, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := &kubeswiftv1.CanIResponse{}
	for _, chk := range req.Msg.GetChecks() {
		out.Decisions = append(out.Decisions, reviewAccess(ctx, cs, chk))
	}
	return connect.NewResponse(out), nil
}

// reviewAccess maps one catalog-kind check to a SelfSubjectAccessReview and
// returns the apiserver's verdict. namespace is ignored for cluster-scoped kinds.
func reviewAccess(ctx context.Context, cs kubernetes.Interface, chk *kubeswiftv1.ResourceAccessCheck) *kubeswiftv1.AccessDecision {
	kind := lookupKind(chk.GetKind())
	if kind == nil {
		return &kubeswiftv1.AccessDecision{Allowed: false, Reason: fmt.Sprintf("unknown kind %q", chk.GetKind())}
	}
	ns := chk.GetNamespace()
	if !kind.namespaced {
		ns = ""
	}
	ssar := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: ns,
				Verb:      chk.GetVerb(),
				Group:     kind.gvr.Group,
				Resource:  kind.gvr.Resource,
			},
		},
	}
	res, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, ssar, metav1.CreateOptions{})
	if err != nil {
		return &kubeswiftv1.AccessDecision{Allowed: false, Reason: err.Error()}
	}
	return &kubeswiftv1.AccessDecision{Allowed: res.Status.Allowed, Reason: res.Status.Reason}
}
