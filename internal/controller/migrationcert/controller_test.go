package migrationcert

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

const testSystemNS = "kubeswift-system"

// certScheme registers the cert-manager Certificate GVK as unstructured so
// the fake client can track it. Mirrors how the cached client treats
// cert-manager types in production (we never import the cert-manager Go
// module — see certificate.go).
func certScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: certManagerGroup, Version: certManagerVersion, Kind: certificateKind},
		&unstructured.Unstructured{},
	)
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: certManagerGroup, Version: certManagerVersion, Kind: certificateKind + "List"},
		&unstructured.UnstructuredList{},
	)
	return s
}

func getCert(ctx context.Context, c client.Client, ns, name string) (*unstructured.Unstructured, error) {
	u := newCertShell()
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, u)
	return u, err
}

func TestMigrationNodeNames(t *testing.T) {
	if got := MigrationNodeCertName("miles"); got != "kubeswift-migration-node-miles" {
		t.Errorf("MigrationNodeCertName = %q", got)
	}
	if got := MigrationNodeSecretName("miles"); got != "kubeswift-migration-node-miles" {
		t.Errorf("MigrationNodeSecretName = %q", got)
	}
	// SAN/checkHost pin value MUST be exactly the node name (Option B).
	if got := MigrationNodeCertSAN("miles"); got != "miles" {
		t.Errorf("MigrationNodeCertSAN = %q, want node name verbatim", got)
	}
}

// TestNewNodeCertificate_Fields pins the cert-manager Certificate shape:
// SAN=nodeName (the Option B identity), both server+client usages (a node
// is dst-server in one migration and src-client in another), and the CA
// Issuer reference. A regression here silently breaks SAN pinning.
func TestNewNodeCertificate_Fields(t *testing.T) {
	u := newNodeCertificate(testSystemNS, "boba")

	if u.GetNamespace() != testSystemNS {
		t.Errorf("namespace = %q, want %q", u.GetNamespace(), testSystemNS)
	}
	if u.GetName() != "kubeswift-migration-node-boba" {
		t.Errorf("name = %q", u.GetName())
	}
	if u.GetAPIVersion() != "cert-manager.io/v1" || u.GetKind() != "Certificate" {
		t.Errorf("GVK = %s/%s", u.GetAPIVersion(), u.GetKind())
	}

	secretName, _, _ := unstructured.NestedString(u.Object, "spec", "secretName")
	if secretName != "kubeswift-migration-node-boba" {
		t.Errorf("spec.secretName = %q", secretName)
	}
	cn, _, _ := unstructured.NestedString(u.Object, "spec", "commonName")
	if cn != "boba" {
		t.Errorf("spec.commonName = %q, want boba", cn)
	}
	dnsNames, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "dnsNames")
	if len(dnsNames) != 1 || dnsNames[0] != "boba" {
		t.Errorf("spec.dnsNames = %v, want [boba]", dnsNames)
	}
	usages, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "usages")
	if len(usages) != 2 || usages[0] != "server auth" || usages[1] != "client auth" {
		t.Errorf("spec.usages = %v, want [server auth, client auth]", usages)
	}
	issuerName, _, _ := unstructured.NestedString(u.Object, "spec", "issuerRef", "name")
	if issuerName != MigrationCAIssuerName {
		t.Errorf("spec.issuerRef.name = %q, want %q", issuerName, MigrationCAIssuerName)
	}
	issuerKind, _, _ := unstructured.NestedString(u.Object, "spec", "issuerRef", "kind")
	if issuerKind != "Issuer" {
		t.Errorf("spec.issuerRef.kind = %q, want Issuer (namespaced, not ClusterIssuer)", issuerKind)
	}
}

