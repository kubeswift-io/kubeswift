package swiftstoragelocation

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
	kscheme "github.com/kubeswift-io/kubeswift/internal/scheme"
)

func clusterLoc(name string, isDefault bool, repo string) *storagev1alpha1.SwiftClusterStorageLocation {
	return &storagev1alpha1.SwiftClusterStorageLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec:       storagev1alpha1.StorageLocationSpec{Default: isDefault, OCI: &storagev1alpha1.OCILocation{Repository: repo}},
	}
}

func nsLoc(ns, name string, isDefault bool) *storagev1alpha1.SwiftStorageLocation {
	return &storagev1alpha1.SwiftStorageLocation{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 1},
		Spec:       storagev1alpha1.StorageLocationSpec{Default: isDefault, OCI: &storagev1alpha1.OCILocation{Repository: "registry.example.com/" + ns}},
	}
}

func newClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(kscheme.Scheme).WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.SwiftClusterStorageLocation{}, &storagev1alpha1.SwiftStorageLocation{}).Build()
}

func cond(conds []metav1.Condition, t string) *metav1.Condition {
	return meta.FindStatusCondition(conds, t)
}

func reconcileCluster(t *testing.T, r *ClusterReconciler, name string) storagev1alpha1.SwiftClusterStorageLocation {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	var got storagev1alpha1.SwiftClusterStorageLocation
	if err := r.Get(context.Background(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

var alwaysReachable Prober = func(context.Context, *storagev1alpha1.OCILocation) (bool, string) {
	return true, "GET /v2/ answered 401"
}

func TestClusterLocation_ValidReadyReachable(t *testing.T) {
	r := &ClusterReconciler{Client: newClient(clusterLoc("main", true, "registry.example.com/kubeswift")), Probe: alwaysReachable}
	got := reconcileCluster(t, r, "main")
	for _, ct := range []string{storagev1alpha1.ConditionValid, storagev1alpha1.ConditionReady, storagev1alpha1.ConditionReachable} {
		if c := cond(got.Status.Conditions, ct); c == nil || c.Status != metav1.ConditionTrue {
			t.Errorf("%s = %+v, want True", ct, c)
		}
	}
	if got.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d", got.Status.ObservedGeneration)
	}
}

// Two cluster defaults: objects that need the default must not get one picked
// for them, so both report it and are not Ready.
func TestClusterLocation_TwoDefaultsAreBothNotReady(t *testing.T) {
	r := &ClusterReconciler{Client: newClient(
		clusterLoc("a", true, "registry.example.com/a"),
		clusterLoc("b", true, "registry.example.com/b"),
		clusterLoc("c", false, "registry.example.com/c"),
	), Probe: alwaysReachable}
	for name, other := range map[string]string{"a": "b", "b": "a"} {
		got := reconcileCluster(t, r, name)
		c := cond(got.Status.Conditions, storagev1alpha1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonAmbiguousDefault || !strings.Contains(c.Message, other) {
			t.Errorf("%s Ready = %+v, want False/%s naming %s", name, c, ReasonAmbiguousDefault, other)
		}
	}
	if c := cond(reconcileCluster(t, r, "c").Status.Conditions, storagev1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("a non-default location is unaffected; Ready = %+v", c)
	}
}

// The manager resyncs every object every 30s and a status write is an event
// of its own, so reconciles come far more often than probes should. A
// registry is probed once per refresh interval, and again at once when the
// spec changes.
func TestClusterLocation_ProbesOncePerRefresh(t *testing.T) {
	probes := 0
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	c := newClient(clusterLoc("main", false, "registry.example.com/kubeswift"))
	r := &ClusterReconciler{Client: c, Now: func() time.Time { return now },
		Probe: func(context.Context, *storagev1alpha1.OCILocation) (bool, string) { probes++; return true, "answered" }}
	reconcile := func() ctrl.Result {
		t.Helper()
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "main"}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	reconcile()
	for i := 0; i < 5; i++ {
		now = now.Add(30 * time.Second)
		res := reconcile()
		if res.RequeueAfter <= 0 || res.RequeueAfter > reachabilityRefresh {
			t.Fatalf("RequeueAfter = %v, want the time left until the next probe", res.RequeueAfter)
		}
	}
	if probes != 1 {
		t.Fatalf("six reconciles within the refresh interval probed %d times, want 1", probes)
	}

	now = now.Add(reachabilityRefresh)
	reconcile()
	if probes != 2 {
		t.Fatalf("after the refresh interval: %d probes, want 2", probes)
	}

	var loc storagev1alpha1.SwiftClusterStorageLocation
	if err := c.Get(context.Background(), types.NamespacedName{Name: "main"}, &loc); err != nil {
		t.Fatal(err)
	}
	loc.Spec.OCI.Insecure = true
	loc.Generation++
	if err := c.Update(context.Background(), &loc); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if probes != 3 {
		t.Fatalf("after a spec change: %d probes, want 3", probes)
	}
	if got := reconcileCluster(t, r, "main"); cond(got.Status.Conditions, storagev1alpha1.ConditionReachable) == nil {
		t.Error("a reconcile that skips the probe must keep the Reachable condition")
	}
}

func TestClusterLocation_InvalidIsNotProbed(t *testing.T) {
	probed := false
	r := &ClusterReconciler{Client: newClient(clusterLoc("bad", false, "registry.example.com/kubeswift:v1")),
		Probe: func(context.Context, *storagev1alpha1.OCILocation) (bool, string) { probed = true; return true, "" }}
	got := reconcileCluster(t, r, "bad")
	if c := cond(got.Status.Conditions, storagev1alpha1.ConditionValid); c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "tag or digest") {
		t.Errorf("Valid = %+v", c)
	}
	if c := cond(got.Status.Conditions, storagev1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %+v", c)
	}
	if probed || cond(got.Status.Conditions, storagev1alpha1.ConditionReachable) != nil {
		t.Error("an invalid location must not be probed")
	}
}

// A namespace's location is never probed (the controller does not make
// requests to hosts a namespace chose), and its default competes only with
// defaults in the same namespace.
func TestNamespaceLocation_NoProbeAndPerNamespaceDefault(t *testing.T) {
	r := &NamespaceReconciler{Client: newClient(nsLoc("team-a", "x", true), nsLoc("team-b", "y", true), nsLoc("team-b", "z", true))}
	get := func(ns, name string) storagev1alpha1.SwiftStorageLocation {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
			t.Fatal(err)
		}
		var got storagev1alpha1.SwiftStorageLocation
		if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	x := get("team-a", "x")
	if c := cond(x.Status.Conditions, storagev1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("team-a's only default: Ready = %+v", c)
	}
	if cond(x.Status.Conditions, storagev1alpha1.ConditionReachable) != nil {
		t.Error("a namespace location must not be probed")
	}
	if c := cond(get("team-b", "y").Status.Conditions, storagev1alpha1.ConditionReady); c == nil || c.Reason != ReasonAmbiguousDefault || !strings.Contains(c.Message, "z") {
		t.Errorf("team-b with two defaults: Ready = %+v", c)
	}
}

// ProbeRegistry against real servers: a registry behind a private CA answers
// only when the location carries that CA; a 401 (credentials wanted) counts as
// answered; something that is not a registry does not.
func TestProbeRegistry(t *testing.T) {
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer registry.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: registry.Certificate().Raw}))
	host := strings.TrimPrefix(registry.URL, "https://")

	if ok, msg := ProbeRegistry(context.Background(), &storagev1alpha1.OCILocation{Repository: host + "/kubeswift", CABundle: ca}); !ok || !strings.Contains(msg, "401") {
		t.Errorf("with its CA: ok=%v msg=%q", ok, msg)
	}
	if ok, msg := ProbeRegistry(context.Background(), &storagev1alpha1.OCILocation{Repository: host + "/kubeswift"}); ok || !strings.Contains(msg, "certificate") {
		t.Errorf("without its CA: ok=%v msg=%q, want a certificate error", ok, msg)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer plain.Close()
	if ok, _ := ProbeRegistry(context.Background(), &storagev1alpha1.OCILocation{Repository: strings.TrimPrefix(plain.URL, "http://") + "/k", Insecure: true}); !ok {
		t.Error("insecure: a plaintext registry should answer")
	}

	notRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer notRegistry.Close()
	if ok, msg := ProbeRegistry(context.Background(), &storagev1alpha1.OCILocation{Repository: strings.TrimPrefix(notRegistry.URL, "http://") + "/k", Insecure: true}); ok || !strings.Contains(msg, "404") {
		t.Errorf("not a registry: ok=%v msg=%q", ok, msg)
	}
}

