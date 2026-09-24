package swiftmigration

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/migrationcert"
)

func copiedNodeIdentity(ns, node string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migrationcert.MigrationNodeSecretName(node),
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/component":  "migration-mtls",
				"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.key": []byte("key-" + node)},
	}
}

func terminalLiveMig(name, ns, dstNode string) *migrationv1alpha1.SwiftMigration {
	m := newMigration(name, ns)
	m.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	m.Status.Mode = migrationv1alpha1.SwiftMigrationModeLive
	m.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseCompleted
	m.Status.DestinationNode = dstNode
	return m
}

func cleanupReconciler(t *testing.T, objs ...client.Object) *SwiftMigrationReconciler {
	t.Helper()
	scheme := validatingScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &SwiftMigrationReconciler{Client: c, Scheme: scheme, MigrationMTLSEnabled: true, SystemNamespace: "kubeswift-system"}
}

// A terminal migration's copied destination-node identity is reclaimed when no
// other active migration in the namespace needs it.
func TestCleanupCopiedNodeIdentities_DeletesWhenUnused(t *testing.T) {
	mig := terminalLiveMig("m", "default", "worker-2")
	sec := copiedNodeIdentity("default", "worker-2")
	r := cleanupReconciler(t, mig, sec)

	if err := r.cleanupCopiedNodeIdentities(context.Background(), mig); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	var got corev1.Secret
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sec), &got); !apierrors.IsNotFound(err) {
		t.Errorf("copied node identity should be deleted, got err=%v", err)
	}
}

// The copy is kept while another active (non-terminal) migration in the
// namespace still references the same node.
func TestCleanupCopiedNodeIdentities_KeptWhileAnotherMigrationActive(t *testing.T) {
	mig := terminalLiveMig("m", "default", "worker-2")
	other := newMigration("other", "default")
	other.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	other.Status.Mode = migrationv1alpha1.SwiftMigrationModeLive
	other.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy // active
	other.Status.DestinationNode = "worker-2"
	sec := copiedNodeIdentity("default", "worker-2")
	r := cleanupReconciler(t, mig, other, sec)

	if err := r.cleanupCopiedNodeIdentities(context.Background(), mig); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	var got corev1.Secret
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sec), &got); err != nil {
		t.Errorf("copied node identity should be kept while another migration uses the node: %v", err)
	}
}

// A Secret without the migration-mtls labels is never touched.
func TestCleanupCopiedNodeIdentities_IgnoresUnlabeledSecret(t *testing.T) {
	mig := terminalLiveMig("m", "default", "worker-2")
	sec := copiedNodeIdentity("default", "worker-2")
	sec.Labels = map[string]string{"app.kubernetes.io/component": "something-else"}
	r := cleanupReconciler(t, mig, sec)

	if err := r.cleanupCopiedNodeIdentities(context.Background(), mig); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	var got corev1.Secret
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sec), &got); err != nil {
		t.Errorf("an unlabeled Secret must not be deleted: %v", err)
	}
}
