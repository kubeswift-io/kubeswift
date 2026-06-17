package swiftmigration

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftguest"
)

// Pod label keys / values for the destination launcher pod. Per design
// §3.2 (pod naming and labels) and §5.1 (informer indexing).
const (
	// LabelMigrationRole distinguishes pods participating in a live
	// migration. Value "destination" identifies the dst pod created
	// by the SwiftMigration controller during Preparing-live; the
	// matching "source" value lands in B3 when the cutover sub-state
	// patches the src pod.
	LabelMigrationRole = "kubeswift.io/migration-role"
	// LabelMigrationName carries the SwiftMigration's metadata.Name.
	// Used by the labeled informer (§5.1) to enqueue SwiftMigration
	// reconciles on dst-pod events without filtering through every
	// pod in the cluster.
	LabelMigrationName = "kubeswift.io/migration"
	// LabelGuestName carries the SwiftGuest's metadata.Name. Phase 1
	// already uses this on src pods; preserved on dst pods so swiftletd
	// status-reporter RBAC matches both.
	LabelGuestName = "swift.kubeswift.io/guest"

	// MigrationRoleDestination identifies dst pods.
	MigrationRoleDestination = "destination"

	// AnnotationMigrationPhase2Ack is the ack-gate annotation Phase 2
	// PR-B's swiftletd reads inside decide() to enable migration
	// action handlers. The destination pod MUST carry this annotation
	// at creation time (F1.6: ack lifecycle is flexible; pod-creation-
	// time is the simplest place to set it). swiftletd's source for
	// this constant: rust/swiftletd/src/action.rs's
	// MIGRATION_PHASE2_ACK_KEY.
	AnnotationMigrationPhase2Ack = "kubeswift.io/migration-phase2-unsafe-plaintext"
	// AnnotationMigrationPhase2AckValue is the required value.
	AnnotationMigrationPhase2AckValue = "ack"

	// EnvKubeswiftMigrationRole is the environment variable swiftletd's
	// main.rs reads (rust/swiftletd/src/main.rs:74) to switch to
	// receiver mode. Value "receiver" lands on dst pods; absent on
	// src pods (which start in normal mode).
	EnvKubeswiftMigrationRole = "KUBESWIFT_MIGRATION_ROLE"
	// EnvKubeswiftMigrationRoleReceiver is the value that triggers
	// receiver-mode boot.
	EnvKubeswiftMigrationRoleReceiver = "receiver"

	// LauncherContainerName is the swiftletd container name across
	// kernel-boot + disk-boot + restore paths (see
	// internal/controller/swiftguest/pod.go and restore.go).
	LauncherContainerName = "launcher"

	// shortUIDLength is the number of leading hex chars of the
	// SwiftMigration UID used in the dst pod name. Per design §3.2:
	// 6 chars is unique-enough cluster-wide given UID is uniform-
	// random and scoped per-migration.
	shortUIDLength = 6

	// dstPodNameMaxLength is the DNS-1123 cap for pod names. Phase 3a
	// admission (Group A's webhook) caps guest.Name at 242 chars for
	// mode=live, leaving 11 chars budget for "-mig-" + 6-char short
	// UID. The helper trusts the cap; an oversize name returns an
	// error rather than truncating (truncation would risk collision).
	dstPodNameMaxLength = 64
)

// dstPodName produces the deterministic destination pod name for the
// given SwiftMigration. Format: `<guest.Name>-mig-<short-uid>`.
//
// Determinism: same SwiftMigration UID always produces the same name,
// so leader-handover mid-Preparing observes the existing dst pod via
// the SAME name and skips Create. Different SwiftMigrations on the
// same SwiftGuest produce different names (UID differs), giving the
// per-migration-mutex property §2.3 relies on.
//
// Returns an error if the resulting name exceeds 64 chars (DNS-1123
// limit). The webhook's 242-char guest-name cap for mode=live makes
// this unreachable in practice; the check is defensive against
// admission bypass.
//
// Returns an error if SwiftMigration.UID is empty (defense against
// programming error — UID is always set on a created CR).
func dstPodName(mig *migrationv1alpha1.SwiftMigration, guestName string) (string, error) {
	if mig.UID == "" {
		return "", fmt.Errorf("SwiftMigration %q has empty UID; dst pod name cannot be derived", mig.Name)
	}
	uid := string(mig.UID)
	if len(uid) < shortUIDLength {
		return "", fmt.Errorf("SwiftMigration %q UID %q shorter than %d chars; cannot derive short-uid", mig.Name, uid, shortUIDLength)
	}
	short := uid[:shortUIDLength]
	name := fmt.Sprintf("%s-mig-%s", guestName, short)
	if len(name) > dstPodNameMaxLength {
		return "", fmt.Errorf("dst pod name %q exceeds DNS-1123 limit (%d chars > %d); guest.Name should have been capped at admission for mode=live", name, len(name), dstPodNameMaxLength)
	}
	return name, nil
}

