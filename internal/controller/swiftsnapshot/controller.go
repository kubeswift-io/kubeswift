// Package swiftsnapshot reconciles SwiftSnapshot resources.
//
// Phase 1 supports the csi-volume-snapshot backend only. The state machine
// is Pending -> Capturing -> Ready (or Failed). The controller creates a
// snapshot.storage.k8s.io VolumeSnapshot of the SwiftGuest's per-guest
// root-disk clone PVC, then waits for readyToUse=true before flipping the
// SwiftSnapshot to Ready.
//
// Local and S3 backends are reserved for later phases; this controller
// rejects them up-front with a Failed condition. The validation webhook
// also rejects them, but defense in depth.
package swiftsnapshot

import (
	"context"
	"strconv"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// SwiftSnapshotReconciler reconciles SwiftSnapshot resources.
type SwiftSnapshotReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// SnapshotS3Image is the snapshot-s3 uploader image used by the s3
	// (Tier C) backend's upload Job. Wired from KUBESWIFT_SNAPSHOT_S3_IMAGE.
	SnapshotS3Image string
	// SnapshotORASImage is the snapshot-oras uploader image used by the oci
	// backend's push Job. Wired from KUBESWIFT_SNAPSHOT_ORAS_IMAGE.
	SnapshotORASImage string
	// VolumeSnapshotEnabled gates the Owns(VolumeSnapshot) watch. When the CSI
	// external-snapshotter CRDs (snapshot.storage.k8s.io/v1) are absent, that watch
	// cannot sync its cache and the manager fatally exits. Set from a one-time
	// discovery check in main. When false, the csi-volume-snapshot backend is
	// unavailable; the local and s3 backends are unaffected.
	VolumeSnapshotEnabled bool
}

