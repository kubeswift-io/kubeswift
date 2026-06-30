package swiftguest

import (
	"context"
	"testing"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func pdbTestGuest() *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default", UID: "g-uid"},
	}
}

func TestEnsureMigrationPDB_Creates(t *testing.T) {
	g := pdbTestGuest()
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(g).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	if err := r.ensureMigrationPDB(context.Background(), g); err != nil {
		t.Fatalf("ensureMigrationPDB: %v", err)
	}

	var pdb policyv1.PodDisruptionBudget
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "g"}, &pdb); err != nil {
		t.Fatalf("PDB not created: %v", err)
	}
	if pdb.Spec.MaxUnavailable == nil || *pdb.Spec.MaxUnavailable != intstr.FromInt32(0) {
		t.Errorf("maxUnavailable must be 0 (the hard floor); got %v", pdb.Spec.MaxUnavailable)
	}
	if pdb.Spec.MinAvailable != nil {
		t.Errorf("minAvailable must be unset (maxUnavailable:0 is the model); got %v", pdb.Spec.MinAvailable)
	}
	if pdb.Spec.Selector == nil || pdb.Spec.Selector.MatchLabels[guestPodLabelKey] != "g" {
		t.Errorf("selector must match launcher pod label %s=g; got %+v", guestPodLabelKey, pdb.Spec.Selector)
	}
	if len(pdb.OwnerReferences) != 1 ||
		pdb.OwnerReferences[0].Name != "g" ||
		pdb.OwnerReferences[0].Controller == nil || !*pdb.OwnerReferences[0].Controller {
		t.Errorf("PDB must be controller-owned by the guest (GC on delete); got %+v", pdb.OwnerReferences)
	}
}

func TestEnsureMigrationPDB_Idempotent(t *testing.T) {
	g := pdbTestGuest()
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(g).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	ctx := context.Background()

	if err := r.ensureMigrationPDB(ctx, g); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := r.ensureMigrationPDB(ctx, g); err != nil {
		t.Fatalf("second ensure must be a no-op, not an error: %v", err)
	}

	var list policyv1.PodDisruptionBudgetList
	if err := c.List(ctx, &list); err != nil {
		t.Fatalf("list PDBs: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected exactly 1 PDB after two ensures; got %d", len(list.Items))
	}
}
