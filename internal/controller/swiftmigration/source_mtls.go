package swiftmigration

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/migrationcert"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/migrationsidecar"
)

// Phase 3c PR 3d — SOURCE-side activation of the live-migration mTLS path.
//
// The source launcher pod was born (PR 3b) with an IDLE stunnel client
// sidecar that idle-polls for three inputs. This file is where the
// SwiftMigration controller provides them when a migration runs:
//   - populateSourceIdentity copies the source node's issued identity into
//     the per-guest Secret the sidecar mounts (Validating-live),
//   - stampSourceMigrationInputs writes the dst-ip / peer-san annotations
//     the sidecar reads via its downward-API volume (Preparing-live, as
//     early as the dst pod has an IP, per the "stamp early" decision),
//   - the bounded send retry (isLocalStunnelNotReady + maxMTLSSendRetries,
//     consumed in stopandcopy_live) is the residual-race safety net.

// maxMTLSSendRetries bounds how many times the controller re-issues the
// send-action while the source mTLS sidecar is still coming up. Each retry
// is one substatePreSend->substateSendPending cycle (a few reconciles at
// stopAndCopyLivePollInterval); ~10 covers kubelet downward-API + Secret
// propagation on a slow-sync cluster. spec.timeout is the hard outer cap.
const maxMTLSSendRetries = 10

// sourcePodMTLSReady reports whether the source pod is ready to act as the
// TLS CLIENT for an mTLS migration: it must carry a migration-stunnel
// sidecar whose STUNNEL_ROLE is "client". Two cases fail this:
//
//   - the source pod predates mTLS enablement (no sidecar at all — the
//     SwiftGuest controller only injects it on newly created pods), or
//   - the source pod is a post-cutover DESTINATION pod from a prior
//     migration (it carries a SERVER-role sidecar; chain migrations under
//     mTLS need a pod recycle first — the role is immutable).
//
// Failing fast here turns a confusing ~60s retry-then-timeout (the source
// CH would keep hitting a local port with no client listening) into a
// single clear admission-time failure with a recycle hint.
func sourcePodMTLSReady(srcPod *corev1.Pod) bool {
	for i := range srcPod.Spec.Containers {
		c := &srcPod.Spec.Containers[i]
		if c.Name != migrationsidecar.ContainerName {
			continue
		}
		for _, e := range c.Env {
			if e.Name == migrationsidecar.EnvRole {
				return e.Value == migrationsidecar.RoleClient
			}
		}
		return false // sidecar present but no role env (treat as not client-ready)
	}
	return false // no sidecar
}

// isLocalStunnelNotReady reports whether a src-side migration-status-detail
// indicates the LOCAL stunnel client (127.0.0.1:6790) was not yet
// listening when the src CH tried to send — i.e. the source sidecar hasn't
// finished idle-polling its inputs and started stunnel.
//
// Reliable signal ONLY under mTLS: with mTLS on, target_url is the local
// client, so a CH "connection_refused" means localhost had nothing
// listening yet. A dst-side disappearance surfaces as transport_error (CH
// connected to the up local client, which then failed reaching the dst),
// NOT connection_refused — so this never misfires on a genuine dst failure.
// Callers MUST additionally gate on r.MigrationMTLSEnabled.
func isLocalStunnelNotReady(detail string) bool {
	d := strings.ToLower(strings.TrimSpace(detail))
	return strings.HasPrefix(d, "send_migration:") && strings.Contains(d, "connection_refused")
}

// isDestReceiverNotReady reports whether a src-side migration-status-detail
// indicates the DESTINATION CH receiver had not yet bound its listener when
// the src CH tried to send. Under mTLS the src CH dials its (up) LOCAL stunnel,
// which forwards to the dst stunnel -> dst CH `vm.receive-migration`
// (127.0.0.1:6790 on the dst). If the dst CH receiver hasn't called
// receive-migration yet, the dst stunnel cannot forward and resets; the src
// local stunnel resets the loopback with 0 bytes sent; CH surfaces a generic
// Status-500 that swiftletd sanitizes to "internal_server_error". Because the
// channel never carried data, re-issuing the send is safe.
//
// This is distinct from a dst *disappearance* mid-transfer (data had flowed),
// which surfaces as "transport_error" and is a genuine failure (excluded here).
// The caller MUST additionally gate on the dst pod NOT terminating so the W18
// dst-K8s-termination case (same "internal_server_error" symptom, but the dst
// is going away) is never mistaken for a not-ready-yet race.
//
// The race bites primary-on-NAD guests: the NAD dst pod's network-init runs the
// multi-node-L2 datapath, making it slower to reach receive-migration, so the
// controller's PreSend can out-run the dst receiver. Non-NAD dst pods get ready
// fast enough to win the race. Found in the multi-node-L2 validation spike.
func isDestReceiverNotReady(detail string) bool {
	d := strings.ToLower(strings.TrimSpace(detail))
	return strings.HasPrefix(d, "send_migration:") && strings.Contains(d, "internal_server_error")
}