// Reconcile drives the SwiftSnapshot state machine.
func (r *SwiftSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var snap snapshotv1alpha1.SwiftSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion path: when the SwiftSnapshot is being deleted and our
	// finalizer is present, run the hostPath cleanup pod. Drop the
	// finalizer once cleanup completes so the apiserver can GC. This
	// runs before the terminal-state check because deletion can
	// happen from any phase.
	if snap.DeletionTimestamp != nil {
		done, err := r.handleDeletion(ctx, &snap)
		if err != nil {
			return ctrl.Result{}, err
		}
		if done {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Terminal states: ensure the cleanup finalizer is in place
	// (no-op for csi-volume-snapshot) and otherwise nothing to do.
	if snap.Status.Phase == snapshotv1alpha1.SwiftSnapshotPhaseReady {
		if err := r.ensureFinalizer(ctx, &snap); err != nil {
			return ctrl.Result{}, err
		}
		// TTL-driven retention: delete once capturedAt+ttl elapses (deferred
		// while still referenced). No-op when spec.ttl is unset.
		requeue, err := r.handleRetention(ctx, &snap)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	if snap.Status.Phase == snapshotv1alpha1.SwiftSnapshotPhaseFailed {
		return ctrl.Result{}, nil
	}

	phase := snap.Status.Phase
	if phase == "" {
		phase = snapshotv1alpha1.SwiftSnapshotPhasePending
	}
	status := snap.Status.DeepCopy()

	switch phase {
	case snapshotv1alpha1.SwiftSnapshotPhasePending:
		result, requeue, err := r.handlePending(ctx, &snap, status)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !result {
			// Not yet ready to advance — persist any progress + requeue.
			if updateErr := r.persist(ctx, &snap, status); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: requeue}, nil
		}

	case snapshotv1alpha1.SwiftSnapshotPhaseCapturing:
		ready, errMsg, err := r.handleCapturing(ctx, &snap, status)
		if err != nil {
			return ctrl.Result{}, err
		}
		if errMsg != "" {
			setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonSnapshotFailed, errMsg)
		} else if !ready {
			if updateErr := r.persist(ctx, &snap, status); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

	case snapshotv1alpha1.SwiftSnapshotPhaseUploading:
		// Full-state (P4 includeDisk): capture-then-terminate the DISK to oci
		// first — terminate the still-paused launcher to release the root PVC,
		// then chunk it — before the memory push below. handleUploadingOCI
		// preserves the status.oci.disk this stamps.
		if snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendOCI &&
			snap.Spec.IncludeDisk && (status.OCI == nil || status.OCI.Disk == nil) {
			done, errMsg, err := r.handleFullStateDiskCapture(ctx, &snap, status)
			if err != nil {
				return ctrl.Result{}, err
			}
			if errMsg != "" {
				setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseFailed)
				setReadyCondition(status, metav1.ConditionFalse, ReasonSnapshotFailed, errMsg)
				return ctrl.Result{}, r.persist(ctx, &snap, status)
			}
			if !done {
				if updateErr := r.persist(ctx, &snap, status); updateErr != nil {
					return ctrl.Result{}, updateErr
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			// disk artifact done — fall through to the memory push.
		}
		// s3 / oci backends: the capture is on the node-local hostPath; watch the
		// upload/push Job move it to the store, then stamp status and go Ready.
		handleUpload := r.handleUploading
		if snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendOCI {
			handleUpload = r.handleUploadingOCI
		}
		ready, errMsg, err := handleUpload(ctx, &snap, status)
		if err != nil {
			return ctrl.Result{}, err
		}
		if errMsg != "" {
			setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonSnapshotFailed, errMsg)
		} else if !ready {
			if updateErr := r.persist(ctx, &snap, status); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

	default:
		// Unknown phase — treat as Pending.
		logger.Info("unknown phase, restarting at Pending", "phase", phase)
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
	}

	return ctrl.Result{}, r.persist(ctx, &snap, status)
}

// handlePending validates inputs, captures CapturedGuestSpec, kicks off the
// VolumeSnapshot, and transitions to Capturing.
//
// Returns (advanced, requeueAfter, err):
//   - advanced=true means the status now reflects Capturing.
//   - advanced=false means we're still in Pending and the caller should
//     requeue after the returned duration.
func (r *SwiftSnapshotReconciler) handlePending(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
	status *snapshotv1alpha1.SwiftSnapshotStatus,
) (bool, time.Duration, error) {
	// Backend dispatch:
	//   csi-volume-snapshot (Phase 1): create snapshot.storage VolumeSnapshot
	//   local              (Phase 2): drive launcher pod via action annotations
	//   s3                 (Phase 3): reuse the local capture (into a derived
	//                       node-local dir), then upload to S3 in the Uploading phase
	switch snap.Spec.Backend.Type {
	case snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot:
		// Falls through to the existing csi-volume-snapshot path.
	case snapshotv1alpha1.SnapshotBackendLocal, snapshotv1alpha1.SnapshotBackendS3, snapshotv1alpha1.SnapshotBackendOCI:
		return r.handlePendingLocal(ctx, snap, status)
	default:
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonUnsupportedBackend,
			"backend "+string(snap.Spec.Backend.Type)+" is not yet implemented in this controller")
		return true, 0, nil
	}

	// Resolve source SwiftGuest in the same namespace.
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: snap.Spec.GuestRef.Name, Namespace: snap.Namespace}, &guest); err != nil {
		if !isNotFound(err) {
			return false, 0, err
		}
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonGuestNotFound,
			"SwiftGuest "+snap.Spec.GuestRef.Name+" not found in namespace "+snap.Namespace)
		// Source guest may appear later — requeue rather than fail.
		return false, 10 * time.Second, nil
	}

	// Gate on the source guest's root disk being populated. The PVC being Bound
	// (checked below) is necessary but NOT sufficient: the per-guest rootclone
	// Job copies image.raw INTO the PVC *after* it binds, so a snapshot taken
	// while the guest is still provisioning captures an empty disk — an
	// unbootable restore. (Cluster-observed: a SwiftSnapshot applied alongside a
	// fresh source guest snapshotted the PVC ~64s before rootclone wrote
	// image.raw.) Require the guest to have finished provisioning: Running
	// (rootclone complete + booted) or Stopped (ran before). This mirrors the
	// Tier B pod-Running gate (local.go), but at the guest-phase level — a
	// disk-only snapshot does not need a live launcher pod, so a Stopped guest's
	// disk remains valid to back up. Pending/Scheduling/Failed requeue.
	if !guestRootDiskPopulated(&guest) {
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonGuestNotReady,
			"source SwiftGuest "+guest.Name+" has not finished provisioning (phase="+
				string(guest.Status.Phase)+"); waiting until its root disk is populated before snapshotting")
		return false, 5 * time.Second, nil
	}

	// Locate the per-guest root-disk clone PVC. The shared SwiftImage PVC
	// is read-only across guests; snapshotting it would be incorrect.
	pvc, err := r.guestRootPVC(ctx, snap.Namespace, guest.Name)
	if err != nil {
		return false, 0, err
	}
	if pvc == nil {
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRootPVCNotFound,
			"per-guest root-disk PVC not yet created; SwiftGuest may still be provisioning")
		return false, 5 * time.Second, nil
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRootPVCNotFound,
			"per-guest root-disk PVC "+pvc.Name+" not yet Bound (phase="+string(pvc.Status.Phase)+")")
		return false, 5 * time.Second, nil
	}

	// Capture spec metadata before kicking off the VolumeSnapshot — these
	// are needed by SwiftRestore to validate compatibility. CSI is disk-only
	// (never a full-state source-independent source), so no resolved surface.
	status.GuestSpec = capturedGuestSpec(&guest, nil)
	if guest.Status.Runtime != nil {
		status.Hypervisor = guest.Status.Runtime.Hypervisor
	}

	// Phase 1 captures only the root disk. Data disks are out of scope.
	setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseCapturing)
	setReadyCondition(status, metav1.ConditionFalse, ReasonCapturing, "creating VolumeSnapshot of root disk")
	return true, 0, nil
}

