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

	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
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

func uNode(name string, ready, schedulable bool) *unstructured.Unstructured {
	status := "False"
	if ready {
		status = "True"
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{"unschedulable": !schedulable},
		"status": map[string]interface{}{
			"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": status}},
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
			swiftGuestGVR:     "SwiftGuestList",
			podGVR:            "PodList",
			nodeGVR:           "NodeList",
			swiftMigrationGVR: "SwiftMigrationList",
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

func TestGuestService_MigrateGuest(t *testing.T) {
	boba := fakeDyn(uGuest("default", "vm-a", "Running"))
	svc := NewGuestService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}}, NewInsecureAuthenticator())

	resp, err := svc.MigrateGuest(context.Background(), connect.NewRequest(&kubeswiftv1.MigrateGuestRequest{
		Ref:        &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "vm-a"},
		TargetNode: "miles",
		Mode:       "offline",
	}))
	if err != nil {
		t.Fatalf("MigrateGuest: %v", err)
	}
	if resp.Msg.Migration.GetCluster() != "boba" || resp.Msg.Migration.GetNamespace() != "default" {
		t.Errorf("unexpected migration ref: %+v", resp.Msg.Migration)
	}
	// A SwiftMigration was actually created in the namespace.
	migs, _ := boba.Resource(swiftMigrationGVR).Namespace("default").List(context.Background(), metav1.ListOptions{})
	if len(migs.Items) != 1 {
		t.Errorf("want 1 SwiftMigration created, got %d", len(migs.Items))
	}

	// target_node is required, not silently defaulted.
	if _, err := svc.MigrateGuest(context.Background(), connect.NewRequest(&kubeswiftv1.MigrateGuestRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "vm-a"},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing target_node should be InvalidArgument, got %v", err)
	}
}

func TestGuestService_CreateGuest(t *testing.T) {
	boba := fakeDyn()
	svc := NewGuestService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}}, NewInsecureAuthenticator())

	resp, err := svc.CreateGuest(context.Background(), connect.NewRequest(&kubeswiftv1.CreateGuestRequest{
		Cluster: "boba", Namespace: "default", Name: "web-1",
		ImageRef: "ubuntu-noble", GuestClassRef: "small", SeedProfileRef: "default-seed", RunPolicy: "Running",
		Ports: []*kubeswiftv1.GuestPortSpec{{Name: "ssh", Port: 22, Expose: "ClusterIP"}},
	}))
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	if resp.Msg.Ref.GetName() != "web-1" || resp.Msg.Ref.GetCluster() != "boba" {
		t.Errorf("unexpected ref: %+v", resp.Msg.Ref)
	}
	// The SwiftGuest exists with the wizard's spec.
	got, err := boba.Resource(swiftGuestGVR).Namespace("default").Get(context.Background(), "web-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("created guest not found: %v", err)
	}
	image, _, _ := unstructured.NestedString(got.Object, "spec", "imageRef", "name")
	class, _, _ := unstructured.NestedString(got.Object, "spec", "guestClassRef", "name")
	ports, _, _ := unstructured.NestedSlice(got.Object, "spec", "network", "ports")
	if image != "ubuntu-noble" || class != "small" || len(ports) != 1 {
		t.Errorf("spec wrong: image=%q class=%q ports=%d", image, class, len(ports))
	}

	// A name clash fails loudly (no silent overwrite of an existing VM).
	_, err = svc.CreateGuest(context.Background(), connect.NewRequest(&kubeswiftv1.CreateGuestRequest{
		Cluster: "boba", Namespace: "default", Name: "web-1", ImageRef: "x", GuestClassRef: "small",
	}))
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("duplicate name should be AlreadyExists, got %v", err)
	}
}

func TestGuestService_DeleteGuest(t *testing.T) {
	boba := fakeDyn(uGuest("default", "vm-a", "Running"))
	svc := NewGuestService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}}, NewInsecureAuthenticator())

	if _, err := svc.DeleteGuest(context.Background(), connect.NewRequest(&kubeswiftv1.DeleteGuestRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("missing ref: want InvalidArgument, got %v", err)
	}
	if _, err := svc.DeleteGuest(context.Background(), connect.NewRequest(&kubeswiftv1.DeleteGuestRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "vm-a"},
	})); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, _ := boba.Resource(swiftGuestGVR).Namespace("default").List(context.Background(), metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Errorf("guest should be deleted, got %d", len(list.Items))
	}
}

