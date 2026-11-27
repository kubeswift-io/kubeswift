package podmode

// Suspend and resume of an agent-sandbox Sandbox in pod mode.
//
// agent-sandbox suspends a Sandbox by deleting the Pod it owns, with nothing
// on the Pod to say why, and resumes it by creating a Pod of the same name (a
// new UID). Pod mode keeps the microVM across the two:
//
//   - The stand-in is held by its finalizer. When it is deleted while its
//     owner Sandbox exists, is not being deleted and has spec.operatingMode
//     Suspended, the controller suspends its SwiftSandbox instead of
//     deleting it, and lets the stand-in go once the SwiftSandbox is
//     Suspended. A SwiftSandbox that cannot be suspended (SuspendUnsupported,
//     SuspendFailed) is deleted as for any deletion: agent-sandbox's suspend
//     still completes, as a cold one.
//   - A suspended SwiftSandbox outlives its stand-in while its owner Sandbox
//     exists. Once the Sandbox is gone it is deleted, with its saved state.
//   - The new stand-in (same name, same owner Sandbox) claims it: the
//     SwiftSandbox's uid label becomes the new stand-in's, and the stand-in's
//     sandbox label names it. The pod-mode webhook pinned the new stand-in
//     to the node holding the saved state; the controller sets the
//     SwiftSandbox Running there.
//   - A microVM that cannot be resumed (the stand-in is on another node, the
//     saved state is lost) is deleted and the stand-in gets a new one, a cold
//     boot: agent-sandbox promises only that PVC data survives a suspend.

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

// suspendRecheck is how often a suspended microVM without a stand-in checks
// that its owner Sandbox still exists (the Sandbox is not watched: it is
// another project's CRD and need not be installed).
const suspendRecheck = 30 * time.Second

// Reasons of the stand-in's readiness gate.
const (
	reasonSuspending = "MicroVMSuspending"
	reasonResuming   = "MicroVMResuming"
)

// SandboxOf is the name of the stand-in's SwiftSandbox: its LabelSandbox,
// once the controller set it, else SandboxName.
func SandboxOf(pod *corev1.Pod) string {
	if n := pod.Labels[podmode.LabelSandbox]; n != "" {
		return n
	}
	return SandboxName(pod)
}

// owns is whether sb is the stand-in's: its uid label is the stand-in's UID
// (only the controller sets it, at creation or when a stand-in claims a
// suspended microVM) and it records the stand-in's namespace and name.
func owns(pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) bool {
	return sb.Labels[podmode.LabelUID] == string(pod.UID) &&
		sb.Annotations[podmode.AnnotationStandIn] == pod.Namespace+"/"+pod.Name
}

// sandboxNameFor is the name LabelSandbox should have: the SwiftSandbox the
// label names when that one is the stand-in's, else SandboxName. A label set
// by hand to another SwiftSandbox is not honoured.
func (r *Reconciler) sandboxNameFor(ctx context.Context, pod *corev1.Pod) (string, error) {
	name := pod.Labels[podmode.LabelSandbox]
	if name == "" || name == SandboxName(pod) {
		return SandboxName(pod), nil
	}
	var sb sandboxv1alpha1.SwiftSandbox
	err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name}, &sb)
	if apierrors.IsNotFound(err) {
		return SandboxName(pod), nil
	}
	if err != nil {
		return "", err
	}
	if owns(pod, &sb) {
		return name, nil
	}
	return SandboxName(pod), nil
}

type ownerState int

const (
	ownerGone ownerState = iota
	ownerRunning
	ownerSuspended
)

// ownerState reads the agent-sandbox Sandbox apiVersion/name with UID uid in
// namespace. It is gone when it does not exist, is another object of that
// name, is being deleted, or agent-sandbox is not installed.
func (r *Reconciler) ownerState(ctx context.Context, namespace, apiVersion, name string, uid types.UID) (ownerState, error) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(podmode.AgentSandboxKind)
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, u)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return ownerGone, nil
	}
	if err != nil {
		return ownerGone, err
	}
	if u.GetUID() != uid || u.GetDeletionTimestamp() != nil {
		return ownerGone, nil
	}
	if mode, _, _ := unstructured.NestedString(u.Object, "spec", "operatingMode"); mode == podmode.AgentSandboxSuspended {
		return ownerSuspended, nil
	}
	return ownerRunning, nil
}