func TestIsWorkerNode(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"plain worker", nil, true},
		{"labeled worker", map[string]string{"kubeswift.io/some": "x"}, true},
		{"control-plane", map[string]string{controlPlaneRoleLabel: ""}, false},
		{"master", map[string]string{masterRoleLabel: ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: tc.labels}}
			if got := isWorkerNode(node); got != tc.want {
				t.Errorf("isWorkerNode = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWorkerNodePredicate_Update pins that the predicate ignores
// heartbeat/status churn but fires when worker<->control-plane membership
// flips. Without the membership filter the reconciler would re-run on
// every node status heartbeat.
func TestWorkerNodePredicate_Update(t *testing.T) {
	r := &MigrationCertReconciler{}
	p := r.workerNodePredicate()

	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}
	workerHeartbeat := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", ResourceVersion: "2"}}
	cp := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: map[string]string{controlPlaneRoleLabel: ""}}}

	if p.Update(event.UpdateEvent{ObjectOld: worker, ObjectNew: workerHeartbeat}) {
		t.Error("predicate fired on no-op heartbeat update")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: worker, ObjectNew: cp}) {
		t.Error("predicate did not fire on worker->control-plane relabel")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: cp, ObjectNew: worker}) {
		t.Error("predicate did not fire on control-plane->worker relabel")
	}
	if !p.Create(event.CreateEvent{Object: cp}) {
		t.Error("Create predicate must always enqueue")
	}
	if !p.Delete(event.DeleteEvent{Object: worker}) {
		t.Error("Delete predicate must always enqueue")
	}
}

func TestEnsureNodeCertificate_CreatesWhenAbsent(t *testing.T) {
	scheme := certScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	if err := ensureNodeCertificate(ctx, c, testSystemNS, "miles"); err != nil {
		t.Fatalf("ensureNodeCertificate: %v", err)
	}
	if _, err := getCert(ctx, c, testSystemNS, "kubeswift-migration-node-miles"); err != nil {
		t.Fatalf("expected certificate to exist: %v", err)
	}
}

func TestEnsureNodeCertificate_IdempotentWhenPresent(t *testing.T) {
	scheme := certScheme(t)
	pre := newNodeCertificate(testSystemNS, "miles")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pre).Build()
	ctx := context.Background()

	if err := ensureNodeCertificate(ctx, c, testSystemNS, "miles"); err != nil {
		t.Fatalf("ensureNodeCertificate (idempotent): %v", err)
	}
	if _, err := getCert(ctx, c, testSystemNS, "kubeswift-migration-node-miles"); err != nil {
		t.Fatalf("certificate should still exist: %v", err)
	}
}

func TestDeleteNodeCertificate(t *testing.T) {
	scheme := certScheme(t)
	pre := newNodeCertificate(testSystemNS, "miles")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pre).Build()
	ctx := context.Background()

	if err := deleteNodeCertificate(ctx, c, testSystemNS, "miles"); err != nil {
		t.Fatalf("deleteNodeCertificate: %v", err)
	}
	if _, err := getCert(ctx, c, testSystemNS, "kubeswift-migration-node-miles"); !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after delete, got %v", err)
	}
	// Idempotent: deleting again tolerates NotFound.
	if err := deleteNodeCertificate(ctx, c, testSystemNS, "miles"); err != nil {
		t.Fatalf("deleteNodeCertificate (NotFound) must be tolerated: %v", err)
	}
}

func TestReconcile_WorkerNode_CreatesCert(t *testing.T) {
	scheme := certScheme(t)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "miles"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := &MigrationCertReconciler{Client: c, SystemNamespace: testSystemNS}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "miles"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := getCert(ctx, c, testSystemNS, "kubeswift-migration-node-miles"); err != nil {
		t.Fatalf("worker node should get a certificate: %v", err)
	}
}

func TestReconcile_ControlPlaneNode_DeletesStaleCert(t *testing.T) {
	scheme := certScheme(t)
	// A node that was a worker (has a cert) and is now control-plane.
	staleCert := newNodeCertificate(testSystemNS, "frida")
	cpNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "frida",
		Labels: map[string]string{controlPlaneRoleLabel: ""},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cpNode, staleCert).Build()
	r := &MigrationCertReconciler{Client: c, SystemNamespace: testSystemNS}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "frida"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := getCert(ctx, c, testSystemNS, "kubeswift-migration-node-frida"); !apierrors.IsNotFound(err) {
		t.Fatalf("control-plane node's stale cert should be deleted, got %v", err)
	}
}

func TestReconcile_NodeGone_DeletesCert(t *testing.T) {
	scheme := certScheme(t)
	// Node object absent; its cert lingers and must be GC'd.
	orphanCert := newNodeCertificate(testSystemNS, "gone")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(orphanCert).Build()
	r := &MigrationCertReconciler{Client: c, SystemNamespace: testSystemNS}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gone"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := getCert(ctx, c, testSystemNS, "kubeswift-migration-node-gone"); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted node's cert should be GC'd, got %v", err)
	}
}
