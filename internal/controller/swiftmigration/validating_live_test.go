package swiftmigration

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/controller/migrationcert"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftguest"
	"github.com/projectbeskar/kubeswift/internal/migrationsidecar"
)

// withClientStunnelSidecar adds a client-role migration-stunnel sidecar to a
// source pod so it passes the Phase 3c sourcePodMTLSReady fail-fast (the
// SwiftGuest controller injects this on real migration-eligible pods).
func withClientStunnelSidecar(p *corev1.Pod) *corev1.Pod {
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{
		Name: migrationsidecar.ContainerName,
		Env:  []corev1.EnvVar{{Name: migrationsidecar.EnvRole, Value: migrationsidecar.RoleClient}},
	})
	return p
}

// migrationNodeIdentitySecret builds a per-node identity Secret fixture
// (the shape cert-manager writes into the system namespace) for the
// Phase 3c Validating-live precondition tests.
func migrationNodeIdentitySecret(systemNS, node string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migrationcert.MigrationNodeSecretName(node),
			Namespace: systemNS,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": []byte("crt-" + node),
			"tls.key": []byte("key-" + node),
			"ca.crt":  []byte("ca"),
		},
	}
}

// newSourcePodWithLauncherImage builds a source pod with a launcher
// container whose image is set explicitly. Used by image-tag-match
// tests where the container image is load-bearing for the assertion.
func newSourcePodWithLauncherImage(guestName, ns, uid, image string) *corev1.Pod {
	p := newSourcePod(guestName, ns, uid)
	p.Spec.Containers = []corev1.Container{
		{Name: LauncherContainerName, Image: image},
	}
	return p
}

// newSourcePod creates a fake source pod for the given guest, with a
// fixed UID so tests can assert SourcePodUID stamping.
func newSourcePod(guestName, ns, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      guestName,
			Namespace: ns,
			UID:       types.UID(uid),
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guestName,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "boba",
		},
	}
}

func TestValidatingLive_HappyPath_AdvancesToPreparing(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	srcPod := newSourcePod("guest", "default", "src-pod-uid-1")
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * 60 * 1e9} // 5min, satisfies MinLiveTimeout
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleValidatingLive(context.Background(), mig, status)
	if result.Err != nil || result.FailureMsg != "" {
		t.Fatalf("handleValidatingLive failure: err=%v msg=%q", result.Err, result.FailureMsg)
	}
	if !result.Advanced {
		t.Fatal("expected Advanced=true on success")
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhasePreparing {
		t.Errorf("phase: want Preparing, got %q", status.Phase)
	}
	if status.Mode != migrationv1alpha1.SwiftMigrationModeLive {
		t.Errorf("status.Mode: want live, got %q", status.Mode)
	}
	if status.SourcePodUID != "src-pod-uid-1" {
		t.Errorf("SourcePodUID: want src-pod-uid-1, got %q", status.SourcePodUID)
	}
	if status.SourceNode != "boba" {
		t.Errorf("SourceNode: want boba, got %q", status.SourceNode)
	}
	if status.DestinationNode != "miles" {
		t.Errorf("DestinationNode: want miles, got %q", status.DestinationNode)
	}
}

// TestValidatingLive_MTLS_IdentitiesPresent_Advances verifies the
// Phase 3c precondition: with mTLS enabled and both node identity
// Secrets present in the system namespace, Validating-live advances AND
// distributes both Secrets into the guest namespace so the launcher
// pods can mount them.
func TestValidatingLive_MTLS_IdentitiesPresent_Advances(t *testing.T) {
	scheme := validatingScheme(t)
	const sysNS = "kubeswift-system"
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	srcPod := withClientStunnelSidecar(newSourcePod("guest", "default", "src-pod-uid-1")) // src node "boba"
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * 60 * 1e9}
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	srcSecret := migrationNodeIdentitySecret(sysNS, "boba")
	dstSecret := migrationNodeIdentitySecret(sysNS, "miles")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod, srcSecret, dstSecret).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10),
		MigrationMTLSEnabled: true, SystemNamespace: sysNS,
	}

	status := mig.Status.DeepCopy()
	result := r.handleValidatingLive(context.Background(), mig, status)
	if result.Err != nil || result.FailureMsg != "" {
		t.Fatalf("handleValidatingLive failure: err=%v msg=%q reason=%q", result.Err, result.FailureMsg, result.FailureReason)
	}
	if !result.Advanced {
		t.Fatal("expected Advanced=true with both identities present")
	}
	// Both node identity Secrets copied into the guest namespace.
	for _, n := range []string{"boba", "miles"} {
		var s corev1.Secret
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: migrationcert.MigrationNodeSecretName(n)}, &s); err != nil {
			t.Errorf("identity Secret for node %q not distributed into guest namespace: %v", n, err)
		}
	}
}

