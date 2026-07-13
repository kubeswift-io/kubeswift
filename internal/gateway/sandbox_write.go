package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	connect "connectrpc.com/connect"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
)

// CreateSandbox builds a SwiftSandbox from the form input and creates it as the
// impersonated user (D1). The admission webhook is the authority on spec
// validity; a denial (or a name clash, or an RBAC refusal) surfaces with its
// reason — never a silent create.
func (s *SandboxService) CreateSandbox(ctx context.Context, req *connect.Request[kubeswiftv1.CreateSandboxRequest]) (*connect.Response[kubeswiftv1.CreateSandboxResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetCluster() == "" || m.GetNamespace() == "" || m.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster, namespace, and name are required"))
	}
	if m.GetImage() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("image is required"))
	}
	dyn, err := s.pool.DynamicFor(m.GetCluster(), id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if _, err := sandboxResource(dyn, m.GetNamespace()).Create(ctx, buildSwiftSandbox(m), metav1.CreateOptions{}); err != nil {
		return nil, mapCreateErr(err)
	}
	return connect.NewResponse(&kubeswiftv1.CreateSandboxResponse{
		Ref: &kubeswiftv1.ObjectRef{Cluster: m.GetCluster(), Namespace: m.GetNamespace(), Name: m.GetName()},
	}), nil
}

// DeleteSandbox deletes one SwiftSandbox as the impersonated user.
func (s *SandboxService) DeleteSandbox(ctx context.Context, req *connect.Request[kubeswiftv1.DeleteSandboxRequest]) (*connect.Response[kubeswiftv1.DeleteSandboxResponse], error) {
	dyn, ref, err := s.authRefDyn(ctx, req.Header(), req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	if err := sandboxResource(dyn, ref.GetNamespace()).Delete(ctx, ref.GetName(), metav1.DeleteOptions{}); err != nil {
		return nil, mapDeleteErr(err)
	}
	return connect.NewResponse(&kubeswiftv1.DeleteSandboxResponse{}), nil
}

// CreateSandboxPool builds a SwiftSandboxPool from the form input and creates it
// as the impersonated user. Same authority model as CreateSandbox.
func (s *SandboxService) CreateSandboxPool(ctx context.Context, req *connect.Request[kubeswiftv1.CreateSandboxPoolRequest]) (*connect.Response[kubeswiftv1.CreateSandboxPoolResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetCluster() == "" || m.GetNamespace() == "" || m.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster, namespace, and name are required"))
	}
	if m.GetImage() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("image is required"))
	}
	dyn, err := s.pool.DynamicFor(m.GetCluster(), id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if _, err := sandboxPoolResource(dyn, m.GetNamespace()).Create(ctx, buildSwiftSandboxPool(m), metav1.CreateOptions{}); err != nil {
		return nil, mapCreateErr(err)
	}
	return connect.NewResponse(&kubeswiftv1.CreateSandboxPoolResponse{
		Ref: &kubeswiftv1.ObjectRef{Cluster: m.GetCluster(), Namespace: m.GetNamespace(), Name: m.GetName()},
	}), nil
}

// DeleteSandboxPool deletes one SwiftSandboxPool as the impersonated user.
func (s *SandboxService) DeleteSandboxPool(ctx context.Context, req *connect.Request[kubeswiftv1.DeleteSandboxPoolRequest]) (*connect.Response[kubeswiftv1.DeleteSandboxPoolResponse], error) {
	dyn, ref, err := s.authRefDyn(ctx, req.Header(), req.Msg.GetRef())
	if err != nil {
		return nil, err
	}
	if err := sandboxPoolResource(dyn, ref.GetNamespace()).Delete(ctx, ref.GetName(), metav1.DeleteOptions{}); err != nil {
		return nil, mapDeleteErr(err)
	}
	return connect.NewResponse(&kubeswiftv1.DeleteSandboxPoolResponse{}), nil
}

// authRefDyn is the shared prelude for the ref-addressed write RPCs: authenticate,
// validate the ref, resolve the impersonating dynamic client.
func (s *SandboxService) authRefDyn(ctx context.Context, hdr http.Header, ref *kubeswiftv1.ObjectRef) (dynamic.Interface, *kubeswiftv1.ObjectRef, error) {
	id, err := s.auth.Authenticate(ctx, hdr)
	if err != nil {
		return nil, nil, err
	}
	if ref == nil || ref.GetCluster() == "" || ref.GetName() == "" {
		return nil, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ref.cluster and ref.name are required"))
	}
	dyn, err := s.pool.DynamicFor(ref.GetCluster(), id)
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeNotFound, err)
	}
	return dyn, ref, nil
}