// A reconcile that skips the probe still writes Reachable, from the last
// probe's result. Taken from the cached object instead, it was lost whenever
// that copy predated the write that added it: the merge patch then rewrote the
// condition list without it, and nothing put it back until the next probe,
// up to 10 minutes later (seen on a cluster).
func TestClusterLocation_SkippedProbeKeepsReachable(t *testing.T) {
	probes := 0
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	c := newClient(clusterLoc("main", false, "registry.example.com/kubeswift"))
	r := &ClusterReconciler{Client: c, Now: func() time.Time { return now },
		Probe: func(context.Context, *storagev1alpha1.OCILocation) (bool, string) {
			probes++
			return false, "x509: certificate signed by unknown authority"
		}}
	reconcileCluster(t, r, "main")

	// What the stale write left: the conditions without Reachable.
	var loc storagev1alpha1.SwiftClusterStorageLocation
	if err := c.Get(context.Background(), types.NamespacedName{Name: "main"}, &loc); err != nil {
		t.Fatal(err)
	}
	meta.RemoveStatusCondition(&loc.Status.Conditions, storagev1alpha1.ConditionReachable)
	if err := c.Status().Update(context.Background(), &loc); err != nil {
		t.Fatal(err)
	}

	now = now.Add(30 * time.Second)
	got := reconcileCluster(t, r, "main")
	c2 := cond(got.Status.Conditions, storagev1alpha1.ConditionReachable)
	if c2 == nil || c2.Status != metav1.ConditionFalse || !strings.Contains(c2.Message, "x509") {
		t.Fatalf("Reachable = %+v, want the last probe's result without probing again", c2)
	}
	if probes != 1 {
		t.Errorf("probes = %d, want 1", probes)
	}
}
