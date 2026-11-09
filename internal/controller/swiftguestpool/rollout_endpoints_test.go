package swiftguestpool

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// syncLauncherPods gives every replica the launcher pod the SwiftGuest
// controller would, Ready when ready(guest) says its port probe passes. A pod
// left by a deleted or replaced replica goes, as garbage collection would.
func syncLauncherPods(t *testing.T, c client.Client, ready func(*swiftv1alpha1.SwiftGuest) bool) {
	t.Helper()
	ctx := context.Background()
	var guests swiftv1alpha1.SwiftGuestList
	if err := c.List(ctx, &guests); err != nil {
		t.Fatal(err)
	}
	byName := map[string]*swiftv1alpha1.SwiftGuest{}
	for i := range guests.Items {
		byName[guests.Items[i].Name] = &guests.Items[i]
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		g := byName[p.Name]
		if g == nil || p.Annotations[swiftv1alpha1.AnnotationTemplateHash] != g.Annotations[swiftv1alpha1.AnnotationTemplateHash] {
			if err := c.Delete(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, g := range byName {
		if g.DeletionTimestamp != nil {
			continue
		}
		status := corev1.ConditionFalse
		if guestReady(g) && ready(g) {
			status = corev1.ConditionTrue
		}
		var pod corev1.Pod
		err := c.Get(ctx, client.ObjectKey{Namespace: g.Namespace, Name: g.Name}, &pod)
		if apierrors.IsNotFound(err) {
			pod = corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: g.Name, Namespace: g.Namespace,
				Labels:      map[string]string{swiftv1alpha1.LabelPoolName: "p"},
				Annotations: map[string]string{swiftv1alpha1.AnnotationTemplateHash: g.Annotations[swiftv1alpha1.AnnotationTemplateHash]},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(g,
					schema.GroupVersionKind{Group: swiftv1alpha1.GroupName, Version: swiftv1alpha1.Version, Kind: "SwiftGuest"})},
			}}
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
			if err := c.Create(ctx, &pod); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
		if err := c.Status().Update(ctx, &pod); err != nil {
			t.Fatal(err)
		}
	}
}

// endpointsReady counts the replicas a client of the pool Service reaches:
// launcher pod Ready.
func endpointsReady(t *testing.T, c client.Client) int {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	n := 0
	for i := range pods.Items {
		if podReady(&pods.Items[i]) {
			n++
		}
	}
	return n
}

// With spec.service set, GuestRunning turns true when the VM starts, while the
// guest OS is still booting and the launcher's port probe still fails. A
// rollout with maxUnavailable 0 counted the booting replacements as available
// and retired every old replica, leaving the pool Service with no ready
// endpoint until the new guests had booted.
func TestRollingUpdate_ServicePoolWaitsForReadyEndpoints(t *testing.T) {
	clock := time.Now()
	pool := failingPool()
	pool.Spec.Replicas = 2
	pool.Spec.Service = &swiftv1alpha1.PoolServiceSpec{Ports: []swiftv1alpha1.GuestPort{{Name: "http", Port: 80}}}
	pool.Spec.UpdateStrategy = &swiftv1alpha1.UpdateStrategy{
		Type:          swiftv1alpha1.UpdateStrategyRollingUpdate,
		RollingUpdate: &swiftv1alpha1.RollingUpdateConfig{MaxUnavailable: 0, MaxSurge: 2},
	}
	c := stampedPoolClient(&clock, pool)
	r := &SwiftGuestPoolReconciler{Client: c, now: func() time.Time { return clock }}
	reconcilePool(t, r)
	markAllServing(t, c)
	booted := func(*swiftv1alpha1.SwiftGuest) bool { return true }
	syncLauncherPods(t, c, booted)
	oldHash := replicaHashes(t, c)["p-0"]

	newHash := changeKernel(t, c, "k2")
	// The new VMs come up but their guests are still booting: no port answers.
	stillBooting := func(g *swiftv1alpha1.SwiftGuest) bool {
		return g.Annotations[swiftv1alpha1.AnnotationTemplateHash] != newHash
	}
	for i := 0; i < 10; i++ {
		reconcilePool(t, r)
		markAllServing(t, c)
		syncLauncherPods(t, c, stillBooting)
		if n := endpointsReady(t, c); n < 2 {
			t.Fatalf("pass %d: %d ready endpoint(s); maxUnavailable 0 must keep both (%v)", i, n, replicaHashes(t, c))
		}
	}
	h := replicaHashes(t, c)
	if h["p-0"] != oldHash || h["p-1"] != oldHash {
		t.Fatalf("old replicas retired while their replacements were still booting: %v", h)
	}

	// The guests finish booting: the rollout completes and drops its surge.
	for i := 0; i < 30 && !allOn(t, c, newHash, 2); i++ {
		reconcilePool(t, r)
		markAllServing(t, c)
		syncLauncherPods(t, c, booted)
		if n := endpointsReady(t, c); n < 2 {
			t.Fatalf("pass %d: %d ready endpoint(s); maxUnavailable 0 must keep both (%v)", i, n, replicaHashes(t, c))
		}
	}
	if !allOn(t, c, newHash, 2) {
		t.Fatalf("rollout did not complete once the guests booted: %v", replicaHashes(t, c))
	}
}