// sandboxOwner is the owner Sandbox recorded on a SwiftSandbox suspended for
// it.
func sandboxOwner(sb *sandboxv1alpha1.SwiftSandbox) (apiVersion, name string, uid types.UID, ok bool) {
	v := sb.Annotations[podmode.AnnotationOwner]
	i := strings.LastIndex(v, "/")
	uid = types.UID(sb.Labels[podmode.LabelOwnerUID])
	if i <= 0 || i == len(v)-1 || uid == "" {
		return "", "", "", false
	}
	return v[:i], v[i+1:], uid, true
}

// held is whether a SwiftSandbox outlives its stand-in: it is suspended, or
// being suspended, for an agent-sandbox Sandbox that still exists. A suspend
// that failed leaves the SwiftSandbox Running: not held.
func (r *Reconciler) held(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (bool, error) {
	if !sb.Spec.Suspended() || sb.DeletionTimestamp != nil {
		return false, nil
	}
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxSuspended && sb.Status.Phase != sandboxv1alpha1.SwiftSandboxSuspending {
		return false, nil
	}
	apiVersion, name, uid, ok := sandboxOwner(sb)
	if !ok {
		return false, nil
	}
	ns := sb.Labels[podmode.LabelStandInNamespace]
	st, err := r.ownerState(ctx, ns, apiVersion, name, uid)
	return st != ownerGone, err
}

// suspendCondition is the SwiftSandbox's Suspended condition, nil when unset.
func suspendCondition(sb *sandboxv1alpha1.SwiftSandbox) *metav1.Condition {
	return meta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionSuspended)
}

// ingressByUID is whether the SwiftSandbox's ingress selects its stand-in by
// UID (made before stand-ins carried LabelSandbox): a resumed stand-in has
// another UID, so it could not reach its microVM.
func ingressByUID(sb *sandboxv1alpha1.SwiftSandbox) bool {
	if sb.Spec.Network.Ingress == nil {
		return false
	}
	for _, p := range sb.Spec.Network.Ingress.From {
		if p.PodSelector != nil {
			if _, ok := p.PodSelector.MatchLabels[podmode.LabelUID]; ok {
				return true
			}
		}
	}
	return false
}

