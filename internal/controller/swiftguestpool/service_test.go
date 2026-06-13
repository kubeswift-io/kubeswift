package swiftguestpool

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/scheme"
)

func poolWithService(svc *swiftv1alpha1.PoolServiceSpec, binding string) *swiftv1alpha1.SwiftGuestPool {
	p := &swiftv1alpha1.SwiftGuestPool{
		ObjectMeta: metav1.ObjectMeta{Name: "infer", Namespace: "default", UID: "pool-uid"},
		Spec:       swiftv1alpha1.SwiftGuestPoolSpec{Replicas: 3, Service: svc},
	}
	if binding != "" {
		p.Spec.Template.Spec.Network = &swiftv1alpha1.GuestNetworkSpec{Binding: binding}
	}
	return p
}

func TestPoolReplicaPorts_ClearsExpose(t *testing.T) {
	in := []swiftv1alpha1.GuestPort{
		{Name: "http", Port: 8080, TargetPort: 80, Protocol: corev1.ProtocolTCP, Expose: "ClusterIP"},
	}
	out := poolReplicaPorts(in)
	if len(out) != 1 || out[0].Expose != "" {
		t.Fatalf("expected Expose cleared, got %+v", out)
	}
	if out[0].Port != 8080 || out[0].TargetPort != 80 || out[0].Name != "http" {
		t.Fatalf("other fields must be preserved, got %+v", out[0])
	}
	// must be a copy — mutating the result must not touch the input
	if in[0].Expose != "ClusterIP" {
		t.Fatalf("input mutated: %+v", in[0])
	}
}

func TestPoolServicePorts_TargetIsPodPort(t *testing.T) {
	out := poolServicePorts([]swiftv1alpha1.GuestPort{
		{Name: "http", Port: 8080, TargetPort: 80}, // proto empty -> TCP default
	})
	if len(out) != 1 {
		t.Fatalf("want 1 port, got %d", len(out))
	}
	if out[0].Protocol != corev1.ProtocolTCP {
		t.Fatalf("want TCP default, got %s", out[0].Protocol)
	}
	// The Service targetPort is the POD port (8080), not the in-guest port (80) —
	// the in-pod DNAT forwards pod:8080 -> vm:80.
	if out[0].TargetPort != intstr.FromInt32(8080) {
		t.Fatalf("want targetPort=8080 (pod side), got %v", out[0].TargetPort)
	}
}

func TestPoolPrimaryBinding(t *testing.T) {
	if got := poolPrimaryBinding(poolWithService(&swiftv1alpha1.PoolServiceSpec{}, "")); got != "nat" {
		t.Fatalf("nil network must default nat, got %s", got)
	}
	if got := poolPrimaryBinding(poolWithService(&swiftv1alpha1.PoolServiceSpec{}, "bridge")); got != "bridge" {
		t.Fatalf("bridge template, got %s", got)
	}
}

func TestReconcilePoolService_MintThenGC(t *testing.T) {
	ctx := context.Background()
	pool := poolWithService(&swiftv1alpha1.PoolServiceSpec{
		Ports: []swiftv1alpha1.GuestPort{{Port: 8080}},
		Type:  "ClusterIP",
	}, "")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool).Build()
	r := &SwiftGuestPoolReconciler{Client: c}

	res, err := r.reconcilePoolService(ctx, pool, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.name != "infer-svc" || !res.ready {
		t.Fatalf("want minted+ready, got %+v", res)
	}
	var svc corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "infer-svc"}, &svc); err != nil {
		t.Fatalf("service should exist: %v", err)
	}
	if svc.Spec.Selector[swiftv1alpha1.LabelPoolName] != "infer" {
		t.Fatalf("selector must be the pool label, got %v", svc.Spec.Selector)
	}
	if !metav1.IsControlledBy(&svc, pool) {
		t.Fatalf("service must be owned by the pool")
	}

	// Removing spec.service GCs the Service.
	pool.Spec.Service = nil
	res, err = r.reconcilePoolService(ctx, pool, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.name != "" {
		t.Fatalf("want no service after removal, got %q", res.name)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "infer-svc"}, &svc); err == nil {
		t.Fatalf("service should be GC'd")
	}
}

