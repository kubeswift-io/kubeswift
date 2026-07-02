// Tier B (local hostPath) capture path for SwiftSnapshot.
//
// Unlike the csi-volume-snapshot path which delegates to the CSI driver,
// local-backend captures drive the running launcher pod directly:
//
//   1. handlePendingLocal  — validate inputs, locate the launcher pod,
//      pin it to a node, write the kubeswift.io/snapshot-action: capture
//      annotation; flip the SwiftSnapshot to Capturing.
//   2. handleCapturingLocal — poll the launcher pod's status annotations
//      (kubeswift.io/snapshot-status, snapshot-status-id,
//      snapshot-pause-window-ms). When swiftletd's status-id matches the
//      action-id we wrote, finalize: status=Ready (or Failed if the
//      launcher reported failure) with NodeName, ObservedPauseWindowMs,
//      SnapshotDirVersion, MemorySnapshot populated.
//
// Idempotency: action-id is `<snapshot-name>-<resourceVersion>`. swiftletd
// keys its "already processed" check on this id (see rust/swiftletd/src/
// action.rs). The controller only writes a new action-id when the
// SwiftSnapshot itself is updated; controller-runtime retries against
// unchanged spec are no-ops on the launcher.

package swiftsnapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// Annotation keys — the controller writes (input to swiftletd).
// Mirrors the constants in rust/swiftletd/src/action.rs.
const (
	annoAction     = "kubeswift.io/snapshot-action"
	annoActionID   = "kubeswift.io/snapshot-action-id"
	annoActionArgs = "kubeswift.io/snapshot-action-args"
)

// Annotation keys — swiftletd writes (read by controller).
const (
	annoStatus        = "kubeswift.io/snapshot-status"
	annoStatusID      = "kubeswift.io/snapshot-status-id"
	annoStatusDetail  = "kubeswift.io/snapshot-status-detail"
	annoPauseWindowMs = "kubeswift.io/snapshot-pause-window-ms"
)

// Action verbs.
const (
	verbCapture = "capture"
	verbResume  = "resume"
	verbPrepare = "prepare"
)

// HostPathBaseDir is the only filesystem prefix accepted by the
// validation webhook for local-backend snapshots. Mirrors
// LocalBackendHostPathPrefix in internal/webhook/swiftsnapshot.
const HostPathBaseDir = "/var/lib/kubeswift/snapshots/"

// SnapshotDirVersionV1 is the only on-disk format version emitted by
// this controller. Restore's hypervisor-version check requires an
// exact match, with patch-level CH drift permitted via major.minor
// (architect risk #3).
const SnapshotDirVersionV1 = "v1"

// CaptureDeadlineAnnotation lets the operator override the wall-clock
// deadline for a Capturing SwiftSnapshot. Value is seconds; missing or
// unparseable falls back to DefaultCaptureDeadlineSeconds.
const CaptureDeadlineAnnotation = "kubeswift.io/snapshot-deadline-seconds"

// DefaultCaptureDeadlineSeconds caps the time a SwiftSnapshot can sit
// in Capturing before the controller forces it to Failed. 600s covers
// a ~200 GiB VM at the Phase 0 ~2.8s/GiB Longhorn curve with margin;
// operators with larger VMs override via CaptureDeadlineAnnotation.
const DefaultCaptureDeadlineSeconds = 600

// captureArgs is the JSON shape we emit on the launcher pod's
// kubeswift.io/snapshot-action-args annotation when the action is
// "capture". Kept structurally identical to swiftletd's CaptureArgs
// (rust/swiftletd/src/action.rs) so deserialization there is direct.
type captureArgs struct {
	DestinationURL      string `json:"destination_url"`
	TimeoutSeconds      int64  `json:"timeout_seconds,omitempty"`
	ResumeAfterSnapshot bool   `json:"resume_after_snapshot"`
}