// newDstPod constructs the destination launcher pod for a live
// migration.
//
// **Approach**: clone the source pod's spec with metadata reset,
// append the migration-specific labels/annotations/env, override
// NodeName, and set ownerRef on the SwiftGuest (F1.5 Option 2 + (P)).
//
// Cloning rather than re-resolving the SwiftGuest: the src pod is
// already on the cluster, already has the correct image, init
// containers, volume mounts, and env. The dst pod needs the same
// runtime intent (so swiftletd loads the same ConfigMaps, same disks,
// same network setup) — the only real differences are pod identity
// (name + ownership) and the receiver-mode env. Re-resolving the
// SwiftGuest via the resolved package would re-fetch SwiftKernel /
// SwiftImage / SwiftSeedProfile / SwiftGuestClass and re-build a pod
// from scratch; that's strictly more work and re-introduces a
// scheme-registration burden in tests.
//
// Idempotency: the helper produces an identical Pod object given the
// same inputs (modulo any transient src-pod ResourceVersion bumps,
// which are stripped). Callers re-running it on leader handover and
// then comparing against an existing-on-cluster dst pod can skip
// Create.
//
// Inputs:
//   - mig: the SwiftMigration. UID drives the name; metadata.Name
//     drives the LabelMigrationName.
//   - guest: the SwiftGuest. metadata.Name drives the LabelGuestName
//     and (with mig.UID) the dst pod name. Used as the ownerRef.
//   - srcPod: the source launcher pod. Spec is cloned; metadata is
//     reset.
//   - scheme: needed by SetControllerReference to resolve the
//     SwiftGuest's GVK for the ownerRef.
//
// dstSidecarConfig carries the Phase 3c (Option B) mTLS parameters
// newDstPod needs to optionally inject the destination stunnel sidecar.
// The zero value (mtlsEnabled=false) reproduces the pre-Phase-3c
// destination pod byte-for-byte — the plaintext path is unchanged.
//
// srcNodeName: the SOURCE node. The dst sidecar pins its SAN via
// CHECK_HOST so the TLS channel authorizes the specific source node for
// THIS migration (W-3c-4), not merely any CA-signed peer.
// dstNodeName: the DESTINATION node, whose per-node identity Secret the
// dst sidecar mounts and presents.
type dstSidecarConfig struct {
	mtlsEnabled bool
	srcNodeName string
	dstNodeName string
}

