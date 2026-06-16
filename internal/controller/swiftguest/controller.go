package swiftguest

import (
	"context"
	"errors"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/imageref"
	"github.com/projectbeskar/kubeswift/internal/metrics"
	"github.com/projectbeskar/kubeswift/internal/resolved"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
	"github.com/projectbeskar/kubeswift/internal/seed"
)

// gpuHypervisorAnnotation overrides hypervisor selection for manual QEMU testing
// without GPU hardware. The annotation value is used as-is ("qemu" or "cloud-hypervisor").
// Only read by the controller; never written.
const gpuHypervisorAnnotation = "kubeswift.io/hypervisor-override"

const (
	SeedConfigMapSuffix = "-seed"

	// defaultNetworkConfig is used when SwiftSeedProfile has no networkData.
	// Matches both predictable naming (en*: ens3, enp0s3, eno1) and legacy (eth*: eth0).
	// Ubuntu/Debian use en*; Rocky/RHEL may use eth0 when net.ifnames=0.
	// Top-level "network:" is required by cloud-init/netplan.
	defaultNetworkConfig = `network:
  version: 2
  ethernets:
    predictable:
      match:
        name: en*
      dhcp4: true
    legacy:
      match:
        name: eth*
      dhcp4: true
`
)

// SwiftGuestReconciler reconciles SwiftGuest resources.
type SwiftGuestReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// MigrationMTLSEnabled mirrors the controller-manager's
	// --migration-mtls-enabled flag (Phase 3c, Option B). When true, every
	// migration-eligible launcher pod is born with an idle source-side
	// stunnel client sidecar (plus its downward-API input volume and a
	// per-guest identity Secret), so that a future live migration has the
	// TLS client already in place inside the immutable, already-running
	// source pod. When false (default) the launcher pod is byte-for-byte
	// unchanged from Phase 3a/3b.
	MigrationMTLSEnabled bool

	// SystemNamespace is the controller-manager's own namespace, where the
	// migration CA Issuer, the per-node identity Secrets, and the
	// kubeswift-migration-stunnel ConfigMap live. Used to copy the stunnel
	// ConfigMap into guest namespaces. Only consulted when
	// MigrationMTLSEnabled.
	SystemNamespace string

	// SnapshotS3Image is the snapshot-s3 downloader image used by a
	// cloneFromSnapshot guest cloning from a Tier C (s3) snapshot — the
	// per-guest download Job pulls the artifacts into the target node's cache
	// before the restore-receive launcher boots. Wired from
	// KUBESWIFT_SNAPSHOT_S3_IMAGE.
	SnapshotS3Image string
}

