package swiftmigration

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/migrationcert"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
)

// handleValidatingLive is the live-mode Validating phase.
//
// Responsibilities (B2 scope):
//
//   - Re-resolve source SwiftGuest + class (defense in depth — webhook
//     ran at admission, but a SwiftGuest mutation between admission
//     and our reconcile could flip migration.enabled=false).
//   - Stamp status.Mode=live, SourceNode, DestinationNode.
//   - Stamp status.SourcePodUID for F4.2 pod-replacement detection.
//     Read directly from the source pod's metadata (canonicalPodName
//     resolves to source pod for pre-cutover phases).
//   - Verify target node is Ready + has headroom (reuse Phase 1's
//     checkNodeCapacity helper).
//   - CPU pre-flight: SILENT SUCCESS per design §7.3. Phase 3b will
//     add a real CPU-feature compatibility check at admission time;
//     Phase 3a accepts the operator-burden of selecting a compatible
//     destination node manually. No code path here; this comment
//     captures the deliberate non-action.
//   - Set Compatible=True, advance to Preparing.
//
// Per-source-node concurrency check is enforced at the webhook
// (validator.go's checkPerSourceNodeConcurrency) at admission time.
// The controller does not re-run it here; race-window analysis: a
// concurrent live SwiftMigration on the same source node would have
// to be admitted between this admission and our reconcile, which
// requires both to be admitted by the webhook simultaneously. The
// webhook serializes per-API-server-cache reads of the
// SwiftMigrationList, providing the same race-safety as a controller-
// side re-check would. If a follow-up surfaces a real concurrency
// gap, the helper exists in the webhook package for reuse.
//
// Defensive guard: assert isLiveMode at entry per architect-discipline
// review answer to Q1.
func (r *SwiftMigrationReconciler) handleValidatingLive(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	if !isLiveMode(mig, status) {
		return phaseFailure(
			"internal: handleValidatingLive invoked without live mode",
			migrationv1alpha1.FailureReasonOther,
		)
	}

	// Re-resolve source SwiftGuest in same namespace.
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return phaseFailure(
				fmt.Sprintf("source SwiftGuest %q no longer exists in namespace %q", mig.Spec.GuestRef.Name, mig.Namespace),
				migrationv1alpha1.FailureReasonOther)
		}
		return phaseTransient(fmt.Errorf("get source SwiftGuest: %w", err))
	}

	// Defense in depth: re-check migration.enabled (webhook caught at
	// submission, but a mutation between admission and now could flip
	// it).
	if guest.Spec.Migration != nil && guest.Spec.Migration.Enabled != nil && !*guest.Spec.Migration.Enabled {
		return phaseFailure(
			fmt.Sprintf("SwiftGuest %q has spec.migration.enabled=false", guest.Name),
			migrationv1alpha1.FailureReasonEligibilityMismatch)
	}

	// Stamp status.Mode + SourceNode + DestinationNode + SourcePodUID.
	// Mode may already be "live" (B1 dispatch path) or "" + spec=live;
	// either way we set it explicitly here for clarity.
	status.Mode = migrationv1alpha1.SwiftMigrationModeLive
	status.SourceNode = guest.Status.NodeName
	status.DestinationNode = mig.Spec.Target.NodeName

	// F4.2: stamp the source pod's UID so Preparing/StopAndCopy can
	// detect pod replacement (eviction + recreate by an external actor).
	// shouldCheckSourcePodUID gates the actual comparison; this is just
	// the baseline capture.
	var srcPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: canonicalPodNameForGuest(&guest), Namespace: guest.Namespace}, &srcPod); err != nil {
		if apierrors.IsNotFound(err) {
			return phaseFailure(
				fmt.Sprintf("source SwiftGuest %q has no pod (status.podRef=%v); cannot live-migrate a non-running guest", guest.Name, guest.Status.PodRef),
				migrationv1alpha1.FailureReasonOther)
		}
		return phaseTransient(fmt.Errorf("get source pod: %w", err))
	}
	status.SourcePodUID = srcPod.UID
	// W26: lock in the src pod NAME at Validating time, mirroring the
	// existing SourcePodUID lock-in. Pre-W26, downstream sites resolved
	// the src pod by either guest.Name (W15 fix in cutover.go +
	// stopandcopy_live.go) or canonicalPodName (preparing_live.go). Both
	// derive from current cluster state and are wrong for back-to-back
	// migrations: after a prior migration's cutover, guest.Status.PodRef
	// .Name points at the prior dst pod (= this migration's src), which
	// is NOT guest.Name. Locking the name in here makes src-pod
	// resolution race-immune AND chain-safe; downstream sites use
	// status.SourcePodRef.Name consistently regardless of cluster state
	// drift mid-migration.
	status.SourcePodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: srcPod.Name}

	// LBA-1 defensive image-tag-match trip-wire.
	//
	// newDstPod (dst_pod.go) constructs the destination pod via
	// srcPod.DeepCopy() which clones the source pod's launcher
	// container image atomically. This guarantees match-tag migration
	// as a structural property; the webhook does not need a
	// version-skew rule because the implementation enforces it.
	//
	// This check is a fail-loud trip-wire: it fires only if a future
	// refactor regresses the clone-src behavior or if the deployed
	// controller's default launcher image diverges from the running
	// guest's image (e.g., partial rolling upgrade mid-fleet). The
	// common path always passes. If it ever fires, the migration
	// enters Failed with ImageTagMismatch reason code, pointing
	// operators at LBA-1.
	if err := checkImageTagMatch(&srcPod); err != nil {
		return phaseFailure(err.Error(), migrationv1alpha1.FailureReasonImageTagMismatch)
	}

	// IPWillChange surfacing (same logic as offline path; live mode
	// on default networking still loses the IP because Cloud Hypervisor
	// receives a fresh DHCP lease on the destination's br0).
	if mig.Spec.AllowIPChange && isDefaultNodeLocalNetworking(&guest) && status.SourceNode != "" && status.SourceNode != status.DestinationNode {
		setCondition(status, migrationv1alpha1.SwiftMigrationConditionIPWillChange,
			metav1.ConditionTrue, ReasonIPWillChange,
			fmt.Sprintf("guest %q is on default node-local networking; will receive a fresh IP on %q", guest.Name, status.DestinationNode))
	}

	// Resolve target node + capacity check.
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.Target.NodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return phaseFailure(
				fmt.Sprintf("target node %q no longer exists in cluster", mig.Spec.Target.NodeName),
				migrationv1alpha1.FailureReasonOther)
		}
		return phaseTransient(fmt.Errorf("get target node: %w", err))
	}
	if node.Spec.Unschedulable {
		return phaseFailure(
			fmt.Sprintf("target node %q is cordoned (spec.unschedulable=true)", mig.Spec.Target.NodeName),
			migrationv1alpha1.FailureReasonOther)
	}
	if !nodeReady(&node) {
		return phaseFailure(
			fmt.Sprintf("target node %q is not Ready", mig.Spec.Target.NodeName),
			migrationv1alpha1.FailureReasonOther)
	}

	// Resolve SwiftGuestClass for resource requirements.
	var class swiftv1alpha1.SwiftGuestClass
	if err := r.Get(ctx, client.ObjectKey{Name: guest.Spec.GuestClassRef.Name}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return phaseFailure(
				fmt.Sprintf("SwiftGuestClass %q referenced by guest %q not found", guest.Spec.GuestClassRef.Name, guest.Name),
				migrationv1alpha1.FailureReasonOther)
		}
		return phaseTransient(fmt.Errorf("get SwiftGuestClass: %w", err))
	}
	if err := r.checkNodeCapacity(ctx, &node, &class); err != nil {
		return phaseFailure(err.Error(), migrationv1alpha1.FailureReasonOther)
	}

	// CPU pre-flight: silent success per design §7.3.

	// Phase 3c (Option B) mTLS precondition (§4.4). When live-migration
	// mTLS is enabled, BOTH participating nodes' per-node identity
	// Secrets must already be provisioned (cert-manager precondition) and
	// are distributed into this guest namespace so the launcher pods can
	// mount them. This is DISTRIBUTION, not issuance — no cert-manager
	// call sits on the migration path; EnsureMigrationIdentitySecret
	// copies an already-issued Secret and returns an error when the
	// source Secret is absent. Failing fast here keeps a missing/expired
	// node identity from ever reaching the cutover window. The dst
	// sidecar mounts the dst-node Secret (PR 3); the src sidecar mounts
	// the src-node Secret (PR 3b) — both are ensured up front so PR 3b
	// needs no Validating-phase change.
	if r.MigrationMTLSEnabled {
		// Fail fast if the source pod can't act as the TLS client: a pod
		// that predates mTLS enablement (no sidecar) or a post-cutover
		// destination pod (server-role sidecar) would otherwise retry the
		// send for ~60s and then time out. A clear admission-time failure
		// with a recycle hint is far better.
		if !sourcePodMTLSReady(&srcPod) {
			return phaseFailure(
				fmt.Sprintf("source pod %q is not mTLS-source-ready (no client-role migration-stunnel sidecar); it predates mTLS enablement or is a post-cutover destination pod — recycle the guest's pod before live-migrating with mTLS", srcPod.Name),
				migrationv1alpha1.FailureReasonSourceSidecarNotReady)
		}
		for _, n := range []string{status.SourceNode, status.DestinationNode} {
			if n == "" {
				return phaseFailure(
					"migration mTLS enabled but a participating node name is empty; cannot resolve per-node identity",
					migrationv1alpha1.FailureReasonMigrationIdentityNotReady)
			}
			if err := migrationcert.EnsureMigrationIdentitySecret(ctx, r.Client, r.SystemNamespace, guest.Namespace, n); err != nil {
				return phaseFailure(
					fmt.Sprintf("migration identity Secret for node %q not ready: %v", n, err),
					migrationv1alpha1.FailureReasonMigrationIdentityNotReady)
			}
		}
		// PR 3d: populate the per-guest identity Secret the SOURCE sidecar
		// mounts with the source node's issued identity, so the idle client
		// sidecar (PR 3b) has its cert/key/ca well before the send. Done
		// here (early) to give kubelet time to propagate the Secret into the
		// mounted volume before StopAndCopy.
		if err := r.populateSourceIdentity(ctx, &guest, status.SourceNode); err != nil {
			return phaseFailure(
				fmt.Sprintf("populate source migration identity: %v", err),
				migrationv1alpha1.FailureReasonMigrationIdentityNotReady)
		}
		// PR 4 audit event (§6.3): record that this migration's channel is
		// mTLS-secured and which per-node identities are pinned, so an
		// operator can see WHICH identity the channel will authenticate.
		// The handshake OUTCOME surfaces later as the Completed event
		// (success) or the failure event's TLS-related detail.
		if r.Recorder != nil {
			r.Recorder.Eventf(mig, corev1.EventTypeNormal, "MTLSChannel",
				"live-migration channel secured with mTLS; pinned peers src=%q dst=%q (SAN-pinned per-node identities)",
				status.SourceNode, status.DestinationNode)
		}
	}

	// Compatible=True, advance to Preparing.
	setCondition(status, migrationv1alpha1.SwiftMigrationConditionCompatible,
		metav1.ConditionTrue, ReasonValidating, "validation passed; target node has capacity (live mode)")
	setReadyCondition(status, metav1.ConditionFalse, ReasonPreparing, "preparing destination launcher pod")
	setPhase(status, migrationv1alpha1.SwiftMigrationPhasePreparing)
	setPhaseDetail(status, "preparing destination launcher pod")
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeNormal, "Validated",
			fmt.Sprintf("validation passed; live-migrating %q from %q to %q", guest.Name, status.SourceNode, status.DestinationNode))
	}
	return phaseAdvance()
}

