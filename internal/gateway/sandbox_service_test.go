package gateway

import (
	"context"
	"errors"
	"testing"

	connect "connectrpc.com/connect"
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
