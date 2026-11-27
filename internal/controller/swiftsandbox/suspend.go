package swiftsandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/names"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

// Suspend (spec.operatingMode Suspended): the launcher pauses the microVM and
// writes its state under SuspendRoot/<sandbox uid> on its node (swiftletd
// suspend.rs), then the controller deletes the launcher pod. The sandbox's
// NetworkPolicy, ServiceAccount and Role stay. Deleting a suspended sandbox
// removes the directory through a Job on that node.
const (
	// SuspendRoot is the node directory sandbox state is suspended to.
	SuspendRoot = "/var/lib/kubeswift/sandbox-suspend"

	sandboxSuspendFinalizer = "sandbox.kubeswift.io/suspend-cleanup"

	// The snapshot action namespace on the launcher pod (swiftletd action.rs).
	annSnapshotAction       = "kubeswift.io/snapshot-action"
	annSnapshotActionID     = "kubeswift.io/snapshot-action-id"
	annSnapshotActionArgs   = "kubeswift.io/snapshot-action-args"
	annSnapshotStatus       = "kubeswift.io/snapshot-status"
	annSnapshotStatusID     = "kubeswift.io/snapshot-status-id"
	annSnapshotStatusDetail = "kubeswift.io/snapshot-status-detail"
	annHypervisorVersion    = "kubeswift.io/guest-hypervisor-version"

	// bridgeFeatureResumeSync is what a resumed guest needs from its kernel's
	// bridge: the guest agent op that resets its clock.
	bridgeFeatureResumeSync = "resume-sync"

	reasonSuspendUnsupported = "SuspendUnsupported"
	reasonSuspendFailed      = "SuspendFailed"
	reasonSuspending         = "Suspending"
	reasonSuspended          = "Suspended"
	reasonNotStarted         = "NotStarted"
	reasonResuming           = "Resuming"
	reasonResumed            = "Resumed"
	reasonResumeSyncFailed   = "ResumeSyncFailed"
	reasonSnapshotLost       = "SnapshotLost"

	// suspendActionTimeout bounds the launcher's pause and snapshot.
	suspendActionTimeout = 5 * time.Minute
	// suspendWaitSlack is how much longer than that the controller waits for
	// the launcher's answer before it gives up on the sandbox.
	suspendWaitSlack = time.Minute

	suspendCleanupComponent = "sandbox-suspend-cleanup"
	suspendCleanupMount     = "/suspend"
)

// suspendDir is where a sandbox's state is kept on its node.
func suspendDir(sb *sandboxv1alpha1.SwiftSandbox) string {
	return SuspendRoot + "/" + string(sb.UID)
}

// suspendActionID names one suspend: a new spec generation (Running, then
// Suspended again) is a new suspend.
func suspendActionID(sb *sandboxv1alpha1.SwiftSandbox) string {
	return fmt.Sprintf("%s-suspend-%d", string(sb.UID), sb.Generation)
}

// suspendState is what swiftletd reports in the suspend action's detail
// (suspend.rs SuspendState).
type suspendState struct {
	GuestIP           string `json:"guestIp"`
	HypervisorVersion string `json:"hypervisorVersion"`
	TakenAtUnix       int64  `json:"takenAtUnix"`
	SizeBytes         int64  `json:"sizeBytes"`
}

