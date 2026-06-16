package swiftmigration

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/controller/migrationcert"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftguest"
	"github.com/projectbeskar/kubeswift/internal/migrationsidecar"
)

func TestIsLocalStunnelNotReady(t *testing.T) {
	cases := []struct {
		detail string
		want   bool
	}{
		{"send_migration: connection_refused", true},
		{"SEND_MIGRATION: CONNECTION_REFUSED", true},     // case-insensitive
		{"send_migration: transport_error", false},       // dst-gone, not local-not-ready
		{"receive_migration: connection_refused", false}, // dst side, not src-local
		{"", false},
		{"some other error", false},
	}
	for _, tc := range cases {
		if got := isLocalStunnelNotReady(tc.detail); got != tc.want {
			t.Errorf("isLocalStunnelNotReady(%q)=%v, want %v", tc.detail, got, tc.want)
		}
	}
}

// TestIsDestReceiverNotReady covers the primary-on-NAD send-retry case found in
// the multi-node-L2 spike: the dst CH receiver hadn't bound its listener yet, so
// the src send reset early and CH surfaced a generic Status-500 →
// "internal_server_error". This is retryable (no data flowed, dst still alive);
// it must NOT collide with the src-local case or the dst-gone transport_error.
func TestIsDestReceiverNotReady(t *testing.T) {
	cases := []struct {
		detail string
		want   bool
	}{
		{"send_migration: internal_server_error", true},
		{"SEND_MIGRATION: INTERNAL_SERVER_ERROR", true},     // case-insensitive
		{"send_migration: transport_error", false},          // dst-gone mid-transfer, not a not-ready race
		{"send_migration: connection_refused", false},       // that is the src-local-stunnel case
		{"receive_migration: internal_server_error", false}, // dst-side verb, not the src send
		{"", false},
		{"some other error", false},
	}
	for _, tc := range cases {
		if got := isDestReceiverNotReady(tc.detail); got != tc.want {
			t.Errorf("isDestReceiverNotReady(%q)=%v, want %v", tc.detail, got, tc.want)
		}
	}
}

// --- target_url repoint (substatePreSend) --------------------------------

func setupPreSend(t *testing.T, mtls bool) (*SwiftMigrationReconciler, *corev1.Pod) {
	t.Helper()
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	// dst is receive-ready; src has no send yet => substatePreSend.
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusReceiveReady, recvActionID(mig), "")
	dst.Status.PodIP = "10.0.0.9"
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	r.MigrationMTLSEnabled = mtls
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("unexpected failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}
	var gotSrc corev1.Pod
	if err := r.Get(context.Background(), key(src), &gotSrc); err != nil {
		t.Fatalf("re-get src: %v", err)
	}
	return r, &gotSrc
}

func preSendTargetURL(t *testing.T, src *corev1.Pod) string {
	t.Helper()
	var args migrationSendArgs
	if err := json.Unmarshal([]byte(src.Annotations[AnnotationMigrationActionArgs]), &args); err != nil {
		t.Fatalf("unmarshal send args %q: %v", src.Annotations[AnnotationMigrationActionArgs], err)
	}
	return args.TargetURL
}

func TestStopAndCopyLive_PreSend_MTLS_TargetLocalhost(t *testing.T) {
	_, src := setupPreSend(t, true)
	if got := preSendTargetURL(t, src); got != "tcp:127.0.0.1:6790" {
		t.Errorf("mTLS target_url: want tcp:127.0.0.1:6790 (local stunnel client), got %q", got)
	}
}

func TestStopAndCopyLive_PreSend_Plaintext_TargetDstIP(t *testing.T) {
	_, src := setupPreSend(t, false)
	if got := preSendTargetURL(t, src); got != "tcp:10.0.0.9:6789" {
		t.Errorf("plaintext target_url: want tcp:10.0.0.9:6789, got %q", got)
	}
}

// TestStopAndCopyLive_PreRecv_MTLS_SkipsAckPatch verifies the Phase 3c
// cleanup: under mTLS the src pod is NOT patched with the plaintext-ack
// annotation (swiftletd bypasses the gate in secured mode) — but the
// migration-name label IS still applied (informer observability).
func TestStopAndCopyLive_PreRecv_MTLS_SkipsAckPatch(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	r.MigrationMTLSEnabled = true

	status := mig.Status.DeepCopy()
	_ = r.handleStopAndCopyLive(context.Background(), mig, status)

	var got corev1.Pod
	if err := r.Get(context.Background(), key(src), &got); err != nil {
		t.Fatalf("re-get src: %v", err)
	}
	if _, present := got.Annotations[AnnotationMigrationPhase2Ack]; present {
		t.Errorf("mTLS src must NOT be patched with the plaintext-ack annotation; got %q",
			got.Annotations[AnnotationMigrationPhase2Ack])
	}
	if got.Labels[LabelMigrationName] != mig.Name {
		t.Errorf("src must still get the migration-name label under mTLS; got %q", got.Labels[LabelMigrationName])
	}
}

