package swiftguestpool

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

var isController = true

func ctrlRequest() ctrl.Request { return ctrl.Request{NamespacedName: testPoolKey} }

func poolWithDataDisk() *swiftv1alpha1.SwiftGuestPool {
	pool := failingPool()
	pool.Spec.VolumeClaimTemplates = []swiftv1alpha1.PersistentVolumeClaimTemplate{{
		Metadata: swiftv1alpha1.PoolObjectMeta{Name: "data"},
	}}
	return pool
}

func dataPVC(t *testing.T, c client.Client) *corev1.PersistentVolumeClaim {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "data-p-0"}, &pvc); err != nil {
		t.Fatalf("data-p-0: %v", err)
	}
	return &pvc
}

// A replica's data disk outlives the pool, as the pool guide says: owned by
// the pool, it was garbage-collected along with it.
func TestEnsurePVC_ReplicaDataIsNotOwnedByThePool(t *testing.T) {
	clock := time.Now()
	c := stampedPoolClient(&clock, poolWithDataDisk())
	r := &SwiftGuestPoolReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
	reconcilePool(t, r)

	pvc := dataPVC(t, c)
	if len(pvc.OwnerReferences) != 0 {
		t.Errorf("owner references = %+v; deleting the pool would delete the replica's data", pvc.OwnerReferences)
	}
	if pvc.Labels[swiftv1alpha1.LabelPoolName] != "p" {
		t.Errorf("labels = %v, want %s=p", pvc.Labels, swiftv1alpha1.LabelPoolName)
	}
}

// A PVC from before the change is adopted and released from the pool.
func TestEnsurePVC_ReleasesALegacyPoolOwnedPVC(t *testing.T) {
	pool := poolWithDataDisk()
	legacy := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-p-0", Namespace: "ns",
		Labels: map[string]string{swiftv1alpha1.LabelPoolName: "p"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "swift.kubeswift.io/v1alpha1", Kind: "SwiftGuestPool",
			Name: "p", UID: pool.UID, Controller: &isController,
		}},
	}}
	clock := time.Now()
	c := stampedPoolClient(&clock, pool, legacy)
	r := &SwiftGuestPoolReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
	reconcilePool(t, r)

	if refs := dataPVC(t, c).OwnerReferences; len(refs) != 0 {
		t.Errorf("owner references = %+v, want the pool's removed", refs)
	}
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), testReplicaKey, &g); err != nil {
		t.Fatalf("replica not created on its existing data disk: %v", err)
	}
}

// Another pool's PVC with a colliding name is never attached: pool "x" with
// template "data-p" and pool "p" with template "data" both name "data-p-0".
func TestEnsurePVC_RefusesAPVCLabelledForAnotherPool(t *testing.T) {
	other := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-p-0", Namespace: "ns",
		Labels: map[string]string{swiftv1alpha1.LabelPoolName: "x"},
	}}
	clock := time.Now()
	c := stampedPoolClient(&clock, poolWithDataDisk(), other)
	r := &SwiftGuestPoolReconciler{Client: c, Recorder: record.NewFakeRecorder(20)}
	_, err := r.Reconcile(context.Background(), ctrlRequest())
	if err == nil || !strings.Contains(err.Error(), "refusing to attach") {
		t.Fatalf("reconcile err = %v, want a refusal to attach pool x's PVC", err)
	}
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), testReplicaKey, &g); err == nil {
		t.Error("replica created on another pool's data disk")
	}
}