// reconcileOperatingMode drives spec.operatingMode. It returns handled when
// it took the reconcile over; otherwise the caller goes on as usual.
func (r *SwiftSandboxReconciler) reconcileOperatingMode(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (bool, ctrl.Result, error) {
	switch sb.Status.Phase {
	case sandboxv1alpha1.SwiftSandboxSuspending:
		res, err := r.reconcileSuspending(ctx, sb)
		return true, res, err
	case sandboxv1alpha1.SwiftSandboxSuspended:
		res, err := r.reconcileSuspended(ctx, sb)
		return true, res, err
	}
	if !sb.Spec.Suspended() {
		return false, ctrl.Result{}, nil
	}
	pod, err := r.launcherPod(ctx, sb)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	// Created Suspended, or suspended before it ever ran: not started.
	if pod == nil && sb.Status.StartedAt == nil {
		if sb.Status.Phase == sandboxv1alpha1.SwiftSandboxPending && sb.Status.Message == notStartedMessage {
			return true, ctrl.Result{}, nil
		}
		res, err := r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxPending, notStartedMessage)
		return true, res, err
	}
	if pod == nil || sb.Status.Phase != sandboxv1alpha1.SwiftSandboxRunning || suspendSettled(sb) {
		return false, ctrl.Result{}, nil
	}
	res, err := r.startSuspend(ctx, sb, pod)
	return true, res, err
}

const notStartedMessage = "not started: spec.operatingMode is Suspended"

// suspendSettled: this generation's suspend was already refused or failed;
// it is not tried again until the spec changes.
func suspendSettled(sb *sandboxv1alpha1.SwiftSandbox) bool {
	c := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionSuspended)
	return c != nil && c.ObservedGeneration == sb.Generation &&
		(c.Reason == reasonSuspendUnsupported || c.Reason == reasonSuspendFailed)
}

// launcherPod is the sandbox's launcher pod, or nil when there is none.
func (r *SwiftSandboxReconciler) launcherPod(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (*corev1.Pod, error) {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: sb.Namespace, Name: firstNonEmpty(sb.Status.PodRef, sb.Name)}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(&pod, sb) {
		return nil, nil
	}
	return &pod, nil
}

// suspendRefusal names why this running sandbox cannot be suspended.
func suspendRefusal(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) string {
	if why := sandboxwebhook.SuspendUnsupported(&sb.Spec); why != "" {
		return why
	}
	if !strings.Contains(" "+pod.Annotations[bridgeFeaturesAnnotation]+" ", " "+bridgeFeatureResumeSync+" ") {
		return "the kernel's bridge lacks " + bridgeFeatureResumeSync + ", which a resumed guest needs: use kernels/sandbox 6.6.25 or gpu-sandbox 6.6.15 or later"
	}
	return ""
}