// stampSourceMigrationInputs idempotently patches the dst-ip / peer-san
// annotations onto the source pod. The SwiftGuest controller's downward-API
// volume projects these into files the idle client sidecar polls; stamping
// them is what unblocks the sidecar and lets it bring stunnel up.
//
// dstIP is the destination pod IP (the TLS connect target); dstNodeSAN is
// the destination node's SAN (the client pins it via checkHost). Stamped as
// early as the dst pod has an IP (Preparing-live) to maximise the
// propagation window before the send is issued.
func (r *SwiftMigrationReconciler) stampSourceMigrationInputs(ctx context.Context, srcPod *corev1.Pod, dstIP, dstNodeSAN string) error {
	if srcPod.Annotations[migrationsidecar.AnnotationDstPodIP] == dstIP &&
		srcPod.Annotations[migrationsidecar.AnnotationPeerSAN] == dstNodeSAN {
		return nil // already stamped with the same values
	}
	patch := client.MergeFrom(srcPod.DeepCopy())
	if srcPod.Annotations == nil {
		srcPod.Annotations = map[string]string{}
	}
	srcPod.Annotations[migrationsidecar.AnnotationDstPodIP] = dstIP
	srcPod.Annotations[migrationsidecar.AnnotationPeerSAN] = dstNodeSAN
	return r.Patch(ctx, srcPod, patch)
}

// populateSourceIdentity copies the SOURCE node's issued migration identity
// (cert-manager Secret kubeswift-migration-node-<srcNode> in the system
// namespace) into the per-guest identity Secret the source sidecar mounts
// (<guest>-migration-identity, created empty by the SwiftGuest controller).
//
// This is distribution, not issuance — the per-node cert is a long-lived
// precondition (Validating-live already verified it exists). Upsert: update
// the placeholder's Data when present, create it (guest-owned) if the
// SwiftGuest controller hasn't yet. Never errors the migration on an
// already-equal Secret (idempotent across reconciles).
func (r *SwiftMigrationReconciler) populateSourceIdentity(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, srcNode string) error {
	srcKey := types.NamespacedName{Namespace: r.SystemNamespace, Name: migrationcert.MigrationNodeSecretName(srcNode)}
	var source corev1.Secret
	if err := r.Get(ctx, srcKey, &source); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("source node migration identity %s not provisioned (cert-manager precondition not ready): %w", srcKey, err)
		}
		return fmt.Errorf("get source node migration identity %s: %w", srcKey, err)
	}
	data := map[string][]byte{}
	for _, k := range []string{"tls.crt", "tls.key", "ca.crt"} {
		if v, ok := source.Data[k]; ok {
			data[k] = v
		}
	}

	name := swiftguest.PerGuestMigrationIdentitySecretName(guest.Name)
	key := types.NamespacedName{Namespace: guest.Namespace, Name: name}
	var existing corev1.Secret
	err := r.Get(ctx, key, &existing)
	if err == nil {
		if secretBytesEqual(existing.Data, data) {
			return nil
		}
		existing.Data = data
		if uerr := r.Update(ctx, &existing); uerr != nil {
			return fmt.Errorf("update per-guest migration identity %s: %w", key, uerr)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get per-guest migration identity %s: %w", key, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kubeswift",
				"app.kubernetes.io/component":  "migration-mtls",
				"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
				"swift.kubeswift.io/guest":     guest.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if serr := controllerutil.SetControllerReference(guest, secret, r.Scheme); serr != nil {
		return fmt.Errorf("set ownerRef on per-guest migration identity %s: %w", key, serr)
	}
	if cerr := r.Create(ctx, secret); cerr != nil {
		if apierrors.IsAlreadyExists(cerr) {
			return nil
		}
		return fmt.Errorf("create per-guest migration identity %s: %w", key, cerr)
	}
	return nil
}

func secretBytesEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || !bytes.Equal(av, bv) {
			return false
		}
	}
	return true
}