// --- bounded send retry (substateSrcFailed) ------------------------------

func srcFailedReconciler(t *testing.T, mtls bool, sendAttempts int32, detail string, dstTerminating bool) (*SwiftMigrationReconciler, *migrationv1alpha1.SwiftMigration) {
	t.Helper()
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = sendAttempts
	stamp(src, migrationActionVerbSend, sendActionID(mig), MigrationStatusFailed, sendActionID(mig), detail)
	if dstTerminating {
		now := metav1.Now()
		dst.DeletionTimestamp = &now
		dst.Finalizers = []string{"kubernetes.io/grace-period"}
	}
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	r.MigrationMTLSEnabled = mtls
	return r, mig
}

func TestStopAndCopyLive_SrcFailed_MTLS_ConnRefused_RetriesSend(t *testing.T) {
	r, mig := srcFailedReconciler(t, true, 1, "send_migration: connection_refused", false)
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureMsg != "" || res.FailureReason != "" {
		t.Fatalf("expected retry (no failure); got msg=%q reason=%q", res.FailureMsg, res.FailureReason)
	}
	if res.Requeue == 0 {
		t.Errorf("expected requeue on retry")
	}
	if status.SendAttempts != 2 {
		t.Errorf("SendAttempts: want 2 (bumped for retry), got %d", status.SendAttempts)
	}
}

func TestStopAndCopyLive_SrcFailed_MTLS_ConnRefused_BudgetExhausted_Fails(t *testing.T) {
	r, mig := srcFailedReconciler(t, true, maxMTLSSendRetries, "send_migration: connection_refused", false)
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason == "" {
		t.Fatalf("expected terminal failure at budget; got retry (reason empty, requeue=%v)", res.Requeue)
	}
	if res.FailureReason != migrationv1alpha1.FailureReasonReceiveDisconnect {
		t.Errorf("FailureReason at budget: want ReceiveDisconnect (connection_refused classifier), got %q", res.FailureReason)
	}
}

func TestStopAndCopyLive_SrcFailed_MTLS_TransportError_NoRetry(t *testing.T) {
	// transport_error == dst-gone mid-stream, NOT local-not-ready: must fail, not retry.
	r, mig := srcFailedReconciler(t, true, 1, "send_migration: transport_error", false)
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason == "" {
		t.Fatalf("transport_error must not be retried; expected failure")
	}
}

func TestStopAndCopyLive_SrcFailed_MTLSOff_ConnRefused_NoRetry(t *testing.T) {
	// With mTLS off, connection_refused is a real dst-unreachable failure.
	r, mig := srcFailedReconciler(t, false, 1, "send_migration: connection_refused", false)
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason == "" {
		t.Fatalf("mTLS-off connection_refused must not be retried; expected failure")
	}
}

func TestStopAndCopyLive_SrcFailed_MTLS_DstTerminating_NoRetry(t *testing.T) {
	// Even with connection_refused, a terminating dst is a genuine failure.
	r, mig := srcFailedReconciler(t, true, 1, "send_migration: connection_refused", true)
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason == "" {
		t.Fatalf("terminating dst must not be retried; expected failure")
	}
}

func TestStopAndCopyLive_SrcFailed_MTLS_DestReceiverNotReady_RetriesSend(t *testing.T) {
	// Multi-node-L2 spike fix: internal_server_error with the dst pod ALIVE is the
	// dst-CH-receiver-not-ready race (early reset, no data flowed) — must retry.
	r, mig := srcFailedReconciler(t, true, 1, "send_migration: internal_server_error", false)
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureMsg != "" || res.FailureReason != "" {
		t.Fatalf("expected retry (no failure); got msg=%q reason=%q", res.FailureMsg, res.FailureReason)
	}
	if res.Requeue == 0 {
		t.Errorf("expected requeue on retry")
	}
	if status.SendAttempts != 2 {
		t.Errorf("SendAttempts: want 2 (bumped for retry), got %d", status.SendAttempts)
	}
}