// startSuspend records where the state will go, then asks the launcher to
// write it. Recorded first: anything that creates node state is written
// down before it exists, so deleting the sandbox can always find it.
func (r *SwiftSandboxReconciler) startSuspend(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) (ctrl.Result, error) {
	if !launcherReady(pod) {
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}
	if why := suspendRefusal(sb, pod); why != "" {
		return r.suspendNotDone(ctx, sb, reasonSuspendUnsupported, why)
	}
	if err := patchFinalizers(ctx, r.Client, sb, func() { controllerutil.AddFinalizer(sb, sandboxSuspendFinalizer) }); err != nil {
		return ctrl.Result{}, err
	}
	sb.Status.SuspendedState = &sandboxv1alpha1.SandboxSuspendedState{
		NodeName:          pod.Spec.NodeName,
		Path:              suspendDir(sb),
		LauncherImage:     launcherImage(pod),
		HypervisorVersion: pod.Annotations[annHypervisorVersion],
	}
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionSuspended, Status: metav1.ConditionFalse,
		Reason: reasonSuspending, Message: "writing the microVM's state to node " + pod.Spec.NodeName,
		ObservedGeneration: sb.Generation,
	})
	if _, err := r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxSuspending, "suspending"); err != nil {
		return ctrl.Result{}, err
	}
	args, err := json.Marshal(map[string]any{
		"destination_url": "file://" + suspendDir(sb) + "/",
		"timeout_seconds": int(suspendActionTimeout.Seconds()),
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[annSnapshotAction] = "suspend"
	pod.Annotations[annSnapshotActionID] = suspendActionID(sb)
	pod.Annotations[annSnapshotActionArgs] = string(args)
	if err := r.Patch(ctx, pod, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// reconcileSuspending waits for the launcher's answer.
func (r *SwiftSandboxReconciler) reconcileSuspending(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ctrl.Result, error) {
	pod, err := r.launcherPod(ctx, sb)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod == nil {
		return r.fail(ctx, sb, reasonSuspendFailed, "the launcher stopped before the microVM's state was written")
	}
	id := suspendActionID(sb)
	if pod.Annotations[annSnapshotStatusID] == id {
		switch pod.Annotations[annSnapshotStatus] {
		case "ready":
			return r.suspendDone(ctx, sb, pod)
		case "failed", "rejected":
			// The launcher resumed the microVM and removed what it wrote.
			if err := patchFinalizers(ctx, r.Client, sb, func() { controllerutil.RemoveFinalizer(sb, sandboxSuspendFinalizer) }); err != nil {
				return ctrl.Result{}, err
			}
			sb.Status.SuspendedState = nil
			msg := "suspend failed: " + firstNonEmpty(pod.Annotations[annSnapshotStatusDetail], "no detail")
			if _, err := r.suspendNotDone(ctx, sb, reasonSuspendFailed, msg); err != nil {
				return ctrl.Result{}, err
			}
			return r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxRunning, "guest running")
		}
	}
	if c := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionSuspended); c != nil &&
		time.Since(c.LastTransitionTime.Time) > suspendActionTimeout+suspendWaitSlack {
		// Paused or not, the microVM's state is unknown: stop it.
		_ = r.Delete(ctx, pod)
		return r.fail(ctx, sb, reasonSuspendFailed, "the launcher did not report the suspend in time")
	}
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// suspendDone records the written state, then stops the launcher.
func (r *SwiftSandboxReconciler) suspendDone(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) (ctrl.Result, error) {
	var st suspendState
	_ = json.Unmarshal([]byte(pod.Annotations[annSnapshotStatusDetail]), &st)
	if sb.Status.SuspendedState == nil {
		sb.Status.SuspendedState = &sandboxv1alpha1.SandboxSuspendedState{NodeName: pod.Spec.NodeName, Path: suspendDir(sb)}
	}
	ss := sb.Status.SuspendedState
	ss.SizeBytes = st.SizeBytes
	ss.GuestIP = firstNonEmpty(st.GuestIP, networkIP(sb))
	ss.HypervisorVersion = firstNonEmpty(st.HypervisorVersion, ss.HypervisorVersion)
	taken := metav1.Now()
	if st.TakenAtUnix > 0 {
		taken = metav1.NewTime(time.Unix(st.TakenAtUnix, 0))
	}
	ss.TakenAt = &taken
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionSuspended, Status: metav1.ConditionTrue,
		Reason: reasonSuspended, Message: fmt.Sprintf("state on node %s (%d bytes)", ss.NodeName, ss.SizeBytes),
		ObservedGeneration: sb.Generation,
	})
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionGuestRunning, Status: metav1.ConditionFalse,
		Reason: reasonSuspended, Message: "suspended", ObservedGeneration: sb.Generation,
	})
	if apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionWorkloadReady) != nil {
		apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
			Type: sandboxv1alpha1.SwiftSandboxConditionWorkloadReady, Status: metav1.ConditionFalse,
			Reason: reasonSuspended, Message: "suspended", ObservedGeneration: sb.Generation,
		})
	}
	if _, err := r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxSuspended, "suspended to node "+ss.NodeName); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Event(sb, corev1.EventTypeNormal, reasonSuspended, fmt.Sprintf("suspended to node %s (%d bytes)", ss.NodeName, ss.SizeBytes))
	}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// reconcileSuspended keeps a suspended sandbox without a launcher.