// Returns an error if dstPodName() fails, if SetControllerReference
// fails (scheme without SwiftGuest registered), if the launcher
// container can't be found (corrupt src pod), or if mTLS is enabled but
// the src/dst node names are empty (the SAN pin / identity Secret would
// be unresolvable). All errors are programming-error class — callers
// should phaseFailure with FailureReasonOther.
func newDstPod(
	mig *migrationv1alpha1.SwiftMigration,
	guest *swiftv1alpha1.SwiftGuest,
	srcPod *corev1.Pod,
	scheme *runtime.Scheme,
	sidecar dstSidecarConfig,
	frozenIntentCMName string,
	ovnDstAnnotations map[string]string,
) (*corev1.Pod, error) {
	name, err := dstPodName(mig, guest.Name)
	if err != nil {
		return nil, err
	}

	pod := srcPod.DeepCopy()

	// Reset metadata: drop UID, ResourceVersion, CreationTimestamp,
	// SelfLink, ManagedFields, OwnerReferences, Finalizers — anything
	// the apiserver populates or that would fail Create. Preserve the
	// useful labels (guest, etc) by starting from src.Labels and
	// adding migration-specific keys.
	preservedLabels := mergeLabelsForDst(srcPod.Labels, guest.Name, mig.Name)
	preservedAnnotations := mergeAnnotationsForDst(srcPod.Annotations, sidecar.mtlsEnabled, ovnDstAnnotations)

	pod.ObjectMeta = metav1.ObjectMeta{
		Name:        name,
		Namespace:   guest.Namespace,
		Labels:      preservedLabels,
		Annotations: preservedAnnotations,
	}
	if err := controllerutil.SetControllerReference(guest, pod, scheme); err != nil {
		return nil, fmt.Errorf("set controller reference on dst pod: %w", err)
	}

	// Override node pinning: dst goes to the migration's target node,
	// NOT the src pod's node.
	pod.Spec.NodeName = mig.Spec.Target.NodeName

	// Add KUBESWIFT_MIGRATION_ROLE=receiver to the launcher container.
	if err := addReceiverEnvToLauncher(pod); err != nil {
		return nil, err
	}

	// W-3c-1 / TFU #24: repoint the runtime-intent volume at the FROZEN
	// per-migration intent CM (lifecycle: start), so a stop-during-migration
	// flip of the live `<guest>-runtime-intent` CM cannot poison the dst
	// receiver's launch gate. Empty name (tests / pre-freeze callers) leaves
	// the inherited live CM in place.
	if frozenIntentCMName != "" {
		repointRuntimeIntentVolume(pod, frozenIntentCMName)
	}

	// Phase 3c (Option B): inject the destination stunnel sidecar (TLS
	// server). Gated on mtlsEnabled; the plaintext path skips this
	// entirely. The dst CH receives on localhost behind this sidecar
	// (listen_url repoint in stopandcopy_live), so swiftletd/CH stay
	// TLS-unaware (design §5 opacity contract).
	//
	// W-3c-1 lifecycle:run freeze is intentionally NOT done here: the
	// controller-driven live cutover Deletes the src pod and never
	// patches runPolicy:Stopped (cutover.go), and the dst receiver reads
	// its runtime intent once at boot during Preparing-live — before any
	// cutover action — so the <guest>-runtime-intent CM cannot flip to
	// lifecycle:stop while this pod depends on it. The freeze is
	// defense-in-depth for a hypothetical future stop-during-migration
	// path (e.g., Phase 4 drain integration); tracked as a follow-up.
	if sidecar.mtlsEnabled {
		if sidecar.srcNodeName == "" || sidecar.dstNodeName == "" {
			return nil, fmt.Errorf(
				"dst pod construction: mTLS enabled but node names unresolved (src=%q dst=%q); cannot pin SAN / mount identity Secret",
				sidecar.srcNodeName, sidecar.dstNodeName)
		}
		injectDstStunnelSidecar(pod, sidecar.srcNodeName, sidecar.dstNodeName)
	}

	// Reset pod status (Create rejects non-empty status; defensive even
	// though apiserver typically ignores it).
	pod.Status = corev1.PodStatus{}

	return pod, nil
}

// mergeLabelsForDst returns a fresh map containing the src pod's
// labels (preserving anything operator/platform code depends on)
// PLUS the migration-specific labels. Existing keys are NOT
// overridden — Phase 3a's labels use a distinct prefix
// (kubeswift.io/migration-*) and the guest label is preserved by
// equality.
func mergeLabelsForDst(srcLabels map[string]string, guestName, migName string) map[string]string {
	m := make(map[string]string, len(srcLabels)+3)
	for k, v := range srcLabels {
		m[k] = v
	}
	m[LabelGuestName] = guestName
	m[LabelMigrationRole] = MigrationRoleDestination
	m[LabelMigrationName] = migName
	return m
}

