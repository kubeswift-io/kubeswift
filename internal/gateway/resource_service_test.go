package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"

	connect "connectrpc.com/connect"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
)

func fakeExplorerDyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			gvr("", "v1", "nodes"):   "NodeList",
			gvr("", "v1", "secrets"): "SecretList",
			gvr("", "v1", "pods"):    "PodList",
		}, objs...)
}

func mkNode(name string, ready bool) *unstructured.Unstructured {
	st := "False"
	if ready {
		st = "True"
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]interface{}{
			"name":   name,
			"labels": map[string]interface{}{"node-role.kubernetes.io/control-plane": ""},
		},
		"spec": map[string]interface{}{},
		"status": map[string]interface{}{
			"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": st}},
			"nodeInfo":   map[string]interface{}{"kubeletVersion": "v1.34.3"},
			"addresses":  []interface{}{map[string]interface{}{"type": "InternalIP", "address": "10.0.0.1"}},
		},
	}}
}

// uSecret carries real (base64) values so the redaction test can assert none
// of them — base64 or decoded — reach the wire.
func uSecret(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]interface{}{"namespace": ns, "name": name},
		"type":     "Opaque",
		"data": map[string]interface{}{
			"password": "c3VwZXItc2VjcmV0", // base64("super-secret")
			"apitoken": "dG9wLXNlY3JldA==", // base64("top-secret")
		},
	}}
}