// handleCapturing creates the VolumeSnapshot if needed and polls readiness.
// Returns (ready, errMsg, err):
//   - errMsg non-empty -> caller transitions to Failed.
//   - ready=true -> caller transitions to Ready (and this function has
//     populated status.Disks/CapturedAt/TotalSizeBytes).
//   - ready=false, errMsg="" -> caller requeues.
func (r *SwiftSnapshotReconciler) handleCapturing(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
	status *snapshotv1alpha1.SwiftSnapshotStatus,
) (bool, string, error) {
	// Backend dispatch — local and s3 both capture via the launcher pod's
	// status annotations (s3 just uploads afterward); CSI drives VolumeSnapshot.
	if snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendLocal ||
		snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendS3 ||
		snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendOCI {
		return r.handleCapturingLocal(ctx, snap, status)
	}
	pvc, err := r.guestRootPVC(ctx, snap.Namespace, snap.Spec.GuestRef.Name)
	if err != nil {
		return false, "", err
	}
	if pvc == nil {
		// PVC vanished mid-capture — fail the snapshot rather than spin.
		return false, "per-guest root-disk PVC " + rootPVCName(snap.Spec.GuestRef.Name) + " disappeared during snapshot", nil
	}

	ready, restoreSize, errMsg, err := r.ensureVolumeSnapshot(ctx, snap, pvc.Name)
	if err != nil {
		return false, "", err
	}
	if errMsg != "" {
		return false, errMsg, nil
	}
	if !ready {
		return false, "", nil
	}

	// VolumeSnapshot is readyToUse — populate status.disks and flip to Ready.
	now := metav1.Now()
	status.CapturedAt = &now
	status.Disks = []snapshotv1alpha1.SnapshotDiskRef{{
		Role:      "root",
		SizeBytes: restoreSize,
		Handle:    snap.Namespace + "/" + VolumeSnapshotName(snap.Name),
	}}
	status.TotalSizeBytes = restoreSize
	setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseReady)
	setReadyCondition(status, metav1.ConditionTrue, ReasonSnapshotReady, "VolumeSnapshot is readyToUse")
	return true, "", nil
}