// suspend handles a stand-in being deleted. handled is false when the
// deletion is not an agent-sandbox suspend or the microVM cannot be
// suspended: release then deletes the microVM, as for any deletion.
func (r *Reconciler) suspend(ctx context.Context, pod *corev1.Pod) (res ctrl.Result, handled bool, err error) {
	var sb sandboxv1alpha1.SwiftSandbox
	err = r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: SandboxOf(pod)}, &sb)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, false, nil
	}
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if !owns(pod, &sb) || sb.DeletionTimestamp != nil {
		return ctrl.Result{}, false, nil
	}

	if !sb.Spec.Suspended() {
		ref := podmode.AgentSandboxOwner(pod)
		if ref == nil || sb.Status.Phase != sandboxv1alpha1.SwiftSandboxRunning {
			return ctrl.Result{}, false, nil
		}
		st, err := r.ownerState(ctx, pod.Namespace, ref.APIVersion, ref.Name, ref.UID)
		if err != nil {
			return ctrl.Result{}, true, err
		}
		if st != ownerSuspended {
			return ctrl.Result{}, false, nil
		}
		why := sandboxwebhook.SuspendUnsupported(&sb.Spec)
		if why == "" && ingressByUID(&sb) {
			why = "its microVM was created before pod mode could resume one"
		}
		if why != "" {
			r.Recorder.Eventf(pod, corev1.EventTypeWarning, "MicroVMSuspendUnsupported",
				"the Sandbox is suspending but this microVM cannot be suspended (%s); deleting it", why)
			return ctrl.Result{}, false, nil
		}
		patch := client.MergeFrom(sb.DeepCopy())
		sb.Spec.OperatingMode = sandboxv1alpha1.SandboxOperatingSuspended
		if sb.Labels == nil {
			sb.Labels = map[string]string{}
		}
		sb.Labels[podmode.LabelOwnerUID] = string(ref.UID)
		sb.Annotations[podmode.AnnotationOwner] = ref.APIVersion + "/" + ref.Name
		if err := r.Patch(ctx, &sb, patch); err != nil {
			return ctrl.Result{}, true, err
		}
		r.Recorder.Eventf(pod, corev1.EventTypeNormal, reasonSuspending,
			"the Sandbox is suspending: suspending SwiftSandbox %s/%s", sb.Namespace, sb.Name)
		return ctrl.Result{RequeueAfter: deleteRequeue}, true,
			r.setReady(ctx, pod, false, reasonSuspending, "the microVM is being suspended")
	}

	// Being suspended. The decision stands unless the Sandbox is gone.
	apiVersion, name, uid, ok := sandboxOwner(&sb)
	if !ok {
		return ctrl.Result{}, false, nil
	}
	if st, err := r.ownerState(ctx, pod.Namespace, apiVersion, name, uid); err != nil || st == ownerGone {
		return ctrl.Result{}, err != nil, err
	}
	// Reasons of the SwiftSandbox controller (internal/controller/swiftsandbox
	// suspend.go); a condition from before this request does not count.
	if c := suspendCondition(&sb); c != nil && c.Status == metav1.ConditionFalse && c.ObservedGeneration == sb.Generation &&
		(c.Reason == "SuspendUnsupported" || c.Reason == "SuspendFailed") {
		r.Recorder.Eventf(pod, corev1.EventTypeWarning, "MicroVMSuspendFailed",
			"the microVM could not be suspended (%s: %s); deleting it", c.Reason, c.Message)
		return ctrl.Result{}, false, nil
	}
	switch sb.Status.Phase {
	case sandboxv1alpha1.SwiftSandboxSuspended:
		node := ""
		if sb.Status.SuspendedState != nil {
			node = sb.Status.SuspendedState.NodeName
		}
		r.Recorder.Eventf(pod, corev1.EventTypeNormal, "MicroVMSuspended",
			"SwiftSandbox %s/%s is suspended on node %s; it resumes with the Sandbox", sb.Namespace, sb.Name, node)
		if !controllerutil.ContainsFinalizer(pod, podmode.Finalizer) {
			return ctrl.Result{}, true, nil
		}
		patch := client.MergeFromWithOptions(pod.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.RemoveFinalizer(pod, podmode.Finalizer)
		return ctrl.Result{}, true, client.IgnoreNotFound(r.Patch(ctx, pod, patch))
	case sandboxv1alpha1.SwiftSandboxFailed, sandboxv1alpha1.SwiftSandboxCompleted:
		return ctrl.Result{}, false, nil
	}
	return ctrl.Result{RequeueAfter: deleteRequeue}, true,
		r.setReady(ctx, pod, false, reasonSuspending, "the microVM is being suspended")
}