func uEvent(ns, name, objName, typ, reason, msg, lastTs string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Event",
		"metadata":       map[string]interface{}{"namespace": ns, "name": name, "uid": name},
		"type":           typ,
		"reason":         reason,
		"message":        msg,
		"count":          int64(1),
		"lastTimestamp":  lastTs,
		"involvedObject": map[string]interface{}{"kind": "SwiftGuest", "name": objName},
	}}
}

func TestGuestService_GetGuestEvents(t *testing.T) {
	guest := uGuest("default", "vm-a", "Pending")
	ev1 := uEvent("default", "ev1", "vm-a", "Warning", "FailedScheduling", "0/3 nodes available", "2026-01-01T00:00:00Z")
	ev2 := uEvent("default", "ev2", "vm-a", "Normal", "Scheduled", "assigned to boba", "2026-01-01T00:01:00Z")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			swiftGuestGVR: "SwiftGuestList",
			eventsGVR:     "EventList",
		}, guest, ev1, ev2)
	svc := NewGuestService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": dyn}}, NewInsecureAuthenticator())

	resp, err := svc.GetGuestEvents(context.Background(), connect.NewRequest(&kubeswiftv1.GetGuestEventsRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "vm-a"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Events) != 2 {
		t.Fatalf("want 2 events, got %d", len(resp.Msg.Events))
	}
	// Newest first.
	if resp.Msg.Events[0].Reason != "Scheduled" {
		t.Errorf("want newest event (Scheduled) first, got %q", resp.Msg.Events[0].Reason)
	}
	if resp.Msg.Events[0].Object != "SwiftGuest/vm-a" {
		t.Errorf("object = %q, want SwiftGuest/vm-a", resp.Msg.Events[0].Object)
	}
}

