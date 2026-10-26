package swiftmigration

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/migrationcert"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/migrationsidecar"
)

// withClientStunnelSidecar adds a client-role migration-stunnel sidecar to a
// source pod so it passes the Phase 3c sourcePodMTLSReady fail-fast (the
// SwiftGuest controller injects this on real migration-eligible pods).
func withClientStunnelSidecar(p *corev1.Pod) *corev1.Pod {
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{
		Name:  migrationsidecar.ContainerName,
		Image: "ghcr.io/test/stunnel:v1",
		Env:   []corev1.EnvVar{{Name: migrationsidecar.EnvRole, Value: migrationsidecar.RoleClient}},
	})
	return ranAsCreated(p)
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
	return ranAsCreated(p)
}

// ranAsCreated sets the pod's container statuses to its spec images, as the
// kubelet reports a launcher pod whose spec nobody patched.
func ranAsCreated(p *corev1.Pod) *corev1.Pod {
	statuses := func(cs []corev1.Container) []corev1.ContainerStatus {
		var out []corev1.ContainerStatus
		for _, c := range cs {
			out = append(out, corev1.ContainerStatus{Name: c.Name, Image: c.Image, ImageID: "sha256:0123"})
		}
		return out
	}
	p.Status.InitContainerStatuses = statuses(p.Spec.InitContainers)
	p.Status.ContainerStatuses = statuses(p.Spec.Containers)
	return p
}

// newSourcePod creates a fake source pod for the given guest, with a
// fixed UID so tests can assert SourcePodUID stamping.
func newSourcePod(guestName, ns, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       guestName,
			Namespace:  ns,
			UID:        types.UID(uid),
			Generation: 1, // as created; the API server bumps it on a spec change
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guestName,
			},
			// The guest's own launcher: controlled by it. Test guests have no
			// UID, so the reference carries none either.
			OwnerReferences: []metav1.OwnerReference{guestControllerRef(guestName)},
		},
		Spec: corev1.PodSpec{
			NodeName: "worker-1",
		},
	}
}

// guestControllerRef is the controller reference the SwiftGuest controller
// puts on a guest's launcher pod.
func guestControllerRef(guestName string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: swiftv1alpha1.GroupName + "/" + swiftv1alpha1.Version,
		Kind:       "SwiftGuest",
		Name:       guestName,
		Controller: ptr.To(true),
	}
}

// An explicit mode=live on storage that cannot be live-migrated must fail in
// Validating with EligibilityMismatch. webhook.enabled defaults to false, so
// the controller is the only place this is guaranteed to be checked.
func TestValidatingLive_NonLiveCapableStorage_FailsEligibility(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	class.Spec.Storage = nil // defaults: ReadWriteOnce + Filesystem
	node := newSpaciousNode("worker-2", 8, 65536)
	srcPod := newSourcePod("guest", "default", "src-pod-uid-1")
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * 60 * 1e9}
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	result := r.handleValidatingLive(context.Background(), mig, mig.Status.DeepCopy())
	if result.FailureMsg == "" {
		t.Fatal("expected a storage-gate failure for RWO/Filesystem storage")
	}
	if result.FailureReason != migrationv1alpha1.FailureReasonEligibilityMismatch {
		t.Errorf("FailureReason: want EligibilityMismatch, got %q", result.FailureReason)
	}
	if !strings.Contains(result.FailureMsg, "ReadWriteMany") {
		t.Errorf("failure message should name the requirement; got %q", result.FailureMsg)
	}
}

func TestValidatingLive_HappyPath_AdvancesToPreparing(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("worker-2", 8, 65536)
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
	if status.SourceNode != "worker-1" {
		t.Errorf("SourceNode: want worker-1, got %q", status.SourceNode)
	}
	if status.DestinationNode != "worker-2" {
		t.Errorf("DestinationNode: want worker-2, got %q", status.DestinationNode)
	}
}

