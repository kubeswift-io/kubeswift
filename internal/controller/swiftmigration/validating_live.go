package swiftmigration

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/migrationcert"
	"github.com/kubeswift-io/kubeswift/internal/migrationsidecar"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
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

	// Storage gate — the controller-side twin of the webhook's
	// gateLiveModeStorage, and the check EligibilityMismatch was defined for.
	// webhook.enabled defaults to false, so admission may never have run it: an
	// explicit mode=live on RWO or Filesystem storage would otherwise go on to
	// a destination pod that cannot attach the disk (Multi-Attach), or live-
	// migrate on storage the project declares unsafe. A missing class defers to
	// resolution, as the webhook does.
	if gate, _, err := r.liveStorageGate(ctx, &guest); err != nil {
		return phaseTransient(fmt.Errorf("check live-migration storage: %w", err))
	} else if gate != nil {
		return phaseFailure(gate.Error(), migrationv1alpha1.FailureReasonEligibilityMismatch)
	}

	// The rest of live eligibility, which auto-mode resolution and the
	// admission webhook (also off by default) apply: devices, backends and
	// networks that stay with the source node cannot follow the guest. An
	// explicit mode=live used to get past here and fail later.
	if guest.HasVFIODevices() {
		return phaseFailure(
			fmt.Sprintf("SwiftGuest %q has VFIO devices (gpuProfileRef or an SR-IOV interface); they cannot live-migrate. Use spec.mode=offline", guest.Name),
			migrationv1alpha1.FailureReasonEligibilityMismatch)
	}
	if guest.HasNodeLocalVirtioBackends() {
		return phaseFailure(
			fmt.Sprintf("SwiftGuest %q has node-local virtio backends (spec.filesystems, spec.vhostUserDevices, or a vhost-user interface); they cannot live-migrate. Use spec.mode=offline", guest.Name),
			migrationv1alpha1.FailureReasonEligibilityMismatch)
	}
	if modelA, err := resolved.NamespaceHasPrimaryUDN(ctx, r.Client, mig.Namespace); err != nil {
		return phaseTransient(fmt.Errorf("check primary-UDN namespace: %w", err))
	} else if modelA {
		return phaseFailure(
			fmt.Sprintf("SwiftGuest %q is on namespace %q's primary user-defined network, which cannot carry a live migration. Use spec.mode=offline", guest.Name, mig.Namespace),
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

	// A volume still attached to another node (the target of a failed
	// earlier attempt, or the source of a migration that has just
	// completed and is still detaching) would stall the attach to the
	// target (#692). Wait for it to go.
	if res := r.awaitVolumeDetach(ctx, status, &srcPod); res != nil {
		return res
	}

	// The destination pod is a copy of the source pod (newDstPod), images
	// included: a live migration keeps the guest on the launcher it was
	// started with, whatever the controller's launcher image is now (a chart
	// upgrade does not move running guests to a new launcher). So the source
	// pod must be one the controller built for this guest, and its images
	// must still be the ones it was created with: a pod's container and
	// init-container images can be patched after creation, and the copy
	// would run a patched image privileged on the destination node.
	if res := sourcePodCopyable(&srcPod, &guest); res != nil {
		return res
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
	if res := r.checkNodeCapacity(ctx, status, &node, &class, migrationv1alpha1.FailureReasonOther); res != nil {
		return res
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
		if sourceSidecarStale(&srcPod) {
			return phaseFailure(
				fmt.Sprintf("source pod %q has an mTLS sidecar that is still tunnelling to the destination of an earlier migration (%s) and cannot follow a new one; it was created by an earlier KubeSwift version. Restart the guest so it gets a new launcher pod, then migrate again (or use spec.mode=offline)",
					srcPod.Name, srcPod.Annotations[migrationsidecar.AnnotationDstPodIP]),
				migrationv1alpha1.FailureReasonSourceSidecarNotReady)
		}
		if status.SourceNode == "" || status.DestinationNode == "" {
			return phaseFailure(
				"migration mTLS enabled but a participating node name is empty; cannot resolve per-node identity",
				migrationv1alpha1.FailureReasonMigrationIdentityNotReady)
		}
		// Copy ONLY the destination node's identity into the guest namespace:
		// the destination pod's stunnel-server sidecar mounts it
		// (MigrationNodeSecretName(dstNode)). The source pod mounts the
		// per-guest Secret instead (populateSourceIdentity below), so the
		// source node's full cert+KEY does not need to sit in the tenant
		// namespace at all — copying it there only widened the exposure of a
		// node-wide private key. The source node's identity is still required
		// to exist: populateSourceIdentity errors if it is not provisioned.
		if err := migrationcert.EnsureMigrationIdentitySecret(ctx, r.Client, r.SystemNamespace, guest.Namespace, status.DestinationNode); err != nil {
			return phaseFailure(
				fmt.Sprintf("migration identity Secret for node %q not ready: %v", status.DestinationNode, err),
				migrationv1alpha1.FailureReasonMigrationIdentityNotReady)
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

// sourcePodCopyable is the gate in front of newDstPod's copy of the source
// pod: the pod must be controlled by the guest and keep the images it was
// created with. nil means the copy may proceed. Run at Validating (to fail
// early) and again in Preparing on the pod actually copied.
func sourcePodCopyable(srcPod *corev1.Pod, guest *swiftv1alpha1.SwiftGuest) *phaseResult {
	if !metav1.IsControlledBy(srcPod, guest) {
		return phaseFailure(
			fmt.Sprintf("source pod %q is not controlled by SwiftGuest %q; refusing to copy it to the destination", srcPod.Name, guest.Name),
			migrationv1alpha1.FailureReasonOther)
	}
	if pending, err := checkSourcePodImages(srcPod); err != nil {
		if pending {
			return phaseTransient(err)
		}
		return phaseFailure(err.Error(), migrationv1alpha1.FailureReasonImageTagMismatch)
	}
	return nil
}

// checkSourcePodImages verifies that the source pod's containers still run
// the images the controller created it with. The controller sets images only
// when it creates a launcher pod, but container and init-container images can
// be patched afterwards by anyone allowed to patch pods; the copy would run a
// patched image privileged on the destination node.
//
//   - metadata.generation (set by API servers from Kubernetes 1.33) is 1 at
//     creation and grows with every spec change, labels and annotations
//     excepted: above 1, the spec was changed.
//   - A launcher pod's restartPolicy is Never, so the kubelet restarts a
//     container only when its spec changed: a restart means a patched image
//     already ran.
//   - Without generation (older API servers), each image's repository must
//     be the one the kubelet reports. Only the repository: the CRI may report
//     another tag of the same image (containerd reports the image's first
//     tag), so a tag cannot be compared.
//
// pending is true when, without generation, a container has no reported
// image yet: retry rather than fail.
func checkSourcePodImages(pod *corev1.Pod) (pending bool, err error) {
	for _, s := range pod.Status.ContainerStatuses {
		if s.RestartCount > 0 {
			return false, fmt.Errorf(
				"source pod %q: container %q restarted %d time(s); a launcher container restarts only after its spec "+
					"was changed, and a live migration copies the spec", pod.Name, s.Name, s.RestartCount)
		}
	}
	if pod.Generation > 1 {
		return false, fmt.Errorf(
			"source pod %q: its spec was changed after it was created (generation %d); a live migration copies the spec",
			pod.Name, pod.Generation)
	}
	if pod.Generation == 1 {
		return false, nil
	}
	check := func(containers []corev1.Container, statuses []corev1.ContainerStatus) (bool, error) {
		ran := make(map[string]string, len(statuses))
		for _, s := range statuses {
			ran[s.Name] = s.Image
		}
		for _, c := range containers {
			image := ran[c.Name]
			if image == "" {
				return true, fmt.Errorf("source pod %q: container %q has no reported image yet", pod.Name, c.Name)
			}
			if imageRepository(c.Image) != imageRepository(image) {
				return false, fmt.Errorf(
					"source pod %q: container %q spec image %q is not from the repository it was started from (%q); "+
						"the pod's spec was changed after it was created, and a live migration copies it",
					pod.Name, c.Name, c.Image, image)
			}
		}
		return false, nil
	}
	if pending, err := check(pod.Spec.InitContainers, pod.Status.InitContainerStatuses); err != nil {
		return pending, err
	}
	return check(pod.Spec.Containers, pod.Status.ContainerStatuses)
}

// imageRepository returns an image reference's repository, without tag or
// digest, with a Docker Hub short name expanded as the CRI reports it
// ("busybox:1.36" -> "docker.io/library/busybox").
func imageRepository(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		ref = ref[:i]
	}
	if i := strings.IndexByte(ref, '/'); i < 0 {
		return "docker.io/library/" + ref
	} else if first := ref[:i]; !strings.ContainsAny(first, ".:") && first != "localhost" {
		return "docker.io/" + ref
	}
	return ref
}
