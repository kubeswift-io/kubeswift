package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	connect "connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	kubeswiftv1 "github.com/projectbeskar/kubeswift/gen/kubeswift/v1"
	"github.com/projectbeskar/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
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