// mergeAnnotationsForDst returns a fresh map containing only the
// migration-relevant annotations. The src pod's annotations are mostly
// dropped intentionally: they include kubeswift.io/guest-ip and other
// runtime status the dst's swiftletd will repopulate, plus operator
// annotations that don't transfer to the dst pod cleanly.
//
// EXCEPTION — the Multus networks REQUEST annotation MUST transfer. It is
// boot-time config (which NADs to attach), not runtime status. A guest whose
// primary rides a multi-node-L2 NAD (primary-on-NAD) — or any guest with a
// secondary NAD NIC — needs the dst pod to request the same interfaces, or
// network-init's setup_primary_nad_nic / setup_secondary_nic wait forever for a
// `net1` that never plumbs and the dst pod never reaches Ready (the migration
// fails DstNeverReady). Offline migration is unaffected: it rebuilds the pod via
// the SwiftGuest pod builder, which re-adds this annotation from spec.interfaces;
// the live path clones the src pod through here, so this allowlist is the only
// place it can be preserved. (Found in the multi-node-L2 validation spike — the
// original allowlist predated primary-on-NAD; same default-to-explicit lesson as
// PR #26 / W26.)
//
// The Phase 2 plaintext-ack annotation is set ONLY on the plaintext path. Under
// mTLS (Phase 3c) the channel is TLS, swiftletd bypasses the ack gate in secured
// mode (KUBESWIFT_MIGRATION_MTLS=1), and emitting an "unsafe-plaintext: ack"
// annotation on a TLS-secured pod is misleading. So when mtlsEnabled we omit it.
// This is safe from version skew: mTLS only ever runs against the Phase 3c
// swiftletd (same release that added the secured-mode bypass), so no in-flight
// migration sees an old swiftletd that still requires the ack. The key is NOT
// deleted outright: the plaintext path still uses the ack gate as the
// THREAT-MODEL's "operator acknowledged cleartext" guard.
func mergeAnnotationsForDst(srcAnnotations map[string]string, mtlsEnabled bool, ovnDstAnnotations map[string]string) map[string]string {
	out := map[string]string{}
	if v, ok := srcAnnotations[swiftguest.MultusAnnotationKey]; ok && v != "" {
		out[swiftguest.MultusAnnotationKey] = v
	}
	if !mtlsEnabled {
		out[AnnotationMigrationPhase2Ack] = AnnotationMigrationPhase2AckValue
	}
	// OVN primary-on-NAD IP preservation. The backend that owns the guest's primary
	// network (kube-ovn today) computed the dst-pod identity annotations from the
	// guest — resolved by the migration controller via
	// swiftguest.OVNMigrationDstAnnotations before this clone, so the per-CNI
	// knowledge stays single-sourced in the swiftguest OVN backend, not here. For
	// kube-ovn that map is the LSP identity MAC + the guest's current IP pin +
	// kubevirt.io/migrationJobName (so its IPAM skips the conflict check and the dst
	// shares the src's still-held static IP across the cutover overlap). It is empty
	// for every non-OVN networking mode, so this merge is inert there. Offline
	// migration is unaffected (it rebuilds the pod via the SwiftGuest pod builder,
	// which re-stamps from scratch); the live path clones the src pod through here,
	// so this is the only place to carry it — same default-to-explicit lesson as the
	// Multus annotation above (W26 / PR #26).
	for k, v := range ovnDstAnnotations {
		out[k] = v
	}
	return out
}

// addReceiverEnvToLauncher mutates the pod's launcher container to
// include KUBESWIFT_MIGRATION_ROLE=receiver in its env. Returns an
// error if no container named "launcher" exists (which would mean
// the src pod is malformed).
func addReceiverEnvToLauncher(pod *corev1.Pod) error {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != LauncherContainerName {
			continue
		}
		// Replace any existing KUBESWIFT_MIGRATION_ROLE entry; src pod
		// shouldn't have one, but if a future src-side use ever sets
		// it, our value wins.
		filtered := c.Env[:0]
		for _, e := range c.Env {
			if e.Name != EnvKubeswiftMigrationRole {
				filtered = append(filtered, e)
			}
		}
		c.Env = append(filtered, corev1.EnvVar{
			Name:  EnvKubeswiftMigrationRole,
			Value: EnvKubeswiftMigrationRoleReceiver,
		})
		return nil
	}
	return fmt.Errorf("dst pod construction: src pod has no container named %q", LauncherContainerName)
}

// dstPodMatches returns true when an existing pod has the expected
// shape for our dst pod: matching labels (guest + migration-role +
// migration name) and an ownerRef pointing at the SwiftGuest. The
// idempotent re-entry path uses this to decide whether to skip
// Create on leader handover.
//
// Returns false (with no error) when any expected attribute differs.
// The caller surfaces the mismatch as phaseFailure with
// FailureReasonOther: a pre-existing pod with the dst's name but
// wrong shape indicates name collision with an unrelated workload,
// which we cannot safely overwrite.
func dstPodMatches(existing *corev1.Pod, mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest) bool {
	if existing.Labels[LabelGuestName] != guest.Name {
		return false
	}
	if existing.Labels[LabelMigrationRole] != MigrationRoleDestination {
		return false
	}
	if existing.Labels[LabelMigrationName] != mig.Name {
		return false
	}
	for _, owner := range existing.OwnerReferences {
		if owner.Kind == "SwiftGuest" && owner.Name == guest.Name && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

// dstPodReady returns true when the destination pod has reached
// Ready. Per design §2.3 Preparing exit condition:
// status.phase=Running AND Ready condition True.
func dstPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