// TestValidatingLive_MTLS_IdentityMissing_FailsNotReady verifies the
// precondition fails fast (before Preparing) when a participating node's
// identity Secret has not been provisioned.
func TestValidatingLive_MTLS_IdentityMissing_FailsNotReady(t *testing.T) {
	scheme := validatingScheme(t)
	const sysNS = "kubeswift-system"
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	srcPod := withClientStunnelSidecar(newSourcePod("guest", "default", "src-pod-uid-1"))
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * 60 * 1e9}
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	// Only the source-node Secret present; destination-node Secret missing.
	srcSecret := migrationNodeIdentitySecret(sysNS, "boba")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod, srcSecret).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10),
		MigrationMTLSEnabled: true, SystemNamespace: sysNS,
	}

	status := mig.Status.DeepCopy()
	result := r.handleValidatingLive(context.Background(), mig, status)
	if result.FailureReason != migrationv1alpha1.FailureReasonMigrationIdentityNotReady {
		t.Fatalf("want FailureReason=%q, got reason=%q msg=%q advanced=%v err=%v",
			migrationv1alpha1.FailureReasonMigrationIdentityNotReady, result.FailureReason, result.FailureMsg, result.Advanced, result.Err)
	}
	if status.Phase == migrationv1alpha1.SwiftMigrationPhasePreparing {
		t.Errorf("must not advance to Preparing when an identity Secret is missing")
	}
}

// TestValidatingLive_MTLS_SourceNotSidecarReady_Fails verifies the fail-fast
// for a source pod that lacks a client-role stunnel sidecar (predates mTLS
// enablement, or is a post-cutover dst pod). Even with both identities
// present, it must fail with SourceSidecarNotReady rather than advance and
// later retry-then-timeout.
func TestValidatingLive_MTLS_SourceNotSidecarReady_Fails(t *testing.T) {
	scheme := validatingScheme(t)
	const sysNS = "kubeswift-system"
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	srcPod := newSourcePod("guest", "default", "src-pod-uid-1") // NO sidecar
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * 60 * 1e9}
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod,
			migrationNodeIdentitySecret(sysNS, "boba"), migrationNodeIdentitySecret(sysNS, "miles")).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{
		Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10),
		MigrationMTLSEnabled: true, SystemNamespace: sysNS,
	}

	status := mig.Status.DeepCopy()
	result := r.handleValidatingLive(context.Background(), mig, status)
	if result.FailureReason != migrationv1alpha1.FailureReasonSourceSidecarNotReady {
		t.Fatalf("want FailureReason=%q, got reason=%q msg=%q",
			migrationv1alpha1.FailureReasonSourceSidecarNotReady, result.FailureReason, result.FailureMsg)
	}
}

// TestValidatingLive_MTLSDisabled_SkipsPrecondition verifies the default
// (plaintext) path is unchanged: no identity Secrets, mTLS off → advances.
func TestValidatingLive_MTLSDisabled_SkipsPrecondition(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	srcPod := newSourcePod("guest", "default", "src-pod-uid-1")
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * 60 * 1e9}
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod).
		WithStatusSubresource(mig).
		Build()
	// MigrationMTLSEnabled left false; SystemNamespace irrelevant.
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleValidatingLive(context.Background(), mig, status)
	if result.Err != nil || result.FailureMsg != "" {
		t.Fatalf("mTLS-off Validating must not fail on missing identities: err=%v msg=%q", result.Err, result.FailureMsg)
	}
	if !result.Advanced {
		t.Fatal("expected Advanced=true on the plaintext path")
	}
}

func TestValidatingLive_NoSourcePod_FailsWithClearMessage(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	// No source pod added — simulates a guest that's not currently
	// running.
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleValidatingLive(context.Background(), mig, status)
	if !strings.Contains(result.FailureMsg, "has no pod") {
		t.Errorf("FailureMsg: want 'has no pod' message, got %q", result.FailureMsg)
	}
	if result.FailureReason != migrationv1alpha1.FailureReasonOther {
		t.Errorf("FailureReason: want Other, got %q", result.FailureReason)
	}
}

func TestValidatingLive_DefensiveGuard_NotLiveMode(t *testing.T) {
	r := &SwiftMigrationReconciler{}
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	result := r.handleValidatingLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(result.FailureMsg, "without live mode") {
		t.Errorf("guard message: got %q", result.FailureMsg)
	}
}

func TestValidatingLive_GuestNotFound_Fails(t *testing.T) {
	scheme := validatingScheme(t)
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	result := r.handleValidatingLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(result.FailureMsg, "no longer exists") {
		t.Errorf("FailureMsg: want guest-missing message, got %q", result.FailureMsg)
	}
}

func TestValidatingLive_TargetNodeCordoned_Fails(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	cordonedNode := newSpaciousNode("miles", 8, 65536)
	cordonedNode.Spec.Unschedulable = true
	srcPod := newSourcePod("guest", "default", "uid")
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, cordonedNode, srcPod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	result := r.handleValidatingLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(result.FailureMsg, "cordoned") {
		t.Errorf("FailureMsg: want cordoned message, got %q", result.FailureMsg)
	}
}