// Reconcile implements the reconcile loop.
func (r *SwiftGuestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, req.NamespacedName, &guest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Per-namespace RBAC bootstrap: idempotently ensure the
	// `swiftletd-reporter` RoleBinding exists in this SwiftGuest's
	// namespace, so the launcher pod's `default` ServiceAccount has
	// pods/get,patch + swiftguests/status:patch. Replaces the prior
	// operator-applied RoleBinding pattern (snapshot walkthrough
	// finding F2 + Phase 2 walkthrough finding W3). Runs early so we
	// don't waste work on subsequent steps if the RBAC bootstrap
	// fails — without the binding, the launcher pod would boot but
	// silently fail every annotation write.
	if err := EnsureSwiftletdRBAC(ctx, r.Client, guest.Namespace); err != nil {
		logger.Error(err, "failed to ensure swiftletd RBAC", "namespace", guest.Namespace)
		return ctrl.Result{}, err
	}

	// cloneFromSnapshot (Snapshot Phase 4): a guest that boots as a clone of a
	// SwiftSnapshot has no imageRef/kernelRef. Resolve the snapshot + the live
	// source guest, self-stamp the clone-mode restore annotations, and resolve
	// using the SOURCE guest's spec (the "effective" guest). Everything after
	// resolution (seed/intent/rootdisk/pod) runs unchanged off the now-stamped
	// restore annotations + the source-resolved rg.
	guestForResolve := &guest
	if guest.UsesCloneFromSnapshot() {
		effective, failReason, requeue, perr := r.prepareCloneFromSnapshot(ctx, &guest)
		if perr != nil {
			return ctrl.Result{}, perr
		}
		if failReason != "" {
			status := guest.Status.DeepCopy()
			SetResolvedCondition(status, false, failReason)
			status.Phase = swiftv1alpha1.SwiftGuestPhaseFailed
			recordGuestMetrics(&guest, &guest.Status, status, nil)
			if err := r.patchStatus(ctx, &guest, status); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("cloneFromSnapshot preparation failed", "reason", failReason)
			return ctrl.Result{}, nil
		}
		if requeue {
			status := guest.Status.DeepCopy()
			status.Phase = swiftv1alpha1.SwiftGuestPhasePending
			SetResolvedCondition(status, false, "waiting for SwiftSnapshot "+guest.Spec.CloneFromSnapshot.SnapshotRef.Name+" to be Ready")
			if err := r.patchStatus(ctx, &guest, status); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		guestForResolve = effective
	}

	res := resolved.NewResolver(r.Client)
	rg, err := res.Resolve(ctx, guestForResolve)
	if err != nil {
		var re *resolved.ResolutionError
		if errors.As(err, &re) {
			// Set Resolved=False, phase=Failed; do not create pod
			status := guest.Status.DeepCopy()
			SetResolvedCondition(status, false, re.Reason)
			status.Phase = swiftv1alpha1.SwiftGuestPhaseFailed
			recordGuestMetrics(&guest, &guest.Status, status, nil)
			if err := r.patchStatus(ctx, &guest, status); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("resolution failed", "reason", re.Reason, "resource", re.AffectedResource)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Set Resolved=True
	status := guest.Status.DeepCopy()
	SetResolvedCondition(status, true, "")

	// Echo the resolved storage spec to status for operator visibility.
	// Pure informational mirror; the SwiftMigration webhook recomputes
	// liveMigrationCapable from the resolved spec rather than reading
	// status (avoids write-back race during cluster restore).
	EchoResolvedStorage(status, rg.Storage.AccessMode, rg.Storage.VolumeMode, rg.Storage.StorageClassName)

	// Per-driver storage pre-flight: surface a StorageReady=False
	// condition when the resolved spec is RWX+Block but the chosen
	// StorageClass is a Longhorn class missing parameters.migratable.
	// Best-effort and informational — the check is a status condition,
	// NOT an admission gate, because StorageClasses are cluster-admin
	// resources and can be fixed without restarting the guest.
	if reason, msg, ok := r.checkStorageReady(ctx, rg); ok {
		SetStorageReadyCondition(status, true, "", "")
	} else {
		SetStorageReadyCondition(status, false, reason, msg)
	}

	// Seed rendering: when ResolvedGuest has Seed, render and create ConfigMap
	seedConfigMapName := ""
	if rg.HasSeed() {
		userData, metaData, networkData, err := seed.Render(ctx, r.Client, guest.Namespace, rg.Seed)
		if err != nil {
			logger.Error(err, "seed render failed")
			return ctrl.Result{}, err
		}
		if networkData == "" {
			networkData = defaultNetworkConfig
		}
		seedConfigMapName = guest.Name + SeedConfigMapSuffix
		desiredCM := seed.BuildConfigMap(seedConfigMapName, guest.Namespace, userData, metaData, networkData)
		if err := controllerutil.SetControllerReference(&guest, desiredCM, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		var existingCM corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: seedConfigMapName}, &existingCM); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
			if err := r.Create(ctx, desiredCM); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			if !equality.Semantic.DeepEqual(existingCM.Data, desiredCM.Data) ||
				!equality.Semantic.DeepEqual(existingCM.OwnerReferences, desiredCM.OwnerReferences) {
				existingCM.Data = desiredCM.Data
				existingCM.OwnerReferences = desiredCM.OwnerReferences
				if err := r.Update(ctx, &existingCM); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
	}

	// Set hypervisor from GPU allocation status when gpuProfileRef is set.
	// The annotation override (kubeswift.io/hypervisor-override) takes precedence
	// for manual QEMU testing without real GPU hardware.
	if guest.Spec.GPUProfileRef != nil && guest.Status.GPU != nil {
		rg.Hypervisor = guest.Status.GPU.Hypervisor
	}
	// DRA backend: status.GPU is not populated until after scheduling, so the
	// hypervisor comes straight from the claim spec's tier (same mapping).
	if rc := guest.Spec.GPUResourceClaim; rc != nil {
		rg.Hypervisor = "cloud-hypervisor"
		if rc.Tier == "hgx-shared" || rc.Tier == "hgx-full" {
			rg.Hypervisor = "qemu"
		}
	}
	if override, ok := guest.Annotations[gpuHypervisorAnnotation]; ok && override != "" {
		rg.Hypervisor = override
	}

	// Create or update intent ConfigMap
	intentConfigMapName := guest.Name + IntentConfigMapSuffix
	intent := runtimeintent.Build(rg)

	// Attach GPU intent when GPUs have been allocated (Phase 3+).
	if guest.Spec.GPUProfileRef != nil && isGPUAllocated(&guest) {
		gpuIntent, err := r.buildGPUIntent(ctx, &guest)
		if err != nil {
			logger.Error(err, "failed to build GPU intent")
			return ctrl.Result{}, err
		}
		intent.GPU = gpuIntent
	}
	// DRA backend: the intent is written BEFORE allocation, so it carries no
	// devices — deviceSource: "env" tells swiftletd to synthesize them from
	// the CDI-injected GPU_PCI_ADDRESSES env (design doc §A2).
	if rc := guest.Spec.GPUResourceClaim; rc != nil {
		intent.GPU = buildDRAGPUIntent(rc)
	}
	// Tier B restore: when the SwiftGuest is marked as the target of
	// an active SwiftRestore, the intent points swiftletd at the
	// in-pod snapshot path (read-only mount for in-place, staged copy
	// for clone). swiftletd reads `restore.snapshotPath`, builds
	// `--restore source_url=file://<path>/`, and skips the normal
	// CH/QEMU spawn. See rust/swiftletd/src/launch.rs run_ch_restore.
	if params, ok := RestoreParamsFromAnnotations(guest.Annotations); ok {
		intent.Restore = &runtimeintent.RestoreIntent{
			SnapshotPath:      params.InPodSnapshotPath(),
			AutoResume:        params.AutoResume,
			MemoryRestoreMode: params.MemoryRestoreMode,
		}
	}
	intentJSON, err := runtimeintent.Serialize(intent)
	if err != nil {
		logger.Error(err, "failed to serialize runtime intent")
		return ctrl.Result{}, err
	}
	desiredIntentCM := &corev1.ConfigMap{
		ObjectMeta: ctrl.ObjectMeta{
			Name:      intentConfigMapName,
			Namespace: guest.Namespace,
		},
		Data: map[string]string{
			IntentFile: string(intentJSON),
		},
	}
	if err := controllerutil.SetControllerReference(&guest, desiredIntentCM, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	var existingIntentCM corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: intentConfigMapName}, &existingIntentCM); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, desiredIntentCM); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		if !equality.Semantic.DeepEqual(existingIntentCM.Data, desiredIntentCM.Data) {
			existingIntentCM.Data = desiredIntentCM.Data
			if err := r.Update(ctx, &existingIntentCM); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if rg.GetLifecycle() == "stop" {
		// Guest is intentionally stopped. If a pod exists and is completed or doesn't exist,
		// update status to Stopped and return without recreating the pod.
		var existingPod corev1.Pod
		podErr := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: canonicalPodName(&guest)}, &existingPod)
		if podErr != nil && client.IgnoreNotFound(podErr) != nil {
			return ctrl.Result{}, podErr
		}
		podGone := apierrors.IsNotFound(podErr)
		podDone := !podGone && (existingPod.Status.Phase == corev1.PodSucceeded || existingPod.Status.Phase == corev1.PodFailed)
		if podGone || podDone {
			status.Phase = swiftv1alpha1.SwiftGuestPhaseStopped
			var podForStopped *corev1.Pod
			if !podGone {
				podForStopped = &existingPod
			}
			recordGuestMetrics(&guest, &guest.Status, status, podForStopped)
			if err := r.patchStatus(ctx, &guest, status); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// This block fires on launcher-pod TERMINAL states (Failed/Succeeded), i.e.
	// when Cloud Hypervisor exits. A guest reboot is NOT terminal on CH v52 (it
	// resets the VM in place — the pod stays Running, validated 2026-06-09), so
	// reboots never reach here. CH exits only on guest shutdown/poweroff (pod
	// Succeeded) or a crash (pod Failed); those are what RestartOnFailure/Always
	// act on.
	if guest.Spec.RunPolicy == swiftv1alpha1.RunPolicyRestartOnFailure ||
		guest.Spec.RunPolicy == swiftv1alpha1.RunPolicyAlways {

		var existingPod corev1.Pod
		podErr := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: canonicalPodName(&guest)}, &existingPod)
		if podErr != nil && !apierrors.IsNotFound(podErr) {
			return ctrl.Result{}, podErr
		}
		podGone := apierrors.IsNotFound(podErr)

		if !podGone {
			shouldRestart := false
			if existingPod.Status.Phase == corev1.PodFailed {
				shouldRestart = true
			}
			if guest.Spec.RunPolicy == swiftv1alpha1.RunPolicyAlways &&
				existingPod.Status.Phase == corev1.PodSucceeded {
				shouldRestart = true
			}

			if shouldRestart {
				// Compute backoff: 10s * 2^restartCount, capped at 300s
				backoff := time.Duration(10) * time.Second
				for i := 0; i < int(guest.Status.RestartCount); i++ {
					backoff *= 2
					if backoff > 300*time.Second {
						backoff = 300 * time.Second
						break
					}
				}

				// Check if enough time has passed since last restart
				if guest.Status.LastRestartTime != nil {
					elapsed := time.Since(guest.Status.LastRestartTime.Time)
					if elapsed < backoff {
						remaining := backoff - elapsed
						if err := r.patchStatus(ctx, &guest, status); err != nil {
							return ctrl.Result{}, err
						}
						return ctrl.Result{RequeueAfter: remaining}, nil
					}
				}

				// Delete the pod so controller recreates it on next reconcile
				if err := r.Delete(ctx, &existingPod); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}

				// Update restart tracking
				now := metav1.Now()
				status.RestartCount++
				status.LastRestartTime = &now
				status.Phase = swiftv1alpha1.SwiftGuestPhaseScheduling
				recordGuestMetrics(&guest, &guest.Status, status, &existingPod)
				if err := r.patchStatus(ctx, &guest, status); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
		}
	}

	// GPU gate (native backend): if gpuProfileRef is set, wait for
	// GPUAllocated=True before creating the launcher pod. The SwiftGPU controller
	// runs independently and sets this condition once devices are reserved on a
	// SwiftGPUNode.
	if guest.Spec.GPUProfileRef != nil && !isGPUAllocated(&guest) {
		logger.Info("waiting for GPU allocation", "guest", req.NamespacedName)
		status.Phase = swiftv1alpha1.SwiftGuestPhasePending
		if err := r.patchStatus(ctx, &guest, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// GPU (DRA backend): unlike the native gate above, a DRA guest does NOT
	// wait for GPUAllocated — allocation happens AT pod-schedule time (the pod
	// carries the ResourceClaim; the scheduler + DRA driver decide). The pod is
	// built immediately, claim-bearing and unpinned; the device identity
	// reaches gpu-init/swiftletd via the driver's CDI containerEdits, and the
	// SwiftGPU controller's Resolve stamps status.GPU after scheduling
	// (status-only, not load-bearing for the runtime). Design doc §A2/§A6.

	// For disk boot, ensure per-guest root disk clone exists and is ready.
	var rootDiskClone *RootDiskCloneResult
	if rg.PreparedImage.PVCName != "" && !rg.HasKernel() {
		res, err := r.EnsureRootDiskClone(ctx, &guest, rg)
		if err != nil {
			// Clone not ready — requeue
			status.Phase = swiftv1alpha1.SwiftGuestPhaseScheduling
			if patchErr := r.patchStatus(ctx, &guest, status); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		rootDiskClone = res
		// Override the PVC name to use the clone instead of the shared image PVC.
		rg.PreparedImage.PVCName = res.PVCName
	}

	// Blank data disks: provision the guest-owned PVCs (and fill Filesystem
	// ones) and gate the pod on them, the same way the root disk gates. A
	// guest must never boot with a missing data disk (Principle #6) — hold it
	// in Scheduling, with DataDisksReady=False naming the blocker, until ready.
	// Works on every boot path (disk, kernel, GPU), so it runs unconditionally.
	if err := r.EnsureBlankDataDisks(ctx, &guest, rg); err != nil {
		status.Phase = swiftv1alpha1.SwiftGuestPhaseScheduling
		SetDataDisksReadyCondition(status, false, "DataDisksProvisioning", err.Error())
		if patchErr := r.patchStatus(ctx, &guest, status); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if rg.HasDataDisks() {
		SetDataDisksReadyCondition(status, true, "", "")
		status.DataDisks = r.dataDiskStatuses(ctx, &guest, rg)
	}

	// Phase 3c (Option B): when mTLS is enabled, the launcher pod for a
	// migration-eligible guest mounts the stunnel-config ConfigMap and a
	// per-guest identity Secret (see applyMigrationSourceSidecar). Ensure
	// both exist in the guest namespace BEFORE building/creating the pod —
	// a pod that references a missing ConfigMap/Secret never starts. These
	// are idempotent and never clobber a migration-populated identity.
	if r.MigrationMTLSEnabled && migrationEligible(&guest) {
		if err := EnsureMigrationStunnelConfig(ctx, r.Client, r.SystemNamespace, guest.Namespace); err != nil {
			logger.Error(err, "failed to ensure migration stunnel ConfigMap")
			return ctrl.Result{}, err
		}
		if err := EnsurePerGuestMigrationIdentitySecret(ctx, r.Client, r.Scheme, &guest); err != nil {
			logger.Error(err, "failed to ensure per-guest migration identity Secret")
			return ctrl.Result{}, err
		}
	}

	// Build and create/update pod
	desiredPod, err := r.buildPod(ctx, &guest, rg, seedConfigMapName, intentConfigMapName, rootDiskClone)
	if err != nil {
		logger.Error(err, "failed to build pod spec")
		return ctrl.Result{}, err
	}
	if err := controllerutil.SetControllerReference(&guest, desiredPod, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	var existingPod corev1.Pod
	var podForMetrics *corev1.Pod
	var cloneIdentityRequeue time.Duration
	if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: canonicalPodName(&guest)}, &existingPod); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		// Self-heal a stale migration PodRef before creating the pod.
		// If status.PodRef points at a <guest>-mig-<uid> pod from a prior
		// live migration that no longer exists, clear it so the next
		// reconcile's canonicalPodName falls back to guest.Name (the name
		// we create below) and finds the pod — otherwise the lookup keeps
		// resolving to the deleted name and this Create loops on
		// AlreadyExists (TFU #18 secondary trap). See staleMigrationPodRef.
		if staleMigrationPodRef(&guest) {
			status.PodRef = nil
		}
		// Tolerate AlreadyExists. During the stale-PodRef self-heal above,
		// the lookup name (canonicalPodName) and the create name
		// (guest.Name) diverge, so a concurrent reconcile reading the
		// pre-clear guest can race us to create the guest.Name pod. The
		// pod existing is the desired outcome; the next reconcile (PodRef
		// now cleared) resolves it via canonicalPodName and maps status.
		// Without this, every offline-after-live migration logs a spurious
		// ERROR-level "already exists" line even though it succeeds.
		if err := r.Create(ctx, desiredPod); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		// Pod just created; set initial status (Pending/Scheduling)
		status.Phase = swiftv1alpha1.SwiftGuestPhaseScheduling
		SetPodScheduledCondition(status, nil, false, "Scheduling")
	} else {
		// Pod exists; update status from pod
		podForMetrics = &existingPod
		MapPodToStatus(&existingPod, status)
		// In-guest identity agent (PR 4): for an agent-enabled cloneFromSnapshot
		// clone, drive the one-shot identity-regen over vsock once it is Running.
		// MUST run BEFORE the IP-not-discovered requeue below: a fresh clone has
		// no IP precisely because the agent has not re-DHCP'd yet, so deferring
		// the stamp would deadlock the IP wait. No-op for non-clones / agent-absent.
		var idErr error
		cloneIdentityRequeue, idErr = r.ensureCloneIdentityRegen(ctx, &guest, &existingPod, status)
		if idErr != nil {
			return ctrl.Result{}, idErr
		}
		// cloneFromSnapshot (Snapshot Phase 4): CH loads the snapshot PAUSED,
		// but swiftletd now passes `resume=true` on `--restore` (CH v52) when the
		// restore intent's AutoResume is set, so the clone comes up RUNNING with
		// no controller-driven resume round-trip needed (replaces the former
		// resumeCloneIfNeeded; Bug #73 / CH v52 capabilities assessment).
		// If guest is running but IP not yet discovered, requeue to catch annotation update
		if status.Phase == swiftv1alpha1.SwiftGuestPhaseRunning &&
			(status.Network == nil || status.Network.PrimaryIP == "") {
			recordGuestMetrics(&guest, &guest.Status, status, podForMetrics)
			if err := r.patchStatus(ctx, &guest, status); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		// TODO: consider updating pod spec if resolved changed (e.g., resources)
	}

	// Service exposure (S1): mint/GC the per-guest Service and echo it in status
	// before the patch. See docs/design/service-exposure.md.
	svcName, err := r.ensureExposedService(ctx, &guest)
	if err != nil {
		return ctrl.Result{}, err
	}
	setExposedPortsStatus(&guest, status, svcName, isLauncherReady(podForMetrics))

	recordGuestMetrics(&guest, &guest.Status, status, podForMetrics)
	if err := r.patchStatus(ctx, &guest, status); err != nil {
		return ctrl.Result{}, err
	}

	// Phase 4 drain integration: ensure the per-guest PodDisruptionBudget
	// (maxUnavailable: 0). The hard floor that protects the VM from drain even
	// when the eviction webhook is down. Reached only past the pod-ensure
	// block above, so the launcher pod exists; owned by the guest (GC on
	// delete). See ensureMigrationPDB.
	if err := r.ensureMigrationPDB(ctx, &guest); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue while an agent-enabled clone's identity regen is still in flight
	// (0 = no requeue once it reaches a terminal CloneIdentityRegenerated state).
	return ctrl.Result{RequeueAfter: cloneIdentityRequeue}, nil
}

func recordGuestMetrics(guest *swiftv1alpha1.SwiftGuest, oldStatus, newStatus *swiftv1alpha1.SwiftGuestStatus, pod *corev1.Pod) {
	ns := guest.Namespace
	oldPhase := swiftv1alpha1.SwiftGuestPhase("")
	if oldStatus != nil {
		oldPhase = oldStatus.Phase
	}
	newPhase := swiftv1alpha1.SwiftGuestPhase("")
	if newStatus != nil {
		newPhase = newStatus.Phase
	}

	// Running-count gauges live in metrics.StateCollector (computed from
	// cluster state at scrape time). Never re-add an Inc/Dec transition
	// gauge here: it silently drifts to 0/negative across controller
	// restarts (already-Running guests produce no transition to re-count).
	if oldPhase == swiftv1alpha1.SwiftGuestPhaseRunning && newPhase != swiftv1alpha1.SwiftGuestPhaseRunning {
		metrics.UnmarkVMBootObserved(ns + "/" + guest.Name)
	}

	// VMBootSeconds: observe when GuestRunning becomes True (once per boot)
	if newPhase == swiftv1alpha1.SwiftGuestPhaseRunning && pod != nil {
		guestRunning := findCondition(newStatus, "GuestRunning")
		if guestRunning != nil && guestRunning.Status == metav1.ConditionTrue {
			key := guest.Namespace + "/" + guest.Name
			if metrics.MarkVMBootObserved(key) {
				elapsed := time.Since(pod.CreationTimestamp.Time).Seconds()
				metrics.VMBootSeconds.WithLabelValues(ns).Observe(elapsed)
			}
		}
	}

	// VMFailuresTotal: increment on transition to Failed. The label carries
	// the bounded condition .Reason token, NOT the free-text .Message —
	// messages embed pod/node names and error strings, i.e. unbounded
	// series cardinality (observability design doc §1.5). Prefer the first
	// non-True condition so a healthy condition's Reason can't shadow the
	// failing one.
	if newPhase == swiftv1alpha1.SwiftGuestPhaseFailed && oldPhase != swiftv1alpha1.SwiftGuestPhaseFailed {
		reason := "Unknown"
		if c := findCondition(newStatus, "PodScheduled"); c != nil && c.Status != metav1.ConditionTrue && c.Reason != "" {
			reason = c.Reason
		} else if c := findCondition(newStatus, "GuestRunning"); c != nil && c.Status != metav1.ConditionTrue && c.Reason != "" {
			reason = c.Reason
		} else if c := findCondition(newStatus, "Resolved"); c != nil && c.Status != metav1.ConditionTrue && c.Reason != "" {
			reason = c.Reason
		}
		metrics.VMFailuresTotal.WithLabelValues(ns, reason).Inc()
	}

	// CloneTotal (Phase 5): cloneFromSnapshot guests, by result. Rides the same
	// phase-transition detection above so it fires once per entry into the
	// state (Running on a successful clone-resume; Failed on a clone prep/boot
	// failure).
	if guest.UsesCloneFromSnapshot() {
		if newPhase == swiftv1alpha1.SwiftGuestPhaseRunning && oldPhase != swiftv1alpha1.SwiftGuestPhaseRunning {
			metrics.CloneTotal.WithLabelValues("running").Inc()
		}
		if newPhase == swiftv1alpha1.SwiftGuestPhaseFailed && oldPhase != swiftv1alpha1.SwiftGuestPhaseFailed {
			metrics.CloneTotal.WithLabelValues("failed").Inc()
		}
	}
}

func findCondition(status *swiftv1alpha1.SwiftGuestStatus, condType string) *metav1.Condition {
	if status == nil {
		return nil
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			return &status.Conditions[i]
		}
	}
	return nil
}

func (r *SwiftGuestReconciler) patchStatus(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, status *swiftv1alpha1.SwiftGuestStatus) error {
	if equality.Semantic.DeepEqual(guest.Status, status) {
		return nil
	}
	patch := client.MergeFrom(guest.DeepCopy())
	guest.Status = *status
	return r.Status().Patch(ctx, guest, patch)
}

// swiftImageToSwiftGuests enqueues SwiftGuests that reference a SwiftImage when the SwiftImage changes.
func (r *SwiftGuestReconciler) swiftImageToSwiftGuests(ctx context.Context, obj client.Object) []reconcile.Request {
	var img imagev1alpha1.SwiftImage
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), &img); err != nil {
		return nil
	}
	guests, err := imageref.ListGuestsReferencingImage(ctx, r.Client, &img)
	if err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(guests))
	for i := range guests {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&guests[i])})
	}
	return reqs
}

// SetupWithManager registers the reconciler with the manager.
func (r *SwiftGuestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&swiftv1alpha1.SwiftGuest{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.Job{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&corev1.Service{}).
		Watches(&imagev1alpha1.SwiftImage{}, handler.EnqueueRequestsFromMapFunc(r.swiftImageToSwiftGuests)).
		Complete(r)
}