// canonicalPodNameForGuest returns the pod name to look up for the
// given SwiftGuest. Pre-cutover (Validating + Preparing for live mode,
// all phases for offline mode), this is guest.Name unchanged. Post-
// cutover for live mode, status.PodRef.Name on the SwiftGuest will
// have been set to the destination pod name; this helper returns
// that value.
//
// Mirrors the swiftguest controller's canonicalPodName helper. This
// duplicate exists because that helper is package-private (unexported),
// NOT because of an import cycle (TFU #20): this package already imports
// internal/controller/swiftguest (see the import block above), and the
// swiftguest controller does not import the swiftmigration controller.
// A future cleanup could export the swiftguest helper (or move it to a
// shared package) and drop this copy.
func canonicalPodNameForGuest(guest *swiftv1alpha1.SwiftGuest) string {
	if guest.Status.PodRef != nil && guest.Status.PodRef.Name != "" {
		return guest.Status.PodRef.Name
	}
	return guest.Name
}

// srcPodLookupName returns the launcher pod name to use for src pod
// lookups in live-mode Preparing / StopAndCopy / cutover phases. Uses
// the SwiftMigration's status.SourcePodRef.Name when set (locked in at
// Validating-live), falling back to canonicalPodNameForGuest for
// pre-Validating reconciles or recovery cases where the field is
// somehow unset.
//
// Why locked-in beats current-cluster-state derivation: between the
// time Validating captures the src pod identity and the time cutover
// patches guest.Status.PodRef.Name to the dst pod, the cluster-state
// derivation diverges from the original src pod. Two separate
// failure modes both blocked by locking in:
//
//   - W15 race: cutoverStep1 patches SwiftGuest podRef before the
//     SwiftMigration status persists; informer-driven race-reconcile
//     reads (phaseDetail=LiveSrcCompleted, podRef=dstName,
//     PodRefSwapped=False). Without locking, canonicalPodName resolves
//     to dst pod and false-fires the F4.2 UID check.
//   - W26 chain: after a prior migration's cutover, status.PodRef.Name
//     points at the prior dst pod (= this migration's src). Without
//     locking, literal guest.Name lookup misses (NotFound) and
//     false-fires SourcePodReplaced; canonicalPodName resolves to the
//     prior dst pod which IS the src for this migration but in
//     cutover.go's executeCutover would post-step1 resolve to THIS
//     migration's dst pod and cutoverStep2 would delete the migrated
//     guest. Locking eliminates both modes uniformly.
func srcPodLookupName(mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest) string {
	if mig.Status.SourcePodRef != nil && mig.Status.SourcePodRef.Name != "" {
		return mig.Status.SourcePodRef.Name
	}
	return canonicalPodNameForGuest(guest)
}

// checkImageTagMatch is the LBA-1 defensive trip-wire. Returns nil
// when the src pod's launcher container image matches the controller's
// default launcher image OR when either value is empty (defensive
// skip — the trip-wire is not load-bearing for correctness).
//
// See handleValidatingLive's wire site for the rationale.
func checkImageTagMatch(srcPod *corev1.Pod) error {
	expected := swiftguest.LauncherImage()
	actual := launcherContainerImage(srcPod)
	if expected == "" || actual == "" {
		// Missing config; not load-bearing. Common path skips.
		return nil
	}
	if actual != expected {
		return fmt.Errorf(
			"image tag mismatch: source pod uses %q, controller default is %q "+
				"(LBA-1 trip-wire)",
			actual, expected,
		)
	}
	return nil
}

// launcherContainerImage returns the image of the pod's launcher
// container, or "" if no container matches LauncherContainerName.
func launcherContainerImage(pod *corev1.Pod) string {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == LauncherContainerName {
			return pod.Spec.Containers[i].Image
		}
	}
	return ""
}