// guestRootDiskPopulated reports whether the source guest's root disk is
// guaranteed populated by the rootclone Job, so a CSI snapshot of its PVC will
// capture a bootable image. Running means the launcher booted from the disk
// (rootclone complete); Stopped means it ran before and was stopped. In
// Pending/Scheduling the rootclone may still be in flight (the PVC binds before
// image.raw is written), and Failed is an uncertain disk state — all three
// requeue rather than snapshot an empty/partial disk.
func guestRootDiskPopulated(guest *swiftv1alpha1.SwiftGuest) bool {
	switch guest.Status.Phase {
	case swiftv1alpha1.SwiftGuestPhaseRunning, swiftv1alpha1.SwiftGuestPhaseStopped:
		return true
	default:
		return false
	}
}

// capturedGuestSpec freezes the SwiftGuest spec fields SwiftRestore needs. When
// rg is non-nil (a full-state oci capture, where the source is live at capture
// time so it resolves cleanly), it also freezes the launcher-sufficient surface a
// source-independent clone needs when the source guest/image/seedProfile are gone.
// A nil rg (CSI/local
// captures, or a resolve failure) leaves the expanded fields empty — such a clone
// still needs the live source spec (the pre-source-independence behaviour).
func capturedGuestSpec(guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest) *snapshotv1alpha1.CapturedGuestSpec {
	out := &snapshotv1alpha1.CapturedGuestSpec{}
	if guest.Spec.ImageRef != nil {
		out.ImageName = guest.Spec.ImageRef.Name
	}
	if rg == nil {
		return out
	}
	out.CPU = strconv.Itoa(rg.Resources.CPU)
	out.MemoryMi = int64(rg.Resources.Memory)
	if !rg.RootDisk.Size.IsZero() {
		out.RootDiskSize = rg.RootDisk.Size.String()
	}
	out.Storage = &snapshotv1alpha1.CapturedStorage{
		AccessMode:       rg.Storage.AccessMode,
		VolumeMode:       rg.Storage.VolumeMode,
		StorageClassName: rg.Storage.StorageClassName,
	}
	out.Network = rg.HasNetwork()
	for _, iface := range rg.Interfaces {
		out.InterfaceNames = append(out.InterfaceNames, iface.Name)
	}
	out.GuestAgent = rg.GuestAgentEnabled
	out.OSType = rg.GetOSType()
	out.HasSeed = guest.Spec.SeedProfileRef != nil
	out.HasDataDisks = guest.Spec.DataDiskRef != nil || len(guest.Spec.DataDiskRefs) > 0
	return out
}

// persist writes status changes back to the API server.
func (r *SwiftSnapshotReconciler) persist(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, status *snapshotv1alpha1.SwiftSnapshotStatus) error {
	// Fire the Phase 5 metric exactly once, on the non-terminal -> terminal
	// transition (compare the live object's old phase against the new one).
	freshTerminal := isSnapshotTerminal(status.Phase) && !isSnapshotTerminal(snap.Status.Phase)
	snap.Status = *status
	if err := r.Status().Update(ctx, snap); err != nil {
		return err
	}
	if freshTerminal {
		recordSnapshotTerminal(snap)
	}
	return nil
}

// isSnapshotTerminal reports whether a SwiftSnapshot phase is terminal.
func isSnapshotTerminal(p snapshotv1alpha1.SwiftSnapshotPhase) bool {
	return p == snapshotv1alpha1.SwiftSnapshotPhaseReady || p == snapshotv1alpha1.SwiftSnapshotPhaseFailed
}

