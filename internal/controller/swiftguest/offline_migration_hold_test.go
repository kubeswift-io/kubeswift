package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// offlineMigration is an offline SwiftMigration of the test guest to node-b.
func offlineMigration(phase migrationv1alpha1.SwiftMigrationPhase) *migrationv1alpha1.SwiftMigration {
	return &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
		Spec: migrationv1alpha1.SwiftMigrationSpec{
			GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: testGuestName},
			Target:   migrationv1alpha1.SwiftMigrationTarget{NodeName: "node-b"},
		},
		Status: migrationv1alpha1.SwiftMigrationStatus{Phase: phase, Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
}

// claimedGuest is the test guest as an offline migration leaves it after its
// claim, with runPolicy Running written back over the claim's Stopped by
// another writer (a GitOps controller's drift correction).
func claimedGuest() *swiftv1alpha1.SwiftGuest {
	g := kernelGuest()
	g.Annotations = map[string]string{migrationv1alpha1.AnnotationMigrationInProgress: "m"}
	g.Spec.RunPolicy = swiftv1alpha1.RunPolicyRunning
	return g
}

// A runPolicy Running re-applied while an offline migration holds the guest
// must not bring the launcher back on the source before the cutover: it
// fought the migration over the pod and failed it with the guest pinned to the
// target while running on the source (#899).
func TestNoLauncherWhileAnOfflineMigrationHoldsTheGuest(t *testing.T) {
	for _, phase := range []migrationv1alpha1.SwiftMigrationPhase{
		migrationv1alpha1.SwiftMigrationPhasePreparing,
		migrationv1alpha1.SwiftMigrationPhaseStopAndCopy,
	} {
		t.Run(string(phase), func(t *testing.T) {
			g := claimedGuest()
			c := guestClientBuilder(g, offlineMigration(phase), readyKernel(), testGuestClass()).Build()
			r := &SwiftGuestReconciler{Client: c, Scheme: c.Scheme(), APIReader: c}

			_, res, err := reconcileGuest(t, r)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if pods := launcherPods(t, c); len(pods) != 0 {
				t.Fatalf("started a launcher under an offline migration: %s on %q", pods[0].Name, pods[0].Spec.NodeName)
			}
			if res.RequeueAfter == 0 {
				t.Error("a held guest must be looked at again")
			}
		})
	}
}

// The marker is read uncached: a reconcile woken by the source launcher's
// deletion may read the guest from before the claim.
func TestNoLauncherFromAGuestCachedBeforeTheClaim(t *testing.T) {
	cached := kernelGuest()
	mig := offlineMigration(migrationv1alpha1.SwiftMigrationPhasePreparing)
	c := guestClientBuilder(cached, readyKernel(), testGuestClass()).Build()
	api := guestClientBuilder(claimedGuest(), mig, readyKernel(), testGuestClass()).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: c.Scheme(), APIReader: api}

	if _, _, err := reconcileGuest(t, r); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pods := launcherPods(t, c); len(pods) != 0 {
		t.Fatalf("started a launcher from a guest cached before the claim: %s", pods[0].Name)
	}
}

// The hold ends at the cutover, when the migration itself sets runPolicy
// Running and the target node, and with the migration: a failed or finished
// one, or a marker whose migration is gone, holds nothing.
func TestLauncherStartsOnceTheOfflineMigrationLetsGo(t *testing.T) {
	cases := map[string]struct {
		mig      *migrationv1alpha1.SwiftMigration
		nodeName string
	}{
		"cut over":          {mig: offlineMigration(migrationv1alpha1.SwiftMigrationPhaseStopAndCopy), nodeName: "node-b"},
		"migration failed":  {mig: offlineMigration(migrationv1alpha1.SwiftMigrationPhaseFailed)},
		"migration gone":    {},
		"migration done":    {mig: offlineMigration(migrationv1alpha1.SwiftMigrationPhaseCompleted), nodeName: "node-b"},
		"migration aborted": {mig: offlineMigration(migrationv1alpha1.SwiftMigrationPhaseCancelled)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := claimedGuest()
			g.Spec.NodeName = tc.nodeName
			objs := []client.Object{g, readyKernel(), testGuestClass()}
			if tc.nodeName != "" {
				objs = append(objs, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: tc.nodeName}})
			}
			if tc.mig != nil {
				objs = append(objs, tc.mig)
			}
			c := guestClientBuilder(objs...).Build()
			r := &SwiftGuestReconciler{Client: c, Scheme: c.Scheme(), APIReader: c}

			if _, _, err := reconcileGuest(t, r); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			pods := launcherPods(t, c)
			if len(pods) != 1 {
				t.Fatalf("launcher not started: %v", pods)
			}
			if pods[0].Spec.NodeName != tc.nodeName {
				t.Errorf("launcher on %q, want %q", pods[0].Spec.NodeName, tc.nodeName)
			}
		})
	}
}