// handlePendingLocal kicks off a local-backend capture. Returns
// (advanced, requeueAfter, err) following the same convention as
// handlePending: advanced=true means status now reflects Capturing.
func (r *SwiftSnapshotReconciler) handlePendingLocal(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
	status *snapshotv1alpha1.SwiftSnapshotStatus,
) (bool, time.Duration, error) {
	// Resolve source SwiftGuest in the same namespace.
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: snap.Spec.GuestRef.Name, Namespace: snap.Namespace}, &guest); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, 0, err
		}
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonGuestNotFound,
			"SwiftGuest "+snap.Spec.GuestRef.Name+" not found in namespace "+snap.Namespace)
		return false, 10 * time.Second, nil
	}

	// Find the launcher pod. It must be Running for memory snapshot to
	// be meaningful — a Stopped guest has no memory state to capture.
	pod, err := r.findLauncherPod(ctx, snap.Namespace, snap.Spec.GuestRef.Name)
	if err != nil {
		return false, 0, err
	}
	if pod == nil {
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonGuestNotFound,
			"launcher pod for SwiftGuest "+snap.Spec.GuestRef.Name+" not yet present")
		return false, 5 * time.Second, nil
	}
	if pod.Status.Phase != corev1.PodRunning {
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonGuestNotFound,
			"launcher pod "+pod.Name+" not yet Running (phase="+string(pod.Status.Phase)+")")
		return false, 5 * time.Second, nil
	}

	// Capture the source guest spec, hypervisor, and node before we
	// write the action — these become part of the snapshot's identity
	// and are needed by SwiftRestore.
	status.GuestSpec = capturedGuestSpec(&guest)
	if guest.Status.Runtime != nil {
		status.Hypervisor = guest.Status.Runtime.Hypervisor
	}
	status.NodeName = pod.Spec.NodeName
	status.SnapshotDirVersion = SnapshotDirVersionV1

	// Resolve destination directory. For the local backend the webhook
	// ensures the operator supplied a hostPath under HostPathBaseDir; for the
	// s3 backend the controller derives the dir (s3LocalDir). swiftletd mkdir's
	// it before capture.
	destDir := captureDestDir(snap)
	srcURL := destDir
	if !strings.HasSuffix(srcURL, "/") {
		srcURL = srcURL + "/"
	}
	srcURL = "file://" + srcURL

	// Generate stable action-id: snapshot-name + first 8 chars of UID.
	// UID is immutable for the lifetime of the SwiftSnapshot; using it
	// (instead of ResourceVersion) means handleCapturingLocal can
	// re-derive the same id on later reconciles even though
	// ResourceVersion bumps every time the controller writes status.
	// swiftletd treats repeated action-ids as no-ops, so the launcher
	// only processes the capture once.
	actionID := capturingActionID(snap)

	// Full-state (includeDisk) capture is capture-then-terminate: the guest must
	// stay paused after the memory snapshot so the disk is frozen at that instant
	// for the disk-chunk step — never resume it, regardless of the spec field.
	resumeAfter := snap.Spec.ResumeAfterSnapshot
	if snap.Spec.IncludeDisk {
		resumeAfter = false
	}
	args := captureArgs{
		DestinationURL:      srcURL,
		ResumeAfterSnapshot: resumeAfter,
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return false, 0, fmt.Errorf("marshal capture args: %w", err)
	}

	// Patch the launcher pod's annotations to drive the action handler.
	if err := r.patchPodActionAnnotations(ctx, pod, verbCapture, actionID, string(argsJSON)); err != nil {
		return false, 0, fmt.Errorf("patch action annotation: %w", err)
	}

	setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseCapturing)
	setReadyCondition(status, metav1.ConditionFalse, ReasonCapturing,
		"sent capture action to launcher pod "+pod.Name+" on node "+pod.Spec.NodeName)
	return true, 0, nil
}