func (r *SwiftSandboxReconciler) reconcileSuspended(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ctrl.Result, error) {
	pod, err := r.launcherPod(ctx, sb)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod != nil {
		if pod.DeletionTimestamp == nil {
			if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}
	if !sb.Spec.Suspended() {
		return r.startResume(ctx, sb)
	}
	return ctrl.Result{}, nil
}

// startResume moves a suspended sandbox to Resuming; the launch path then
// builds a launcher that restores it (resumeLaunch). spec.timeout counts
// running time only, so StartedAt moves forward by the suspended interval.
func (r *SwiftSandboxReconciler) startResume(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ctrl.Result, error) {
	ss := sb.Status.SuspendedState
	if ss == nil || ss.NodeName == "" {
		return r.snapshotLost(ctx, sb, "no saved state is recorded")
	}
	gone, err := r.nodeGone(ctx, ss.NodeName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if gone {
		return r.snapshotLost(ctx, sb, "node "+ss.NodeName+" that holds the saved state is gone")
	}
	if sb.Status.StartedAt != nil && ss.TakenAt != nil {
		if away := time.Since(ss.TakenAt.Time); away > 0 {
			moved := metav1.NewTime(sb.Status.StartedAt.Add(away))
			sb.Status.StartedAt = &moved
		}
	}
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionSuspended, Status: metav1.ConditionFalse,
		Reason: reasonResuming, Message: "restoring the microVM on node " + ss.NodeName,
		ObservedGeneration: sb.Generation,
	})
	if _, err := r.setPhase(ctx, sb, sandboxv1alpha1.SwiftSandboxResuming, "resuming on node "+ss.NodeName); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// snapshotLost fails a sandbox whose saved state cannot be restored. The
// finalizer stays: deleting the sandbox still removes what is on the node.
func (r *SwiftSandboxReconciler) snapshotLost(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, msg string) (ctrl.Result, error) {
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionSuspended, Status: metav1.ConditionFalse,
		Reason: reasonSnapshotLost, Message: msg, ObservedGeneration: sb.Generation,
	})
	if r.Recorder != nil {
		r.Recorder.Event(sb, corev1.EventTypeWarning, reasonSnapshotLost, msg)
	}
	return r.fail(ctx, sb, reasonSnapshotLost, msg)
}

// resuming: the launch path is building, or watching, a launcher that
// restores the sandbox.
func resuming(sb *sandboxv1alpha1.SwiftSandbox) bool {
	return sb.Status.Phase == sandboxv1alpha1.SwiftSandboxResuming && sb.Status.SuspendedState != nil
}

// pinnedImage is spec.image by the digest the sandbox was suspended with: a
// tag that moved since must not change the disk under the restored guest.
func pinnedImage(sb *sandboxv1alpha1.SwiftSandbox) (string, error) {
	if sb.Status.Rootfs == nil || sb.Status.Rootfs.Digest == "" {
		return "", fmt.Errorf("no rootfs digest is recorded")
	}
	ref, err := name.ParseReference(sb.Spec.Image)
	if err != nil {
		return "", err
	}
	return ref.Context().Digest(sb.Status.Rootfs.Digest).String(), nil
}

// resumeDrift names content the restored guest would find changed: a model
// or artifact that now resolves to another digest than it had.
func resumeDrift(sb *sandboxv1alpha1.SwiftSandbox, modelDigest string, arts []sandboxv1alpha1.SandboxArtifactStatus) string {
	if sb.Status.Model != nil && modelDigest != "" && sb.Status.Model.Digest != modelDigest {
		return fmt.Sprintf("model %s resolves to %s now, not %s", sb.Spec.Model.ImageRef, modelDigest, sb.Status.Model.Digest)
	}
	had := map[string]string{}
	for _, a := range sb.Status.Artifacts {
		had[a.Name] = a.Digest
	}
	for _, a := range arts {
		if d, ok := had[a.Name]; ok && d != a.Digest {
			return fmt.Sprintf("artifact %s resolves to %q now, not %q", a.Name, a.Digest, d)
		}
	}
	return ""
}

// resumeLaunch turns a launcher built for a cold start into one that
// restores the suspended state: the restore in its intent, and the launcher
// image that wrote the state.
func resumeLaunch(sb *sandboxv1alpha1.SwiftSandbox, intent *runtimeintent.RuntimeIntent) {
	intent.Restore = &runtimeintent.RestoreIntent{
		SnapshotPath:      sb.Status.SuspendedState.Path,
		AutoResume:        true,
		MemoryRestoreMode: "copy",
	}
}