// adopt has a new stand-in claim the suspended microVM of the same
// agent-sandbox Sandbox: same namespace, same name, same owner UID.
func (r *Reconciler) adopt(ctx context.Context, pod *corev1.Pod) error {
	if SandboxOf(pod) != SandboxName(pod) {
		return nil
	}
	ref := podmode.AgentSandboxOwner(pod)
	if ref == nil {
		return nil
	}
	var list sandboxv1alpha1.SwiftSandboxList
	if err := r.List(ctx, &list, client.InNamespace(r.Namespace), client.MatchingLabels{
		podmode.LabelStandInNamespace: pod.Namespace, podmode.LabelOwnerUID: string(ref.UID),
	}); err != nil {
		return err
	}
	for i := range list.Items {
		sb := &list.Items[i]
		if sb.Annotations[podmode.AnnotationStandIn] != pod.Namespace+"/"+pod.Name ||
			sb.DeletionTimestamp != nil || sb.Name == SandboxName(pod) {
			continue
		}
		claimed := sb.Labels[podmode.LabelUID] == string(pod.UID)
		if !claimed && (!sb.Spec.Suspended() || sb.Status.Phase != sandboxv1alpha1.SwiftSandboxSuspended) {
			continue
		}
		if !claimed {
			patch := client.MergeFromWithOptions(sb.DeepCopy(), client.MergeFromWithOptimisticLock{})
			sb.Labels[podmode.LabelUID] = string(pod.UID)
			sb.Annotations[podmode.AnnotationRun] = fmt.Sprint(RunOf(pod))
			if err := r.Patch(ctx, sb, patch); err != nil {
				return err
			}
		}
		patch := client.MergeFrom(pod.DeepCopy())
		pod.Labels[podmode.LabelSandbox] = sb.Name
		if err := r.Patch(ctx, pod, patch); err != nil {
			return err
		}
		r.Recorder.Eventf(pod, corev1.EventTypeNormal, "MicroVMClaimed",
			"resuming the suspended microVM of SwiftSandbox %s/%s", sb.Namespace, sb.Name)
		return nil
	}
	return nil
}

// resume moves a claimed microVM (sb is not SandboxName(pod)) back to
// Running. done is false once there is nothing left to do for the resume.
func (r *Reconciler) resume(ctx context.Context, pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) (res ctrl.Result, done bool, err error) {
	if sb.Spec.Suspended() {
		if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxSuspended {
			return ctrl.Result{RequeueAfter: deleteRequeue}, true,
				r.setReady(ctx, pod, false, reasonResuming, "waiting for the microVM to be suspended")
		}
		st := sb.Status.SuspendedState
		if st == nil || st.NodeName != pod.Spec.NodeName {
			at := "nowhere"
			if st != nil {
				at = "node " + st.NodeName
			}
			res, err := r.coldBoot(ctx, pod, sb, fmt.Sprintf("its saved state is on %s and this Pod on node %s", at, pod.Spec.NodeName))
			return res, true, err
		}
		patch := client.MergeFrom(sb.DeepCopy())
		sb.Spec.OperatingMode = sandboxv1alpha1.SandboxOperatingRunning
		if err := r.Patch(ctx, sb, patch); err != nil {
			return ctrl.Result{}, true, err
		}
		r.Recorder.Eventf(pod, corev1.EventTypeNormal, reasonResuming,
			"resuming SwiftSandbox %s/%s on node %s", sb.Namespace, sb.Name, st.NodeName)
		return ctrl.Result{}, true, r.setReady(ctx, pod, false, reasonResuming, "the microVM is being resumed")
	}
	if c := suspendCondition(sb); sb.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed &&
		c != nil && c.Reason == "SnapshotLost" {
		res, err := r.coldBoot(ctx, pod, sb, c.Message)
		return res, true, err
	}
	if sb.Status.Phase == sandboxv1alpha1.SwiftSandboxResuming {
		return ctrl.Result{}, true, r.setReady(ctx, pod, false, reasonResuming, "the microVM is being resumed")
	}
	return ctrl.Result{}, false, nil
}

// coldBoot drops a claimed microVM that cannot be resumed; the stand-in then
// gets a new one, as for any new Pod.
func (r *Reconciler) coldBoot(ctx context.Context, pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox, why string) (ctrl.Result, error) {
	r.Recorder.Eventf(pod, corev1.EventTypeWarning, "MicroVMResumeFailed",
		"SwiftSandbox %s/%s cannot be resumed (%s); starting a new microVM", sb.Namespace, sb.Name, why)
	if err := r.Delete(ctx, sb); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}
	patch := client.MergeFrom(pod.DeepCopy())
	pod.Labels[podmode.LabelSandbox] = SandboxName(pod)
	if err := r.Patch(ctx, pod, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, r.setReady(ctx, pod, false, "MicroVMStarting", "starting a new microVM: "+why)
}