// mapCreateErr maps a create failure to a connect code — a name clash, a webhook
// denial, or an RBAC refusal surfaces with its reason (no silent create).
func mapCreateErr(err error) *connect.Error {
	switch {
	case apierrors.IsAlreadyExists(err):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// mapDeleteErr maps a delete failure to a connect code (NotFound / RBAC).
func mapDeleteErr(err error) *connect.Error {
	switch {
	case apierrors.IsNotFound(err):
		return connect.NewError(connect.CodeNotFound, err)
	case apierrors.IsForbidden(err) || apierrors.IsInvalid(err):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// buildSwiftSandbox constructs the SwiftSandbox unstructured from the form
// request — only set fields are written; CRD defaults + the webhook do the rest.
func buildSwiftSandbox(m *kubeswiftv1.CreateSandboxRequest) *unstructured.Unstructured {
	spec := map[string]interface{}{"image": m.GetImage()}
	if v := m.GetCommand(); len(v) > 0 {
		spec["command"] = toIfaceSlice(v)
	}
	if v := m.GetArgs(); len(v) > 0 {
		spec["args"] = toIfaceSlice(v)
	}
	if v := m.GetEnv(); len(v) > 0 {
		spec["env"] = envToIface(v)
	}
	if v := m.GetWorkingDir(); v != "" {
		spec["workingDir"] = v
	}
	if v := m.GetNetworkMode(); v != "" {
		spec["network"] = map[string]interface{}{"mode": v}
	}
	if v := m.GetCpu(); v > 0 {
		spec["cpu"] = int64(v)
	}
	if v := m.GetMemoryMib(); v > 0 {
		spec["memory"] = fmt.Sprintf("%dMi", v)
	}
	if v := m.GetPoolRef(); v != "" {
		spec["poolRef"] = map[string]interface{}{"name": v}
	}
	if v := m.GetKernelProfileRef(); v != "" {
		spec["kernelProfileRef"] = map[string]interface{}{"name": v}
	}
	if v := m.GetTimeout(); v != "" {
		spec["timeout"] = v
	}
	if v := m.GetTtl(); v != "" {
		spec["ttl"] = v
	}
	if v := m.GetImagePullSecret(); v != "" {
		spec["imagePullSecret"] = v
	}
	return sandboxUnstructured("SwiftSandbox", m.GetName(), m.GetNamespace(), spec)
}

// buildSwiftSandboxPool constructs the SwiftSandboxPool unstructured from the
// form request.
func buildSwiftSandboxPool(m *kubeswiftv1.CreateSandboxPoolRequest) *unstructured.Unstructured {
	spec := map[string]interface{}{"image": m.GetImage()}
	if v := m.GetMinWarm(); v > 0 {
		spec["minWarm"] = int64(v)
	}
	if v := m.GetMaxWarm(); v > 0 {
		spec["maxWarm"] = int64(v)
	}
	if v := m.GetCpu(); v > 0 {
		spec["cpu"] = int64(v)
	}
	if v := m.GetMemoryMib(); v > 0 {
		spec["memory"] = fmt.Sprintf("%dMi", v)
	}
	if v := m.GetNetworkMode(); v != "" {
		spec["network"] = map[string]interface{}{"mode": v}
	}
	if v := m.GetKernelProfileRef(); v != "" {
		spec["kernelProfileRef"] = map[string]interface{}{"name": v}
	}
	if v := m.GetImagePullSecret(); v != "" {
		spec["imagePullSecret"] = v
	}
	return sandboxUnstructured("SwiftSandboxPool", m.GetName(), m.GetNamespace(), spec)
}

func sandboxUnstructured(kind, name, namespace string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "sandbox.kubeswift.io/v1alpha1",
		"kind":       kind,
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		"spec":       spec,
	}}
}

func toIfaceSlice(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// envToIface renders the form's env map as the SwiftSandbox spec.env list of
// {name, value} (the CRD's corev1.EnvVar shape).
func envToIface(env map[string]string) []interface{} {
	out := make([]interface{}, 0, len(env))
	for k, v := range env {
		out = append(out, map[string]interface{}{"name": k, "value": v})
	}
	return out
}