// recordSnapshotTerminal emits the Phase 5 snapshot metrics on the
// non-terminal -> terminal transition: a per-backend/result counter, plus
// capture/pause/upload latency and size for successful captures.
func recordSnapshotTerminal(snap *snapshotv1alpha1.SwiftSnapshot) {
	backend := string(snap.Spec.Backend.Type)
	st := &snap.Status
	var result string
	switch st.Phase {
	case snapshotv1alpha1.SwiftSnapshotPhaseReady:
		result = "ready"
	case snapshotv1alpha1.SwiftSnapshotPhaseFailed:
		result = "failed"
	default:
		return
	}
	metrics.SnapshotTotal.WithLabelValues(backend, result).Inc()
	if result != "ready" {
		return
	}
	if st.CapturedAt != nil {
		if d := st.CapturedAt.Sub(snap.CreationTimestamp.Time); d > 0 {
			metrics.SnapshotCaptureSeconds.WithLabelValues(backend).Observe(d.Seconds())
		}
	}
	if st.ObservedPauseWindowMs > 0 {
		metrics.SnapshotPauseWindowSeconds.WithLabelValues(backend).Observe(float64(st.ObservedPauseWindowMs) / 1000.0)
	}
	if st.TotalSizeBytes > 0 {
		metrics.SnapshotSizeBytes.WithLabelValues(backend).Observe(float64(st.TotalSizeBytes))
	}
	if st.S3 != nil && st.S3.UploadedAt != nil && st.CapturedAt != nil {
		if d := st.S3.UploadedAt.Sub(st.CapturedAt.Time); d > 0 {
			metrics.SnapshotUploadSeconds.Observe(d.Seconds())
		}
	}
}

// isNotFound is a small wrapper so this package doesn't grow extra imports
// for one call site.
func isNotFound(err error) bool {
	return client.IgnoreNotFound(err) == nil
}

// SetupWithManager registers the reconciler.
//
// Owns(VolumeSnapshot) gives the csi-volume-snapshot path immediate
// requeue when readyToUse flips. The local-backend (Tier B) path
// uses Watches on Pod to make pod-phase transitions (Failed/Succeeded
// mid-capture) observable without waiting for periodic resync — the
// architect Q1 pod-death-recovery requirement. We can't Owns(Pod)
// because the launcher pod is owned by the SwiftGuest, not the
// SwiftSnapshot, so EnqueueRequestsFromMapFunc maps Pod events to
// the SwiftSnapshots that reference that pod's guest.
func (r *SwiftSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&snapshotv1alpha1.SwiftSnapshot{}).
		Owns(&batchv1.Job{})
	if r.VolumeSnapshotEnabled {
		b = b.Owns(&volumesnapshotv1.VolumeSnapshot{})
	}
	return b.
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToSnapshots),
		).
		Complete(r)
}

// podToSnapshots maps a Pod event to the SwiftSnapshots that target
// the Pod's guest. Returns at most one Request: launcher pods are
// 1:1 with SwiftGuests, and we filter to SwiftSnapshots in the same
// namespace whose guestRef.name matches the pod name AND whose phase
// is Capturing (terminal phases don't need re-reconcile).
//
// The mapper returns no requests for non-launcher pods (e.g. CSI
// driver pods, image-import jobs); the guestRef-name filter is the
// cheap lever that gates the list operation.
func (r *SwiftSnapshotReconciler) podToSnapshots(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	var snaps snapshotv1alpha1.SwiftSnapshotList
	if err := r.List(ctx, &snaps, client.InNamespace(pod.Namespace)); err != nil {
		// Drop the event silently; the periodic resync (10h default)
		// will eventually pick up the transition. Logging at info
		// level would be noisy on every Pod event.
		return nil
	}
	var out []ctrlreconcile.Request
	for i := range snaps.Items {
		s := &snaps.Items[i]
		if s.Spec.GuestRef.Name != pod.Name {
			continue
		}
		// Only Capturing snapshots care about Pod transitions —
		// Pending hasn't dispatched the action, terminal phases are
		// done. Skipping the rest keeps the reconcile-rate budget
		// for the SwiftSnapshots that actually need it.
		if s.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
			continue
		}
		out = append(out, ctrlreconcile.Request{
			NamespacedName: client.ObjectKey{Name: s.Name, Namespace: s.Namespace},
		})
	}
	return out
}