// --- Phase 3b PR 2 Commit D: net-new test coverage --------------------

// TestValidatingLive_ImageTagMatch_HappyPath verifies that when the src
// pod's launcher container image matches the controller's default
// (swiftguest.LauncherImage), the LBA-1 trip-wire passes silently and
// the phase advances normally.
func TestValidatingLive_ImageTagMatch_HappyPath(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	// Pin the launcher image via env so the test asserts on a stable
	// value (independent of LauncherImageDefault drift).
	t.Setenv(swiftguest.LauncherImageEnv, "ghcr.io/test/swiftletd:v1.0.0")
	srcPod := newSourcePodWithLauncherImage("guest", "default", "uid", "ghcr.io/test/swiftletd:v1.0.0")
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleValidatingLive(context.Background(), mig, status)
	if result.FailureMsg != "" {
		t.Fatalf("expected silent pass on matching image tags; got failure: %q", result.FailureMsg)
	}
	if !result.Advanced {
		t.Fatal("expected Advanced=true on matching image tags")
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhasePreparing {
		t.Errorf("phase: want Preparing, got %q", status.Phase)
	}
}

// TestValidatingLive_ImageTagMatch_Mismatch_FailsWithImageTagMismatch
// is the LBA-1 trip-wire fail-loud assertion. If a future refactor
// regresses newDstPod's clone-src guarantee (or if a partial rolling
// upgrade puts the controller and src pod on different launcher
// versions), the migration must fail with ImageTagMismatch and the
// message must point operators at LBA-1.
func TestValidatingLive_ImageTagMatch_Mismatch_FailsWithImageTagMismatch(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	t.Setenv(swiftguest.LauncherImageEnv, "ghcr.io/test/swiftletd:v2.0.0")
	// src pod runs the OLD launcher image; controller default is v2.0.0.
	srcPod := newSourcePodWithLauncherImage("guest", "default", "uid", "ghcr.io/test/swiftletd:v1.0.0")
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	result := r.handleValidatingLive(context.Background(), mig, &mig.Status)
	if result.FailureReason != migrationv1alpha1.FailureReasonImageTagMismatch {
		t.Errorf("FailureReason: want ImageTagMismatch, got %q", result.FailureReason)
	}
	if !strings.Contains(result.FailureMsg, "v1.0.0") || !strings.Contains(result.FailureMsg, "v2.0.0") {
		t.Errorf("FailureMsg should include both image strings; got %q", result.FailureMsg)
	}
	if !strings.Contains(result.FailureMsg, "LBA-1") {
		t.Errorf("FailureMsg should reference LBA-1; got %q", result.FailureMsg)
	}
}

// TestValidatingLive_ImageTagMatch_NoLauncherContainer_DefensiveSkip
// verifies that when the src pod has no container named "launcher"
// (configuration gap, not a regression), the trip-wire defensively
// returns nil and the phase proceeds normally. The trip-wire is not
// load-bearing for correctness; this path matches the docstring's
// "Common path skips" guarantee.
func TestValidatingLive_ImageTagMatch_NoLauncherContainer_DefensiveSkip(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	t.Setenv(swiftguest.LauncherImageEnv, "ghcr.io/test/swiftletd:v1.0.0")
	// newSourcePod produces a pod with NO containers, so
	// launcherContainerImage returns "" → defensive skip.
	srcPod := newSourcePod("guest", "default", "uid")
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleValidatingLive(context.Background(), mig, status)
	if result.FailureMsg != "" {
		t.Fatalf("expected defensive-skip pass on missing launcher container; got failure: %q", result.FailureMsg)
	}
	if !result.Advanced {
		t.Fatal("expected Advanced=true under defensive-skip")
	}
}

// TestValidatingLive_MigrationDisabled_FailsWithEligibilityMismatch
// is the defense-in-depth path: webhook caught migration.enabled=false
// at admission, but a SwiftGuest mutation between admission and
// reconcile flipped it. Phase 3b PR 2 reclassified this from Other →
// EligibilityMismatch (Commit C wiring).
func TestValidatingLive_MigrationDisabled_FailsWithEligibilityMismatch(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	disabled := false
	guest.Spec.Migration = &swiftv1alpha1.MigrationSpec{Enabled: &disabled}
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	srcPod := newSourcePod("guest", "default", "uid")
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	result := r.handleValidatingLive(context.Background(), mig, &mig.Status)
	if result.FailureReason != migrationv1alpha1.FailureReasonEligibilityMismatch {
		t.Errorf("FailureReason: want EligibilityMismatch (Phase 3b PR 2; refined from Other), got %q", result.FailureReason)
	}
	if !strings.Contains(result.FailureMsg, "migration.enabled=false") {
		t.Errorf("FailureMsg: want migration.enabled message, got %q", result.FailureMsg)
	}
}