func TestReconcilePoolService_NoReadyReplicas(t *testing.T) {
	ctx := context.Background()
	pool := poolWithService(&swiftv1alpha1.PoolServiceSpec{Ports: []swiftv1alpha1.GuestPort{{Port: 22}}}, "")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool).Build()
	r := &SwiftGuestPoolReconciler{Client: c}

	res, err := r.reconcilePoolService(ctx, pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.name != "infer-svc" || res.ready {
		t.Fatalf("service minted but not ready with 0 replicas, got %+v", res)
	}
}

func TestReconcilePoolService_BridgeUnsupported(t *testing.T) {
	ctx := context.Background()
	pool := poolWithService(&swiftv1alpha1.PoolServiceSpec{Ports: []swiftv1alpha1.GuestPort{{Port: 8080}}}, "bridge")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool).Build()
	r := &SwiftGuestPoolReconciler{Client: c}

	res, err := r.reconcilePoolService(ctx, pool, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.name != "" || res.ready || res.reason != "BridgeBindingUnsupported" {
		t.Fatalf("bridge template must mint no Service and explain why, got %+v", res)
	}
	var svc corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "infer-svc"}, &svc); err == nil {
		t.Fatalf("no Service should exist for a bridge template")
	}
}

func TestReconcilePoolService_AnnotationsAndLBClass(t *testing.T) {
	ctx := context.Background()
	lbClass := "tailscale"
	pool := poolWithService(&swiftv1alpha1.PoolServiceSpec{
		Ports:             []swiftv1alpha1.GuestPort{{Port: 8080}},
		Type:              "LoadBalancer",
		Annotations:       map[string]string{"tailscale.com/hostname": "infer", "metallb.universe.tf/address-pool": "prod"},
		LoadBalancerClass: &lbClass,
	}, "")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool).Build()
	r := &SwiftGuestPoolReconciler{Client: c}

	if _, err := r.reconcilePoolService(ctx, pool, 1); err != nil {
		t.Fatal(err)
	}
	var svc corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "infer-svc"}, &svc); err != nil {
		t.Fatal(err)
	}
	if svc.Annotations["tailscale.com/hostname"] != "infer" || svc.Annotations["metallb.universe.tf/address-pool"] != "prod" {
		t.Fatalf("operator annotations must land on the Service, got %v", svc.Annotations)
	}
	if svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass != "tailscale" {
		t.Fatalf("loadBalancerClass must be set for a LoadBalancer Service, got %v", svc.Spec.LoadBalancerClass)
	}

	// A foreign annotation written by another controller must survive an update.
	svc.Annotations["metallb.universe.tf/ip-allocated-from-pool"] = "prod"
	if err := c.Update(ctx, &svc); err != nil {
		t.Fatal(err)
	}
	// Operator changes one of their annotations -> overlay, foreign preserved.
	pool.Spec.Service.Annotations["tailscale.com/hostname"] = "infer2"
	if _, err := r.reconcilePoolService(ctx, pool, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "infer-svc"}, &svc); err != nil {
		t.Fatal(err)
	}
	if svc.Annotations["tailscale.com/hostname"] != "infer2" {
		t.Fatalf("operator annotation change must apply, got %q", svc.Annotations["tailscale.com/hostname"])
	}
	if svc.Annotations["metallb.universe.tf/ip-allocated-from-pool"] != "prod" {
		t.Fatalf("foreign annotation must be preserved across update, got %v", svc.Annotations)
	}
}

func TestReconcilePoolService_Headless(t *testing.T) {
	ctx := context.Background()
	pool := poolWithService(&swiftv1alpha1.PoolServiceSpec{
		Ports:    []swiftv1alpha1.GuestPort{{Port: 8080}},
		Headless: true,
	}, "")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool).Build()
	r := &SwiftGuestPoolReconciler{Client: c}

	if _, err := r.reconcilePoolService(ctx, pool, 1); err != nil {
		t.Fatal(err)
	}
	var svc corev1.Service
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "infer-svc"}, &svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatalf("headless Service must have clusterIP=None, got %q", svc.Spec.ClusterIP)
	}
}