// resumeLauncherImage puts the recorded launcher image on every container
// that runs the current one (launcher, network-init).
func resumeLauncherImage(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) {
	want := sb.Status.SuspendedState.LauncherImage
	if want == "" {
		return
	}
	cur := swiftguest.LauncherImage()
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Image == cur {
			pod.Spec.InitContainers[i].Image = want
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Image == cur {
			pod.Spec.Containers[i].Image = want
		}
	}
}

// annResumed is swiftletd's report of the restored guest's resume-sync
// (suspend.rs): {"ok":..., "synced":[...], "clockOffsetMs":..., "error":...}.
const annResumed = "kubeswift.io/sandbox-resumed"

// resumeSyncWait bounds how long a restored, running launcher may take to
// report resume-sync before the sandbox goes on without it.
const resumeSyncWait = 2 * time.Minute

type resumeResult struct {
	OK            bool     `json:"ok"`
	Error         string   `json:"error"`
	Synced        []string `json:"synced"`
	ClockOffsetMs *int64   `json:"clockOffsetMs"`
}

// resumeSettled decides, for a restored launcher that is running, whether
// the resume is done; it then records it and returns true.
func (r *SwiftSandboxReconciler) resumeSettled(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) bool {
	raw, ok := pod.Annotations[annResumed]
	c := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionSuspended)
	if !ok && (c == nil || time.Since(c.LastTransitionTime.Time) < resumeSyncWait) {
		return false
	}
	var res resumeResult
	if ok {
		_ = json.Unmarshal([]byte(raw), &res)
	} else {
		res.Error = "the launcher did not report the guest's clock sync"
	}
	reason, msg := reasonResumed, "resumed on node "+pod.Spec.NodeName
	if res.ClockOffsetMs != nil {
		msg += fmt.Sprintf("; guest clock was %d ms off", *res.ClockOffsetMs)
	}
	if !res.OK {
		reason, msg = reasonResumeSyncFailed, "resumed, but the guest's clock and entropy were not reset: "+firstNonEmpty(res.Error, "no detail")
		if r.Recorder != nil {
			r.Recorder.Event(sb, corev1.EventTypeWarning, reason, msg)
		}
	} else if r.Recorder != nil {
		r.Recorder.Event(sb, corev1.EventTypeNormal, reasonResumed, msg)
	}
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionSuspended, Status: metav1.ConditionFalse,
		Reason: reason, Message: msg, ObservedGeneration: sb.Generation,
	})
	return true
}

// restoreError is why a restoring launcher could not start the guest, as
// swiftletd reported it ("" when it did not).
func restoreError(pod *corev1.Pod) string {
	var res resumeResult
	if raw, ok := pod.Annotations[annResumed]; !ok || json.Unmarshal([]byte(raw), &res) != nil || res.OK {
		return ""
	}
	// swiftletd prefixes a restore that never started the guest; anything
	// else is a guest that runs but was not synced.
	if !strings.HasPrefix(res.Error, restoreErrorPrefix) {
		return ""
	}
	return res.Error
}

const restoreErrorPrefix = "restore: "

// finishResume: the restored sandbox runs; the saved state is gone (swiftletd
// removes it once the guest is synced), so nothing on the node is left to clean.
func (r *SwiftSandboxReconciler) finishResume(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) error {
	// The patch refreshes sb from the server, status included: keep the
	// status this pass has set (the Resumed condition) for the write after.
	status := sb.Status.DeepCopy()
	if err := patchFinalizers(ctx, r.Client, sb, func() { controllerutil.RemoveFinalizer(sb, sandboxSuspendFinalizer) }); err != nil {
		return err
	}
	sb.Status = *status
	sb.Status.SuspendedState = nil
	return nil
}