// TestValidatingLive_MTLS_IdentitiesPresent_Advances verifies the
// Phase 3c precondition: with mTLS enabled and both node identity
// Secrets present in the system namespace, Validating-live advances AND
// distributes the DESTINATION node's identity into the guest namespace
// (the destination pod mounts it). The source node's identity goes only
// into the per-guest Secret the source sidecar mounts — its full cert+key
// is not copied as migration-node-<src>, so a node-wide key is not left
// sitting in the tenant namespace.
func TestValidatingLive_MTLS_IdentitiesPresent_Advances(t *testing.T) {
	scheme := validatingScheme(t)
	const sysNS = "kubeswift-system"
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("worker-2", 8, 65536)
	srcPod := withClientStunnelSidecar(newSourcePod("guest", "default", "src-pod-uid-1")) // src node "worker-1"
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * 60 * 1e9}
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	srcSecret := migrationNodeIdentitySecret(sysNS, "worker-1")
	dstSecret := migrationNodeIdentitySecret(sysNS, "worker-2")

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
	// The DESTINATION node's identity is copied into the guest namespace.
	var dst corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: migrationcert.MigrationNodeSecretName("worker-2")}, &dst); err != nil {
		t.Errorf("destination identity Secret not distributed into guest namespace: %v", err)
	}
	// The SOURCE node's full identity must NOT be copied as migration-node-<src>;
	// the source sidecar reads the per-guest Secret instead.
	var srcCopy corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: migrationcert.MigrationNodeSecretName("worker-1")}, &srcCopy); !apierrors.IsNotFound(err) {
		t.Errorf("source node's full identity should not be copied into the tenant namespace; got err=%v", err)
	}
	// The per-guest identity Secret carries the source identity.
	var perGuest corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: swiftguest.PerGuestMigrationIdentitySecretName("guest")}, &perGuest); err != nil {
		t.Errorf("per-guest source identity Secret not populated: %v", err)
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
	node := newSpaciousNode("worker-2", 8, 65536)
	srcPod := withClientStunnelSidecar(newSourcePod("guest", "default", "src-pod-uid-1"))
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * 60 * 1e9}
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	// Only the source-node Secret present; destination-node Secret missing.
	srcSecret := migrationNodeIdentitySecret(sysNS, "worker-1")

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
	node := newSpaciousNode("worker-2", 8, 65536)
	srcPod := newSourcePod("guest", "default", "src-pod-uid-1") // NO sidecar
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * 60 * 1e9}
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod,
			migrationNodeIdentitySecret(sysNS, "worker-1"), migrationNodeIdentitySecret(sysNS, "worker-2")).
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
	node := newSpaciousNode("worker-2", 8, 65536)
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
	node := newSpaciousNode("worker-2", 8, 65536)
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
	cordonedNode := newSpaciousNode("worker-2", 8, 65536)
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

// The launcher-image checks are in image_upgrade_test.go.

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
	node := newSpaciousNode("worker-2", 8, 65536)
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

// A shared-base guest cannot be migrated in ANY mode, and the refusal happens
// once, in handleValidating, before auto-resolution and the mode dispatch.
//
// Offline is the case that used to be wrong: auto resolved shared-base guests
// to offline, and the refusal recommended it. But offline only sets
// spec.nodeName to the target and restarts — it copies nothing — so the guest
// would arrive on a node that does not hold its disk.
func TestHandleValidating_SharedBaseDiskRefusesEveryMode(t *testing.T) {
	for _, mode := range []migrationv1alpha1.SwiftMigrationMode{
		migrationv1alpha1.SwiftMigrationModeLive,
		migrationv1alpha1.SwiftMigrationModeOffline,
		migrationv1alpha1.SwiftMigrationModeAuto,
	} {
		t.Run(string(mode), func(t *testing.T) {
			scheme := testScheme(t)
			guest := &swiftv1alpha1.SwiftGuest{
				ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
				Spec:       swiftv1alpha1.SwiftGuestSpec{GuestClassRef: corev1.LocalObjectReference{Name: "shared"}},
			}
			// Cluster-scoped: no namespace.
			class := &swiftv1alpha1.SwiftGuestClass{
				ObjectMeta: metav1.ObjectMeta{Name: "shared"},
				Spec:       swiftv1alpha1.SwiftGuestClassSpec{SharedBaseDisk: true},
			}
			mig := newMigration("m", "default")
			mig.Spec.Mode = mode
			mig.Spec.AllowIPChange = true // so networking cannot be what refuses it

			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class).Build()
			r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

			res := r.handleValidating(context.Background(), mig, &mig.Status)
			if res == nil || res.FailureMsg == "" {
				t.Fatalf("mode=%s migration of a sharedBaseDisk guest was not refused (result %+v)", mode, res)
			}
			if !strings.Contains(res.FailureMsg, "cannot be migrated in any mode") {
				t.Errorf("wrong refusal: %q", res.FailureMsg)
			}
			// Auto must fail outright, not resolve to a mode and carry on.
			if mig.Status.Mode == migrationv1alpha1.SwiftMigrationModeOffline ||
				mig.Status.Mode == migrationv1alpha1.SwiftMigrationModeLive {
				t.Errorf("mode=%s was resolved to %q before being refused", mode, mig.Status.Mode)
			}
		})
	}
}
