package gateway

import (
	"context"
	"errors"
	"testing"

	connect "connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
)

func uSandbox(ns, name, phase, image string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "sandbox.kubeswift.io/v1alpha1",
		"kind":       "SwiftSandbox",
		"metadata":   map[string]interface{}{"namespace": ns, "name": name},
		"spec": map[string]interface{}{
			"image":   image,
			"cpu":     int64(1),
			"memory":  "512Mi",
			"command": []interface{}{"sh", "-c"},
			"args":    []interface{}{"echo hi"},
		},
		"status": map[string]interface{}{"phase": phase},
	}}
}

func uSandboxPool(ns, name, phase string, minWarm, warm int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "sandbox.kubeswift.io/v1alpha1",
		"kind":       "SwiftSandboxPool",
		"metadata":   map[string]interface{}{"namespace": ns, "name": name},
		"spec":       map[string]interface{}{"image": "alpine", "minWarm": minWarm, "memory": "256Mi"},
		"status":     map[string]interface{}{"phase": phase, "warmReplicas": warm},
	}}
}

func fakeSandboxDyn(objs ...*unstructured.Unstructured) dynamic.Interface {
	// Track each object under its EXPLICIT GVR — the dynamic fake's default
	// UnsafeGuessKindToResource maps SwiftSandbox to "swiftsandboxs", not the CRD's
	// actual "swiftsandboxes" plural, so a constructor-seeded object lands under the
	// wrong resource. Tracker().Create with the real GVR avoids that.
	c := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			swiftSandboxGVR:     "SwiftSandboxList",
			swiftSandboxPoolGVR: "SwiftSandboxPoolList",
		})
	for _, o := range objs {
		gvr := swiftSandboxGVR
		if o.GetKind() == "SwiftSandboxPool" {
			gvr = swiftSandboxPoolGVR
		}
		if err := c.Tracker().Create(gvr, o, o.GetNamespace()); err != nil {
			panic(err)
		}
	}
	return c
}

func TestSandboxService_ListSandboxes_FanOutMergeAndPartialError(t *testing.T) {
	prov := &fakeProvider{
		clients: map[string]dynamic.Interface{
			"boba":  fakeSandboxDyn(uSandbox("default", "sb-a", "Running", "alpine"), uSandbox("default", "sb-b", "Completed", "alpine")),
			"miles": fakeSandboxDyn(uSandbox("default", "sb-c", "Running", "python")),
		},
		errs: map[string]error{"down": errors.New("connection refused")},
	}
	svc := NewSandboxService(prov, NewInsecureAuthenticator())

	resp, err := svc.ListSandboxes(context.Background(), connect.NewRequest(&kubeswiftv1.ListSandboxesRequest{}))
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if len(resp.Msg.Sandboxes) != 3 {
		t.Fatalf("got %d sandboxes, want 3", len(resp.Msg.Sandboxes))
	}
	if len(resp.Msg.Errors) != 1 || resp.Msg.Errors[0].Cluster != "down" {
		t.Errorf("want one partial-fleet error for 'down', got %+v", resp.Msg.Errors)
	}
	// cluster dimension stamped + merged sort by (cluster, ns, name).
	if resp.Msg.Sandboxes[0].GetRef().GetCluster() != "boba" || resp.Msg.Sandboxes[2].GetRef().GetCluster() != "miles" {
		t.Errorf("not sorted by cluster: %v / %v", resp.Msg.Sandboxes[0].GetRef(), resp.Msg.Sandboxes[2].GetRef())
	}
	// flat-row fields mapped (image, default network mode).
	if resp.Msg.Sandboxes[0].GetImage() != "alpine" || resp.Msg.Sandboxes[0].GetNetworkMode() != "restricted" {
		t.Errorf("row map wrong: %+v", resp.Msg.Sandboxes[0])
	}
}