// handleCapturingLocal polls the launcher pod's status-id annotation
// for our action-id and finalizes when it matches.
//
// Returns (ready, errMsg, err) following handleCapturing's contract:
//   - ready=true → caller transitions to Ready.
//   - errMsg != "" → caller transitions to Failed.
//   - ready=false, errMsg="" → caller requeues.
func (r *SwiftSnapshotReconciler) handleCapturingLocal(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
	status *snapshotv1alpha1.SwiftSnapshotStatus,
) (bool, string, error) {
	// Wall-clock deadline. Without this a launcher that died silently
	// (no Failed/Succeeded transition because, e.g., the Pod object was
	// force-deleted) would leave the SwiftSnapshot stuck in Capturing
	// indefinitely. The Pod watcher (SetupWithManager) handles the
	// fast path; this is the slow-path safety net.
	if exceeded, deadlineSecs := captureDeadlineExceeded(snap); exceeded {
		return false, fmt.Sprintf("capture deadline (%ds) exceeded", deadlineSecs), nil
	}

	pod, err := r.findLauncherPod(ctx, snap.Namespace, snap.Spec.GuestRef.Name)
	if err != nil {
		return false, "", err
	}
	if pod == nil {
		// Launcher pod gone mid-capture: cannot recover. The Pod watcher
		// in SetupWithManager makes this observation prompt — without
		// it we'd wait for the next periodic resync.
		return false, "launcher pod for SwiftGuest " + snap.Spec.GuestRef.Name + " disappeared during capture", nil
	}
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		// Launcher exited (RestartPolicy=Never) before reporting status
		// ready. The Pod watcher in SetupWithManager surfaces this on
		// the same reconcile pass as the phase transition.
		return false, "launcher pod " + pod.Name + " phase=" + string(pod.Status.Phase) + " before capture completed", nil
	}

	annotations := pod.GetAnnotations()
	expectedID := capturingActionID(snap)
	statusID := annotations[annoStatusID]
	statusVal := annotations[annoStatus]
	statusDetail := annotations[annoStatusDetail]

	// status-id mismatch: swiftletd hasn't picked up our action yet, or
	// it's processing a different one. Wait for the next tick.
	if statusID != expectedID {
		return false, "", nil
	}

	switch statusVal {
	case "running", "":
		return false, "", nil
	case "rejected":
		return false, "launcher rejected capture action: " + statusDetail, nil
	case "failed":
		return false, "capture failed: " + statusDetail, nil
	case "ready":
		// Finalize.
	default:
		return false, "unexpected snapshot-status value: " + statusVal, nil
	}

	// Pause window from launcher's annotation (best-effort parse).
	if v, ok := annotations[annoPauseWindowMs]; ok && v != "" {
		if ms, parseErr := strconv.ParseInt(v, 10, 64); parseErr == nil {
			status.ObservedPauseWindowMs = ms
		}
	}

	// MemorySnapshot ref: handle is the on-node hostPath. Size in
	// bytes is computed lazily by SwiftRestore at restore time
	// (du(1)-style on a path requires either shelling out from the
	// controller pod or a DaemonSet helper, neither of which is in
	// Phase 2 commit 8's scope).
	now := metav1.Now()
	status.CapturedAt = &now
	status.MemorySnapshot = &snapshotv1alpha1.MemorySnapshotRef{
		Handle: captureDestDir(snap),
	}

	// s3 / oci backends: capture is done locally; now move it to the store.
	// Create the node-pinned upload/push Job and advance to Uploading (the
	// matching handler watches it to completion, then stamps status + Ready).
	// The local backend is done — go straight to Ready.
	if snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendS3 {
		if err := r.ensureUploadJob(ctx, snap, status); err != nil {
			return false, "", err
		}
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseUploading)
		setReadyCondition(status, metav1.ConditionFalse, ReasonCapturing,
			"capture complete on node "+status.NodeName+"; uploading to S3")
		return true, "", nil
	}
	if snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendOCI {
		if err := r.ensureOCIPushJob(ctx, snap, status); err != nil {
			return false, "", err
		}
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseUploading)
		setReadyCondition(status, metav1.ConditionFalse, ReasonCapturing,
			"capture complete on node "+status.NodeName+"; pushing to "+ociReference(snap))
		return true, "", nil
	}

	setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseReady)
	setReadyCondition(status, metav1.ConditionTrue, ReasonSnapshotReady,
		"launcher reported snapshot ready on node "+status.NodeName)
	return true, "", nil
}