// suspendNotDone records why spec.operatingMode Suspended did not take
// effect for this generation; the sandbox keeps running.
func (r *SwiftSandboxReconciler) suspendNotDone(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, reason, msg string) (ctrl.Result, error) {
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionSuspended, Status: metav1.ConditionFalse,
		Reason: reason, Message: msg, ObservedGeneration: sb.Generation,
	})
	if r.Recorder != nil {
		r.Recorder.Event(sb, corev1.EventTypeWarning, reason, msg)
	}
	if err := r.Status().Update(ctx, sb); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

func launcherImage(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		if c.Name == "launcher" {
			return c.Image
		}
	}
	return ""
}

func networkIP(sb *sandboxv1alpha1.SwiftSandbox) string {
	if sb.Status.Network == nil {
		return ""
	}
	return sb.Status.Network.PrimaryIP
}

// deleteSuspendState is the deletion path for a sandbox that wrote state to
// a node: a Job there removes the directory, bounded, then the finalizer goes.
func (r *SwiftSandboxReconciler) deleteSuspendState(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (bool, error) {
	done, err := r.cleanSuspendState(ctx, sb)
	if err != nil {
		return false, err
	}
	if !done && sb.DeletionTimestamp != nil && time.Since(sb.DeletionTimestamp.Time) < outputCleanupTimeout+time.Minute {
		return false, nil
	}
	return true, patchFinalizers(ctx, r.Client, sb, func() { controllerutil.RemoveFinalizer(sb, sandboxSuspendFinalizer) })
}

func (r *SwiftSandboxReconciler) cleanSuspendState(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (bool, error) {
	if r.SystemNamespace == "" || sb.Status.SuspendedState == nil {
		return true, nil
	}
	node := sb.Status.SuspendedState.NodeName
	gone, err := r.nodeGone(ctx, node)
	if err != nil {
		return false, err
	}
	if gone {
		if r.Recorder != nil {
			r.Recorder.Event(sb, corev1.EventTypeWarning, "SuspendCleanupSkipped", "node "+node+" is gone; its saved state goes with it")
		}
		return true, nil
	}
	name := names.JobName(string(sb.UID), "-suspend-clean")
	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Namespace: r.SystemNamespace, Name: name}, &job)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, buildSuspendCleanupJob(sb, name, r.SystemNamespace, SandboxMaterializeImage())); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, nil
		case batchv1.JobFailed:
			if r.Recorder != nil {
				r.Recorder.Event(sb, corev1.EventTypeWarning, "SuspendCleanupFailed", "could not remove "+suspendDir(sb)+" on node "+node+": "+c.Message)
			}
			return true, nil
		}
	}
	return false, nil
}

// buildSuspendCleanupJob removes one sandbox's suspend directory on its node
// (sandbox-materialize clean-suspend, which checks the UID again).
func buildSuspendCleanupJob(sb *sandboxv1alpha1.SwiftSandbox, name, namespace, image string) *batchv1.Job {
	labels := exportLabels(sb, suspendCleanupComponent, "")
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, Labels: labels,
			Annotations: map[string]string{exportForAnnotation: sb.Namespace + "/" + sb.Name},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(2)),
			ActiveDeadlineSeconds:   ptr.To(int64(outputCleanupTimeout.Seconds())),
			TTLSecondsAfterFinished: ptr.To(int32(outputCleanupTTL.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					NodeName:                     sb.Status.SuspendedState.NodeName,
					AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{{
						Name:  "clean",
						Image: image,
						Args:  []string{"clean-suspend", "--root=" + suspendCleanupMount, "--sandbox-uid=" + string(sb.UID)},
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:                ptr.To(int64(0)),
							RunAsNonRoot:             ptr.To(false),
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "suspend", MountPath: suspendCleanupMount}},
					}},
					Volumes: []corev1.Volume{{Name: "suspend", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
						Path: SuspendRoot, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}}},
				},
			},
		},
	}
}