func TestSandboxService_ListSandboxes_PhaseFilter(t *testing.T) {
	prov := &fakeProvider{clients: map[string]dynamic.Interface{
		"boba": fakeSandboxDyn(uSandbox("default", "sb-a", "Running", "alpine"), uSandbox("default", "sb-b", "Completed", "alpine")),
	}}
	svc := NewSandboxService(prov, NewInsecureAuthenticator())
	resp, err := svc.ListSandboxes(context.Background(), connect.NewRequest(&kubeswiftv1.ListSandboxesRequest{Phase: "Completed"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Sandboxes) != 1 || resp.Msg.Sandboxes[0].GetRef().GetName() != "sb-b" {
		t.Errorf("phase filter wrong: %+v", resp.Msg.Sandboxes)
	}
}

func TestSandboxService_ListSandboxPools(t *testing.T) {
	prov := &fakeProvider{clients: map[string]dynamic.Interface{
		"boba": fakeSandboxDyn(uSandboxPool("default", "pool-a", "Ready", 2, 2)),
	}}
	svc := NewSandboxService(prov, NewInsecureAuthenticator())
	resp, err := svc.ListSandboxPools(context.Background(), connect.NewRequest(&kubeswiftv1.ListSandboxPoolsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Pools) != 1 {
		t.Fatalf("got %d pools, want 1", len(resp.Msg.Pools))
	}
	p := resp.Msg.Pools[0]
	if p.GetPhase() != "Ready" || p.GetMinWarm() != 2 || p.GetWarmReplicas() != 2 || p.GetRef().GetCluster() != "boba" {
		t.Errorf("pool map wrong: %+v", p)
	}
}

func TestSandboxService_GetSandboxDetail(t *testing.T) {
	prov := &fakeProvider{clients: map[string]dynamic.Interface{
		"boba": fakeSandboxDyn(uSandbox("default", "sb-a", "Running", "alpine")),
	}}
	svc := NewSandboxService(prov, NewInsecureAuthenticator())
	resp, err := svc.GetSandboxDetail(context.Background(), connect.NewRequest(&kubeswiftv1.GetSandboxDetailRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "sb-a"},
	}))
	if err != nil {
		t.Fatalf("GetSandboxDetail: %v", err)
	}
	if resp.Msg.GetSandbox().GetImage() != "alpine" {
		t.Errorf("sandbox image: %q", resp.Msg.GetSandbox().GetImage())
	}
	spec := resp.Msg.GetSpec()
	if spec.GetImage() != "alpine" || len(spec.GetCommand()) != 2 || spec.GetArgs()[0] != "echo hi" {
		t.Errorf("spec map wrong: %+v", spec)
	}
}

func TestSandboxService_CreateSandbox(t *testing.T) {
	boba := fakeSandboxDyn()
	svc := NewSandboxService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}}, NewInsecureAuthenticator())

	resp, err := svc.CreateSandbox(context.Background(), connect.NewRequest(&kubeswiftv1.CreateSandboxRequest{
		Cluster: "boba", Namespace: "default", Name: "sb-1",
		Image: "alpine:3.20", Command: []string{"sh", "-c"}, Args: []string{"echo hi"},
		Env: map[string]string{"A": "1"}, NetworkMode: "open", Cpu: 2, MemoryMib: 512, PoolRef: "p1",
	}))
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if resp.Msg.GetRef().GetName() != "sb-1" || resp.Msg.GetRef().GetCluster() != "boba" {
		t.Errorf("ref: %+v", resp.Msg.GetRef())
	}
	got, err := boba.Resource(swiftSandboxGVR).Namespace("default").Get(context.Background(), "sb-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("created sandbox not found: %v", err)
	}
	image, _, _ := unstructured.NestedString(got.Object, "spec", "image")
	mem, _, _ := unstructured.NestedString(got.Object, "spec", "memory")
	poolRef, _, _ := unstructured.NestedString(got.Object, "spec", "poolRef", "name")
	netMode, _, _ := unstructured.NestedString(got.Object, "spec", "network", "mode")
	if image != "alpine:3.20" || mem != "512Mi" || poolRef != "p1" || netMode != "open" {
		t.Errorf("spec wrong: image=%q mem=%q pool=%q net=%q", image, mem, poolRef, netMode)
	}

	// Missing image -> InvalidArgument (a clear gateway error ahead of the webhook).
	_, err = svc.CreateSandbox(context.Background(), connect.NewRequest(&kubeswiftv1.CreateSandboxRequest{Cluster: "boba", Namespace: "default", Name: "x"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing image: want InvalidArgument, got %v", err)
	}
	// A name clash fails loudly.
	_, err = svc.CreateSandbox(context.Background(), connect.NewRequest(&kubeswiftv1.CreateSandboxRequest{Cluster: "boba", Namespace: "default", Name: "sb-1", Image: "x"}))
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("duplicate name should be AlreadyExists, got %v", err)
	}
}

func TestSandboxService_DeleteSandbox(t *testing.T) {
	boba := fakeSandboxDyn(uSandbox("default", "sb-a", "Running", "alpine"))
	svc := NewSandboxService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}}, NewInsecureAuthenticator())

	if _, err := svc.DeleteSandbox(context.Background(), connect.NewRequest(&kubeswiftv1.DeleteSandboxRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing ref: want InvalidArgument, got %v", err)
	}
	if _, err := svc.DeleteSandbox(context.Background(), connect.NewRequest(&kubeswiftv1.DeleteSandboxRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "sb-a"},
	})); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ := boba.Resource(swiftSandboxGVR).Namespace("default").List(context.Background(), metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Errorf("sandbox should be deleted, got %d", len(list.Items))
	}
}

func TestSandboxService_CreateSandboxPool(t *testing.T) {
	boba := fakeSandboxDyn()
	svc := NewSandboxService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}}, NewInsecureAuthenticator())

	if _, err := svc.CreateSandboxPool(context.Background(), connect.NewRequest(&kubeswiftv1.CreateSandboxPoolRequest{
		Cluster: "boba", Namespace: "default", Name: "pool-1", Image: "alpine", MinWarm: 3, MaxWarm: 6, MemoryMib: 256,
	})); err != nil {
		t.Fatalf("CreateSandboxPool: %v", err)
	}
	got, err := boba.Resource(swiftSandboxPoolGVR).Namespace("default").Get(context.Background(), "pool-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pool not found: %v", err)
	}
	minWarm, _, _ := unstructured.NestedInt64(got.Object, "spec", "minWarm")
	mem, _, _ := unstructured.NestedString(got.Object, "spec", "memory")
	if minWarm != 3 || mem != "256Mi" {
		t.Errorf("pool spec wrong: minWarm=%d mem=%q", minWarm, mem)
	}
}