// GetGuestDetail surfaces the structured spec (for Clone).
func TestGuestService_GetGuestDetail_Spec(t *testing.T) {
	g := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "swift.kubeswift.io/v1alpha1", "kind": "SwiftGuest",
		"metadata": map[string]interface{}{"namespace": "default", "name": "vm-a"},
		"spec": map[string]interface{}{
			"imageRef":      map[string]interface{}{"name": "ubuntu-noble"},
			"guestClassRef": map[string]interface{}{"name": "small"},
			"runPolicy":     "Running",
		},
	}}
	boba := fakeDyn(g)
	svc := NewGuestService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}}, NewInsecureAuthenticator())
	resp, err := svc.GetGuestDetail(context.Background(), connect.NewRequest(&kubeswiftv1.GetGuestDetailRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "vm-a"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Spec == nil || resp.Msg.Spec.ImageRef != "ubuntu-noble" || resp.Msg.Spec.GuestClassRef != "small" {
		t.Errorf("spec not surfaced for clone: %+v", resp.Msg.Spec)
	}
}

func TestGuestService_GetGuestDetail_Network(t *testing.T) {
	g := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "swift.kubeswift.io/v1alpha1", "kind": "SwiftGuest",
		"metadata": map[string]interface{}{"namespace": "default", "name": "vm-a"},
		"spec": map[string]interface{}{
			"imageRef":      map[string]interface{}{"name": "ubuntu-noble"},
			"guestClassRef": map[string]interface{}{"name": "small"},
			"network": map[string]interface{}{
				"binding": "nat",
				"ports": []interface{}{
					map[string]interface{}{"name": "ssh", "port": int64(22), "targetPort": int64(22), "protocol": "TCP", "expose": "ClusterIP"},
					map[string]interface{}{"name": "web", "port": int64(80), "targetPort": int64(8080), "protocol": "TCP"},
				},
			},
		},
		"status": map[string]interface{}{
			"network": map[string]interface{}{
				"egress":       "ClusterServices",
				"serviceRef":   map[string]interface{}{"name": "vm-a-svc"},
				"exposedPorts": []interface{}{map[string]interface{}{"name": "ssh", "port": int64(22), "targetPort": int64(22), "protocol": "TCP"}},
			},
			"conditions": []interface{}{
				map[string]interface{}{"type": "EgressReady", "status": "True", "reason": "Reachable", "lastTransitionTime": "2026-06-25T00:00:00Z"},
				map[string]interface{}{"type": "ServiceReady", "status": "True", "reason": "Ready", "lastTransitionTime": "2026-06-25T00:00:00Z"},
				map[string]interface{}{"type": "PortsProgrammed", "status": "False", "reason": "Pending", "lastTransitionTime": "2026-06-25T00:00:00Z"},
			},
		},
	}}
	svc := NewGuestService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": fakeDyn(g)}}, NewInsecureAuthenticator())
	resp, err := svc.GetGuestDetail(context.Background(), connect.NewRequest(&kubeswiftv1.GetGuestDetailRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "vm-a"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	n := resp.Msg.Network
	if n == nil {
		t.Fatal("network not surfaced")
	}
	if n.Binding != "nat" || n.Egress != "ClusterServices" || n.ServiceRef != "vm-a-svc" {
		t.Errorf("bad network header: %+v", n)
	}
	if !n.EgressReady || !n.ServiceReady || n.PortsProgrammed {
		t.Errorf("condition mapping wrong: egressReady=%v serviceReady=%v portsProgrammed=%v", n.EgressReady, n.ServiceReady, n.PortsProgrammed)
	}
	if len(n.Ports) != 2 {
		t.Fatalf("want 2 ports, got %d", len(n.Ports))
	}
	if n.Ports[0].Name != "ssh" || n.Ports[0].Expose != "ClusterIP" || !n.Ports[0].Programmed {
		t.Errorf("ssh port wrong: %+v", n.Ports[0])
	}
	if n.Ports[1].Name != "web" || n.Ports[1].Expose != "" || n.Ports[1].Programmed {
		t.Errorf("web port should be DNAT-only + not programmed: %+v", n.Ports[1])
	}
}

func TestGuestService_GetGuestDetail_NoNetwork(t *testing.T) {
	g := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "swift.kubeswift.io/v1alpha1", "kind": "SwiftGuest",
		"metadata": map[string]interface{}{"namespace": "default", "name": "vm-b"},
		"spec":     map[string]interface{}{"imageRef": map[string]interface{}{"name": "ubuntu"}, "guestClassRef": map[string]interface{}{"name": "small"}},
	}}
	svc := NewGuestService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": fakeDyn(g)}}, NewInsecureAuthenticator())
	resp, err := svc.GetGuestDetail(context.Background(), connect.NewRequest(&kubeswiftv1.GetGuestDetailRequest{
		Ref: &kubeswiftv1.ObjectRef{Cluster: "boba", Namespace: "default", Name: "vm-b"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Network != nil {
		t.Errorf("expected nil network for guest without spec.network, got %+v", resp.Msg.Network)
	}
}

func TestGuestService_CreateGuest_Validation(t *testing.T) {
	svc := NewGuestService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": fakeDyn()}}, NewInsecureAuthenticator())
	cases := []struct {
		name string
		req  *kubeswiftv1.CreateGuestRequest
	}{
		{"missing name", &kubeswiftv1.CreateGuestRequest{Cluster: "boba", Namespace: "default", ImageRef: "x", GuestClassRef: "small"}},
		{"missing class", &kubeswiftv1.CreateGuestRequest{Cluster: "boba", Namespace: "default", Name: "g", ImageRef: "x"}},
		{"no boot source", &kubeswiftv1.CreateGuestRequest{Cluster: "boba", Namespace: "default", Name: "g", GuestClassRef: "small"}},
		{"two boot sources", &kubeswiftv1.CreateGuestRequest{Cluster: "boba", Namespace: "default", Name: "g", ImageRef: "x", KernelRef: "y", GuestClassRef: "small"}},
	}
	for _, tc := range cases {
		if _, err := svc.CreateGuest(context.Background(), connect.NewRequest(tc.req)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: want InvalidArgument, got %v", tc.name, err)
		}
	}
}