// findLauncherPod returns the launcher pod for the SwiftGuest, or nil
// if not yet present.
//
// Convention: launcher pod name == SwiftGuest name (one launcher per
// guest). Get-by-name keeps RBAC narrow.
func (r *SwiftSnapshotReconciler) findLauncherPod(ctx context.Context, namespace, guestName string) (*corev1.Pod, error) {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Name: guestName, Namespace: namespace}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pod, nil
}

// patchPodActionAnnotations writes a merge patch that sets the action
// triple atomically. Same-key writes from the lease poller, socket-
// ready callback, and runtime reporter all go through their own
// keys (writer-key partitioning, architect Q1), so this patch is safe.
func (r *SwiftSnapshotReconciler) patchPodActionAnnotations(
	ctx context.Context,
	pod *corev1.Pod,
	verb, actionID, argsJSON string,
) error {
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				annoAction:     verb,
				annoActionID:   actionID,
				annoActionArgs: argsJSON,
			},
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return r.Patch(ctx, pod, client.RawPatch(types.MergePatchType, data))
}

// reservedReferences keeps verbResume and verbPrepare reachable so
// the constants don't get optimized out by go vet's unused-export
// check. They are consumed by commit 10 (SwiftRestore) to drive the
// resume action and by commit 7's restore-receive intent. Defined
// here because the action constants logically live with the
// snapshot controller.
var _ = []string{verbResume, verbPrepare}

// capturingActionID returns a stable per-SwiftSnapshot action-id used
// to drive (and later observe) the launcher pod's snapshot-action
// annotations.
//
// Stable across status updates: derived from snap.Name and snap.UID,
// both of which are immutable for the lifetime of the resource.
// The original implementation used snap.ResourceVersion which mutates
// on every status write — handlePendingLocal would send id=N, the
// status update bumped resourceVersion to N+2, handleCapturingLocal
// would compute expectedID=N+2 and never match the launcher's mirror
// (still pinned to N). Tier B captures got stuck in Capturing until
// the wall-clock deadline tripped them to Failed.
//
// First 8 chars of the UID are sufficient — UIDs are random UUIDs,
// collisions on the first 8 hex chars across concurrent SwiftSnapshots
// in the same namespace are not a concern (and the resource Name is a
// uniqueness prefix anyway).
func capturingActionID(snap *snapshotv1alpha1.SwiftSnapshot) string {
	uid := string(snap.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return snap.Name + "-" + uid
}

// captureDeadlineExceeded returns (true, deadlineSeconds) when the
// SwiftSnapshot has been in a non-terminal state past its deadline.
// Reads kubeswift.io/snapshot-deadline-seconds (operator override)
// with DefaultCaptureDeadlineSeconds fallback. The "since" reference
// is the SwiftSnapshot's CreationTimestamp — capture is a
// from-creation-to-Ready operation, so the deadline applies to the
// whole run, not just the Capturing phase.
func captureDeadlineExceeded(snap *snapshotv1alpha1.SwiftSnapshot) (bool, int64) {
	deadline := int64(DefaultCaptureDeadlineSeconds)
	if v, ok := snap.Annotations[CaptureDeadlineAnnotation]; ok && v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			deadline = parsed
		}
	}
	if snap.CreationTimestamp.IsZero() {
		return false, deadline
	}
	elapsed := time.Since(snap.CreationTimestamp.Time)
	return elapsed.Seconds() > float64(deadline), deadline
}
