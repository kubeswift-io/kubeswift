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
	"k8s.io/client-go/rest"

	kubeswiftv1 "github.com/projectbeskar/kubeswift/gen/kubeswift/v1"
)

// fakeProvider stands in for the ClientPool: a fake dynamic client per member,
// plus members whose client construction fails (the unreachable case), and an
// optional per-member Prometheus endpoint (telemetry plane).
type fakeProvider struct {
	clients map[string]dynamic.Interface
	errs    map[string]error
	prom    map[string]string
}

func (f *fakeProvider) PrometheusEndpoint(cluster string) string {
	return f.prom[cluster]
}

func (f *fakeProvider) RestConfigFor(cluster string, _ Identity) (*rest.Config, error) {
	if _, ok := f.clients[cluster]; ok {
		return &rest.Config{Host: "http://fake"}, nil
	}
	return nil, errors.New("no config for " + cluster)
}

func (f *fakeProvider) DynamicFor(cluster string, _ Identity) (dynamic.Interface, error) {
	if e, ok := f.errs[cluster]; ok {
		return nil, e
	}
	if c, ok := f.clients[cluster]; ok {
		return c, nil
	}
	return nil, errors.New("no client for " + cluster)
}

func (f *fakeProvider) Members() []string {
	var m []string
	for k := range f.clients {
		m = append(m, k)
	}
	for k := range f.errs {
		m = append(m, k)
	}
	return m
}

func uGuest(ns, name, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "swift.kubeswift.io/v1alpha1",
		"kind":       "SwiftGuest",
		"metadata":   map[string]interface{}{"namespace": ns, "name": name},
		"spec": map[string]interface{}{
			"guestClassRef": map[string]interface{}{"name": "default"},
			"imageRef":      map[string]interface{}{"name": "ubuntu-noble"},
		},
		"status": map[string]interface{}{"phase": phase},
	}}
}

func uPod(ns, name, guest string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"namespace": ns, "name": name,
			"labels": map[string]interface{}{guestPodLabel: guest},
		},
	}}
}

func fakeDyn(objs ...*unstructured.Unstructured) dynamic.Interface {
	ro := make([]runtime.Object, len(objs))
	for i, o := range objs {
		ro[i] = o
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			swiftGuestGVR: "SwiftGuestList",
			podGVR:        "PodList",
		}, ro...)
}

func TestGuestService_ListGuests_FanOutMergeAndPartialError(t *testing.T) {
	prov := &fakeProvider{
		clients: map[string]dynamic.Interface{
			"boba":  fakeDyn(uGuest("default", "vm-a", "Running"), uGuest("default", "vm-b", "Pending")),
			"miles": fakeDyn(uGuest("default", "vm-c", "Running")),
		},
		errs: map[string]error{"down": errors.New("connection refused")},
	}
	svc := NewGuestService(prov, NewInsecureAuthenticator())

	resp, err := svc.ListGuests(context.Background(), connect.NewRequest(&kubeswiftv1.ListGuestsRequest{}))
	if err != nil {
		t.Fatalf("ListGuests: %v", err)
	}
	if len(resp.Msg.Guests) != 3 {
		t.Fatalf("got %d guests, want 3 (boba+miles)", len(resp.Msg.Guests))
	}
	// The unreachable member surfaces as a per-cluster error, not a fatal one.
	if len(resp.Msg.Errors) != 1 || resp.Msg.Errors[0].Cluster != "down" {
		t.Errorf("want one partial-fleet error for 'down', got %+v", resp.Msg.Errors)
	}
	// cluster dimension stamped; merged result sorted by (cluster, ns, name).
	if resp.Msg.Guests[0].GetRef().GetCluster() != "boba" || resp.Msg.Guests[2].GetRef().GetCluster() != "miles" {
		t.Errorf("not sorted by cluster: %v / %v", resp.Msg.Guests[0].GetRef(), resp.Msg.Guests[2].GetRef())
	}
}

func TestGuestService_ListGuests_PhaseFilter(t *testing.T) {
	prov := &fakeProvider{clients: map[string]dynamic.Interface{
		"boba": fakeDyn(uGuest("default", "vm-a", "Running"), uGuest("default", "vm-b", "Pending")),
	}}
	svc := NewGuestService(prov, NewInsecureAuthenticator())
	resp, err := svc.ListGuests(context.Background(), connect.NewRequest(&kubeswiftv1.ListGuestsRequest{Phase: "Running"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Guests) != 1 || resp.Msg.Guests[0].GetRef().GetName() != "vm-a" {
		t.Errorf("phase filter wrong: %+v", resp.Msg.Guests)
	}
}

func TestGuestService_TargetClusters(t *testing.T) {
	prov := &fakeProvider{clients: map[string]dynamic.Interface{
		"boba": fakeDyn(), "miles": fakeDyn(), "frida": fakeDyn(),
	}}
	svc := NewGuestService(prov, NewInsecureAuthenticator())

	all := svc.targetClusters(nil) // nil selector = whole fleet, sorted
	if len(all) != 3 || all[0] != "boba" {
		t.Errorf("nil selector should return the sorted fleet: %v", all)
	}
	sub := svc.targetClusters(&kubeswiftv1.ClusterSelector{Clusters: []string{"miles", "ghost"}})
	if len(sub) != 1 || sub[0] != "miles" { // unknown 'ghost' dropped
		t.Errorf("subset should intersect registered members: %v", sub)
	}
}

func TestGuestService_StartStopGuest(t *testing.T) {
	boba := fakeDyn(uGuest("default", "vm-a", "Running"), uPod("default", "vm-a", "vm-a"))
	svc := NewGuestService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}}, NewInsecureAuthenticator())
	ref := &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "vm-a"}
	podCount := func() int {
		l, _ := boba.Resource(podGVR).Namespace("default").List(context.Background(), metav1.ListOptions{})
		return len(l.Items)
	}

	start, err := svc.StartGuest(context.Background(), connect.NewRequest(&kubeswiftv1.GuestActionRequest{Ref: ref}))
	if err != nil {
		t.Fatalf("StartGuest: %v", err)
	}
	if start.Msg.Guest.GetRef().GetName() != "vm-a" {
		t.Errorf("StartGuest returned %+v", start.Msg.Guest)
	}
	// StartGuest must NOT delete the launcher pod (the controller recreates a
	// stopped one; a running one keeps running).
	if got := podCount(); got != 1 {
		t.Errorf("StartGuest left %d launcher pods, want 1", got)
	}

	if _, err := svc.StopGuest(context.Background(), connect.NewRequest(&kubeswiftv1.GuestActionRequest{Ref: ref})); err != nil {
		t.Fatalf("StopGuest: %v", err)
	}
	// StopGuest must delete the launcher pod — the stop guard is reactive only,
	// so a runPolicy patch alone would leave the VM running.
	if got := podCount(); got != 0 {
		t.Errorf("StopGuest left %d launcher pods, want 0", got)
	}

	// A missing ref is rejected, not silently a no-op.
	if _, err := svc.StartGuest(context.Background(), connect.NewRequest(&kubeswiftv1.GuestActionRequest{})); err == nil {
		t.Error("StartGuest with no ref should be InvalidArgument")
	}
}