func TestStopAndCopyLive_SrcFailed_MTLS_InternalServerError_DstTerminating_NoRetry(t *testing.T) {
	// The W18 gate: internal_server_error while the dst is TERMINATING is a genuine
	// dst-K8s-termination (shares the symptom but the dst is going away) — must fail,
	// NOT be mistaken for the not-ready-yet race.
	r, mig := srcFailedReconciler(t, true, 1, "send_migration: internal_server_error", true)
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason == "" {
		t.Fatalf("terminating dst must not be retried even on internal_server_error; expected failure")
	}
}

// --- stampSourceMigrationInputs ------------------------------------------

func TestStampSourceMigrationInputs_PatchesAndIsIdempotent(t *testing.T) {
	scheme := testScheme(t)
	src := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "team-a"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(src).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(5)}
	ctx := context.Background()

	if err := r.stampSourceMigrationInputs(ctx, src, "10.0.0.9", "miles"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	var got corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "guest"}, &got); err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if got.Annotations[migrationsidecar.AnnotationDstPodIP] != "10.0.0.9" {
		t.Errorf("dst-ip annotation: got %q", got.Annotations[migrationsidecar.AnnotationDstPodIP])
	}
	if got.Annotations[migrationsidecar.AnnotationPeerSAN] != "miles" {
		t.Errorf("peer-san annotation: got %q", got.Annotations[migrationsidecar.AnnotationPeerSAN])
	}
	// Idempotent: second call with same values is a no-op (no error).
	if err := r.stampSourceMigrationInputs(ctx, &got, "10.0.0.9", "miles"); err != nil {
		t.Errorf("idempotent stamp errored: %v", err)
	}
}

// --- populateSourceIdentity ----------------------------------------------

func TestPopulateSourceIdentity_CopiesNodeIdentityIntoPerGuestSecret(t *testing.T) {
	scheme := testScheme(t)
	const sysNS = "kubeswift-system"
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "team-a", UID: "guest-uid"}}
	srcNodeSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: migrationcert.MigrationNodeSecretName("boba"), Namespace: sysNS},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("CRT"), "tls.key": []byte("KEY"), "ca.crt": []byte("CA")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, srcNodeSecret).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, SystemNamespace: sysNS}
	ctx := context.Background()

	if err := r.populateSourceIdentity(ctx, guest, "boba"); err != nil {
		t.Fatalf("populateSourceIdentity: %v", err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: swiftguest.PerGuestMigrationIdentitySecretName("guest")}, &s); err != nil {
		t.Fatalf("per-guest identity not created: %v", err)
	}
	if string(s.Data["tls.crt"]) != "CRT" || string(s.Data["tls.key"]) != "KEY" || string(s.Data["ca.crt"]) != "CA" {
		t.Errorf("per-guest identity data mismatch: %+v", s.Data)
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != "guest" {
		t.Errorf("per-guest identity must be guest-owned; got %+v", s.OwnerReferences)
	}
	// Idempotent: second call with identical source is a no-op.
	if err := r.populateSourceIdentity(ctx, guest, "boba"); err != nil {
		t.Errorf("idempotent populate errored: %v", err)
	}
}

func TestPopulateSourceIdentity_UpdatesExistingPlaceholder(t *testing.T) {
	scheme := testScheme(t)
	const sysNS = "kubeswift-system"
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "team-a", UID: "guest-uid"}}
	srcNodeSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: migrationcert.MigrationNodeSecretName("boba"), Namespace: sysNS},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": []byte("CRT"), "tls.key": []byte("KEY"), "ca.crt": []byte("CA")},
	}
	// Empty placeholder already created by the SwiftGuest controller.
	placeholder := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: swiftguest.PerGuestMigrationIdentitySecretName("guest"), Namespace: "team-a"},
		Type:       corev1.SecretTypeOpaque,
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, srcNodeSecret, placeholder).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, SystemNamespace: sysNS}
	ctx := context.Background()

	if err := r.populateSourceIdentity(ctx, guest, "boba"); err != nil {
		t.Fatalf("populateSourceIdentity: %v", err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: swiftguest.PerGuestMigrationIdentitySecretName("guest")}, &s); err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(s.Data["tls.crt"]) != "CRT" {
		t.Errorf("placeholder not populated; tls.crt=%q", s.Data["tls.crt"])
	}
}

func TestPopulateSourceIdentity_MissingSourceErrors(t *testing.T) {
	scheme := testScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "team-a", UID: "guest-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, SystemNamespace: "kubeswift-system"}
	if err := r.populateSourceIdentity(context.Background(), guest, "boba"); err == nil {
		t.Errorf("missing source node identity must error; got nil")
	}
}