func TestResourceService_ListResourceKinds(t *testing.T) {
	svc := NewResourceService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": fakeExplorerDyn()}}, NewInsecureAuthenticator())
	resp, err := svc.ListResourceKinds(context.Background(), connect.NewRequest(&kubeswiftv1.ListResourceKindsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Kinds) != len(resourceCatalog) {
		t.Fatalf("got %d kinds, want %d", len(resp.Msg.Kinds), len(resourceCatalog))
	}
	byKey := map[string]*kubeswiftv1.ResourceKind{}
	for _, k := range resp.Msg.Kinds {
		byKey[k.Key] = k
	}
	if k := byKey["nodes"]; k == nil || k.Namespaced || k.Category != "Cluster" {
		t.Errorf("nodes should be a cluster-scoped Cluster kind, got %+v", k)
	}
	if k := byKey["secrets"]; k == nil || k.Category != "Config" {
		t.Errorf("secrets should be a Config kind, got %+v", k)
	}
	// SwiftGuests/SwiftMigrations have dedicated views — they must NOT appear.
	if byKey["swiftguests"] != nil || byKey["swiftmigrations"] != nil {
		t.Errorf("swiftguests/swiftmigrations must not be in the explorer catalog")
	}
	// Sandboxes + warm pools are browsed in the Explorer's KubeSwift menu (not a
	// dedicated top-bar view).
	if k := byKey["swiftsandboxes"]; k == nil || !k.Namespaced || k.Category != "KubeSwift" {
		t.Errorf("swiftsandboxes should be a namespaced KubeSwift kind, got %+v", k)
	}
	if k := byKey["swiftsandboxpools"]; k == nil || !k.Namespaced || k.Category != "KubeSwift" {
		t.Errorf("swiftsandboxpools should be a namespaced KubeSwift kind, got %+v", k)
	}
}

// TestResourceService_SecretRedaction is the load-bearing E4 test: Secret values
// (base64 or decoded) must never reach the response — only key names + type.
func TestResourceService_SecretRedaction(t *testing.T) {
	prov := &fakeProvider{clients: map[string]dynamic.Interface{"boba": fakeExplorerDyn(uSecret("default", "db-creds"))}}
	svc := NewResourceService(prov, NewInsecureAuthenticator())

	resp, err := svc.ListResources(context.Background(), connect.NewRequest(&kubeswiftv1.ListResourcesRequest{Cluster: "boba", Kind: "secrets"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Msg.Error)
	}
	if len(resp.Msg.Resources) != 1 {
		t.Fatalf("want 1 secret, got %d", len(resp.Msg.Resources))
	}
	r := resp.Msg.Resources[0]
	if r.Columns["type"] != "Opaque" {
		t.Errorf("type = %q, want Opaque", r.Columns["type"])
	}
	if r.Columns["keys"] != "apitoken,password" {
		t.Errorf("keys = %q, want apitoken,password (sorted names only)", r.Columns["keys"])
	}
	blob := fmt.Sprintf("%+v", resp.Msg)
	for _, leak := range []string{"c3VwZXItc2VjcmV0", "super-secret", "dG9wLXNlY3JldA==", "top-secret"} {
		if strings.Contains(blob, leak) {
			t.Fatalf("secret value leaked into the explorer response: %q", leak)
		}
	}
}

func TestResourceService_NodeProjection(t *testing.T) {
	prov := &fakeProvider{clients: map[string]dynamic.Interface{"boba": fakeExplorerDyn(mkNode("miles", true))}}
	svc := NewResourceService(prov, NewInsecureAuthenticator())

	resp, err := svc.ListResources(context.Background(), connect.NewRequest(&kubeswiftv1.ListResourcesRequest{Cluster: "boba", Kind: "nodes"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Resources) != 1 {
		t.Fatalf("want 1 node, got %d", len(resp.Msg.Resources))
	}
	r := resp.Msg.Resources[0]
	if r.Ref.Cluster != "boba" || r.Ref.Name != "miles" {
		t.Errorf("ref = %+v", r.Ref)
	}
	for k, want := range map[string]string{"status": "Ready", "roles": "control-plane", "version": "v1.34.3", "internalIP": "10.0.0.1"} {
		if r.Columns[k] != want {
			t.Errorf("column %q = %q, want %q", k, r.Columns[k], want)
		}
	}
}

func TestResourceService_UnknownKindAndCluster(t *testing.T) {
	svc := NewResourceService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": fakeExplorerDyn()}}, NewInsecureAuthenticator())

	// Unknown kind -> hard InvalidArgument (a client bug, not a cluster problem).
	_, err := svc.ListResources(context.Background(), connect.NewRequest(&kubeswiftv1.ListResourcesRequest{Cluster: "boba", Kind: "frobnicators"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown kind: want InvalidArgument, got %v", err)
	}
	// Missing cluster -> hard InvalidArgument.
	_, err = svc.ListResources(context.Background(), connect.NewRequest(&kubeswiftv1.ListResourcesRequest{Kind: "nodes"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing cluster: want InvalidArgument, got %v", err)
	}
	// Unknown/unreachable cluster -> soft ClusterError, never a silent empty list.
	resp, err := svc.ListResources(context.Background(), connect.NewRequest(&kubeswiftv1.ListResourcesRequest{Cluster: "ghost", Kind: "nodes"}))
	if err != nil {
		t.Fatalf("unreachable cluster should be a soft ClusterError, got hard err %v", err)
	}
	if resp.Msg.Error == nil || resp.Msg.Error.Cluster != "ghost" {
		t.Errorf("want a ghost ClusterError, got %+v", resp.Msg.Error)
	}
}

// GetResource on a Secret must also redact values (E4) — it returns the FULL
// object, so without the redaction it would leak what ListResources hides.
func TestResourceService_GetResource_RedactsSecret(t *testing.T) {
	prov := &fakeProvider{clients: map[string]dynamic.Interface{"boba": fakeExplorerDyn(uSecret("default", "db-creds"))}}
	svc := NewResourceService(prov, NewInsecureAuthenticator())
	resp, err := svc.GetResource(context.Background(), connect.NewRequest(&kubeswiftv1.GetResourceRequest{
		Cluster: "boba", Kind: "secrets", Namespace: "default", Name: "db-creds",
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"c3VwZXItc2VjcmV0", "super-secret", "dG9wLXNlY3JldA==", "top-secret"} {
		if strings.Contains(resp.Msg.Yaml, leak) || strings.Contains(resp.Msg.Json, leak) {
			t.Fatalf("GetResource leaked secret value %q", leak)
		}
	}
	if !strings.Contains(resp.Msg.Json, "db-creds") || !strings.Contains(resp.Msg.Json, "Opaque") {
		t.Errorf("GetResource should keep Secret metadata + type, got %s", resp.Msg.Json)
	}
}

func TestResourceService_DeleteResource(t *testing.T) {
	prov := &fakeProvider{clients: map[string]dynamic.Interface{"boba": fakeExplorerDyn(mkNode("miles", true))}}
	svc := NewResourceService(prov, NewInsecureAuthenticator())

	// Missing name -> InvalidArgument.
	_, err := svc.DeleteResource(context.Background(), connect.NewRequest(&kubeswiftv1.DeleteResourceRequest{Cluster: "boba", Kind: "nodes"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing name: want InvalidArgument, got %v", err)
	}
	// Delete then confirm gone.
	if _, err := svc.DeleteResource(context.Background(), connect.NewRequest(&kubeswiftv1.DeleteResourceRequest{Cluster: "boba", Kind: "nodes", Name: "miles"})); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ := svc.ListResources(context.Background(), connect.NewRequest(&kubeswiftv1.ListResourcesRequest{Cluster: "boba", Kind: "nodes"}))
	if len(list.Msg.Resources) != 0 {
		t.Errorf("node should be deleted, got %d", len(list.Msg.Resources))
	}
}

func TestResourceService_ApplyResource_Validation(t *testing.T) {
	svc := NewResourceService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": fakeExplorerDyn()}}, NewInsecureAuthenticator())
	cases := []struct {
		name string
		req  *kubeswiftv1.ApplyResourceRequest
	}{
		{"missing cluster", &kubeswiftv1.ApplyResourceRequest{Kind: "nodes", Yaml: "apiVersion: v1\nkind: Node\nmetadata:\n  name: x"}},
		{"unknown kind", &kubeswiftv1.ApplyResourceRequest{Cluster: "boba", Kind: "frobnicators", Yaml: "x"}},
		{"empty yaml", &kubeswiftv1.ApplyResourceRequest{Cluster: "boba", Kind: "nodes", Yaml: "   "}},
		{"non-object yaml", &kubeswiftv1.ApplyResourceRequest{Cluster: "boba", Kind: "nodes", Yaml: "- a\n- b"}},
		{"no identity", &kubeswiftv1.ApplyResourceRequest{Cluster: "boba", Kind: "nodes", Yaml: "metadata: {}"}},
		{"namespaced needs ns", &kubeswiftv1.ApplyResourceRequest{Cluster: "boba", Kind: "secrets", Yaml: "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s"}},
	}
	for _, tc := range cases {
		_, err := svc.ApplyResource(context.Background(), connect.NewRequest(tc.req))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: want InvalidArgument, got %v", tc.name, err)
		}
	}
}

// ApplyResource server-side-applies a (cluster-scoped) object as the user.
func TestResourceService_ApplyResource_Creates(t *testing.T) {
	prov := &fakeProvider{clients: map[string]dynamic.Interface{"boba": fakeExplorerDyn()}}
	svc := NewResourceService(prov, NewInsecureAuthenticator())
	y := "apiVersion: v1\nkind: Node\nmetadata:\n  name: newnode\n  resourceVersion: \"999\"\n"
	resp, err := svc.ApplyResource(context.Background(), connect.NewRequest(&kubeswiftv1.ApplyResourceRequest{Cluster: "boba", Kind: "nodes", Yaml: y}))
	if err != nil {
		t.Skipf("fake dynamic client SSA Apply unsupported in this client-go: %v", err)
	}
	if !strings.Contains(resp.Msg.Json, "newnode") {
		t.Errorf("applied node not echoed back: %s", resp.Msg.Json)
	}
}
