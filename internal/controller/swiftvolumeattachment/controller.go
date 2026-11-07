// Package swiftvolumeattachment reconciles SwiftVolumeAttachments: it puts
// each attachment's volume on its guest's node with an attachment pod, and
// writes the guest's desired set of hot-plugged volumes onto the guest's
// launcher pod, which plugs them into Cloud Hypervisor (internal/hotplug).
//
// It reconciles per guest, not per attachment: the desired set is one
// annotation for all of a guest's attachments, so one pass computes it.
//
// The one safety rule: an attachment pod on the launcher's node is deleted
// only once the launcher has reported, for the set now on its pod, that it
// does not hold the volume (or the launcher is not running). Deleting it
// earlier would take the volume away from a running VM.
package swiftvolumeattachment

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/hotplug"
)

// GuestIndex indexes SwiftVolumeAttachments by spec.guestRef.name.
const GuestIndex = "spec.guestRef.name"

// launcherFeaturesAnnotation lists what a launcher supports.
const launcherFeaturesAnnotation = "kubeswift.io/launcher-features"

// guestLabel names the guest on its launcher pod.
const guestLabel = "swift.kubeswift.io/guest"

// unsettledRequeue re-checks a guest whose attachments are still moving, in
// case an event is missed. Pod and attachment events drive the real work.
const unsettledRequeue = 15 * time.Second

// Reconciler reconciles the SwiftVolumeAttachments of one guest.
type Reconciler struct {
	client.Client
	Recorder record.EventRecorder
}

// launcherView is what one pass knows of the guest's launcher.
type launcherView struct {
	pod *corev1.Pod
	// active: the launcher pod is Running, so its VM may hold disks.
	active bool
	// supports: the launcher converges on a desired set.
	supports bool
	// current is the desired set on the pod now.
	current map[string]hotplug.Volume
	// reported is the launcher's report.
	reported map[string]hotplug.Status
	// settled: the report reflects current.
	settled bool
}

// holds reports whether the launcher may hold the named volume: it is in
// the set, or in the report, or the report is behind the set.
func (l *launcherView) holds(name string) bool {
	if !l.active || !l.supports {
		return false
	}
	if _, ok := l.current[name]; ok {
		return true
	}
	if _, ok := l.reported[name]; ok {
		return true
	}
	return !l.settled
}

// outcome is what a pass decided for one attachment.
type outcome struct {
	entry *hotplug.Volume
	force bool
	// done: the attachment is deleting and fully released.
	done bool
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("guest", req.NamespacedName)
	var list swiftv1alpha1.SwiftVolumeAttachmentList
	if err := r.List(ctx, &list, client.InNamespace(req.Namespace), client.MatchingFields{GuestIndex: req.Name}); err != nil {
		return reconcile.Result{}, err
	}
	if len(list.Items) == 0 {
		return reconcile.Result{}, nil
	}
	svas := list.Items
	hotplug.SortAttachments(svas)

	var guest *swiftv1alpha1.SwiftGuest
	var g swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, req.NamespacedName, &g); err == nil {
		guest = &g
	} else if !apierrors.IsNotFound(err) {
		return reconcile.Result{}, err
	}
	l, err := r.launcher(ctx, guest)
	if err != nil {
		return reconcile.Result{}, err
	}

	// The namespace's attachments, for the sibling rules (other guests'
	// attachments of the same claim count).
	var all swiftv1alpha1.SwiftVolumeAttachmentList
	if err := r.List(ctx, &all, client.InNamespace(req.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	live := make([]swiftv1alpha1.SwiftVolumeAttachment, 0, len(all.Items))
	for _, o := range all.Items {
		if o.DeletionTimestamp.IsZero() {
			live = append(live, o)
		}
	}
	hotplug.SortAttachments(live)

	desired := []hotplug.Volume{}
	var force []string
	unsettled := false
	for i := range svas {
		sva := &svas[i]
		if sva.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(sva, swiftv1alpha1.VolumeDetachFinalizer) {
			if err := patchFinalizer(ctx, r.Client, sva, true); err != nil {
				return reconcile.Result{}, err
			}
		}
		orig := sva.DeepCopy()
		out, err := r.reconcileOne(ctx, sva, guest, l, earlierThan(live, sva))
		if err != nil {
			return reconcile.Result{}, err
		}
		if out.entry != nil {
			desired = append(desired, *out.entry)
		}
		if out.force {
			force = append(force, sva.Name)
		}
		sva.Status.ObservedGeneration = sva.Generation
		if !equality.Semantic.DeepEqual(orig.Status, sva.Status) {
			if err := r.Status().Patch(ctx, sva, client.MergeFrom(orig)); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return reconcile.Result{}, err
			}
		}
		if out.done {
			if err := patchFinalizer(ctx, r.Client, sva, false); err != nil {
				return reconcile.Result{}, err
			}
			r.event(sva, corev1.EventTypeNormal, "VolumeDetached", "volume %s detached from SwiftGuest %s", sva.Spec.PVCRef.Name, sva.Spec.GuestRef.Name)
			continue
		}
		if sva.Status.Phase != swiftv1alpha1.VolumeAttachmentAttached && sva.Status.Phase != swiftv1alpha1.VolumeAttachmentFailed {
			unsettled = true
		}
	}

	// The set goes onto the launcher only after every attachment pod it
	// names is recorded in its attachment's status (written above): an
	// unrecorded pod is an orphan the next pass deletes.
	if l.active && l.supports {
		if err := r.writeSet(ctx, l.pod, desired, force); err != nil {
			return reconcile.Result{}, err
		}
	}
	logger.V(1).Info("hotplug set", "volumes", len(desired), "forceDetach", force)
	if unsettled {
		return reconcile.Result{RequeueAfter: unsettledRequeue}, nil
	}
	return reconcile.Result{}, nil
}

func (r *Reconciler) launcher(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*launcherView, error) {
	l := &launcherView{current: map[string]hotplug.Volume{}, reported: map[string]hotplug.Status{}, settled: true}
	if guest == nil {
		return l, nil
	}
	pod, err := swiftguest.LauncherPod(ctx, r.Client, guest)
	if err != nil || pod == nil {
		return l, err
	}
	l.pod = pod
	l.active = pod.Status.Phase == corev1.PodRunning
	l.supports = hotplug.HasLauncherFeature(pod.Annotations[launcherFeaturesAnnotation])
	vols, err := hotplug.DecodeVolumes(pod.Annotations[hotplug.AnnotationVolumes])
	if err != nil {
		// Only this controller writes the set; one that does not parse is
		// replaced by the next write. Treat the launcher as unsettled so
		// nothing it might hold is released meanwhile.
		l.settled = false
	}
	for _, v := range vols {
		l.current[v.Name] = v
	}
	reported, err := hotplug.DecodeStatus(pod.Annotations[hotplug.AnnotationStatus])
	if err != nil {
		l.settled = false
	} else {
		l.reported = reported
		l.settled = l.settled && hotplug.Settled(pod.Annotations)
	}
	return l, nil
}

// reconcileOne decides one attachment and updates its status in place.
func (r *Reconciler) reconcileOne(
	ctx context.Context,
	sva *swiftv1alpha1.SwiftVolumeAttachment,
	guest *swiftv1alpha1.SwiftGuest,
	l *launcherView,
	earlier []swiftv1alpha1.SwiftVolumeAttachment,
) (outcome, error) {
	st := &sva.Status
	st.Serial = swiftv1alpha1.HotplugSerial(sva.UID)

	if !sva.DeletionTimestamp.IsZero() {
		return r.detach(ctx, sva, l)
	}

	// Why the attachment is not plugged in, when it is not.
	phase, reason, msg := swiftv1alpha1.VolumeAttachmentPending, "", ""
	var pvc corev1.PersistentVolumeClaim
	switch {
	case guest == nil:
		reason, msg = swiftv1alpha1.VolumeAttachmentReasonGuestNotFound, fmt.Sprintf("SwiftGuest %s not found", sva.Spec.GuestRef.Name)
	case hotplug.GuestRefusal(guest) != "":
		phase, reason, msg = swiftv1alpha1.VolumeAttachmentFailed, swiftv1alpha1.VolumeAttachmentReasonVolumeUnsupported, hotplug.GuestRefusal(guest)
	}
	if reason == "" {
		err := r.Get(ctx, types.NamespacedName{Namespace: sva.Namespace, Name: sva.Spec.PVCRef.Name}, &pvc)
		switch {
		case apierrors.IsNotFound(err):
			reason, msg = swiftv1alpha1.VolumeAttachmentReasonPVCNotFound, fmt.Sprintf("PersistentVolumeClaim %s not found", sva.Spec.PVCRef.Name)
		case err != nil:
			return outcome{}, err
		}
	}
	if reason == "" {
		if m := hotplug.PVCRefusal(&pvc); m != "" {
			phase, reason, msg = swiftv1alpha1.VolumeAttachmentFailed, swiftv1alpha1.VolumeAttachmentReasonVolumeUnsupported, m
		} else if m := hotplug.SiblingRefusal(sva, earlier, &pvc); m != "" {
			phase, reason, msg = swiftv1alpha1.VolumeAttachmentFailed, swiftv1alpha1.VolumeAttachmentReasonVolumeUnsupported, m
		}
	}
	_, inSet := l.current[sva.Name]
	if reason == "" {
		switch {
		case !l.active:
			reason, msg = swiftv1alpha1.VolumeAttachmentReasonGuestNotRunning, fmt.Sprintf("SwiftGuest %s is not running", sva.Spec.GuestRef.Name)
		case !l.supports:
			reason, msg = swiftv1alpha1.VolumeAttachmentReasonLauncherUnsupported,
				fmt.Sprintf("the launcher of SwiftGuest %s does not support hot-plugged volumes (started before the upgrade, or the feature is off); restart the guest", guest.Name)
		case guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] != "" && !inSet:
			reason, msg = swiftv1alpha1.VolumeAttachmentReasonMigrationInProgress,
				fmt.Sprintf("SwiftMigration %s is moving SwiftGuest %s", guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress], guest.Name)
		}
	}
	if reason != "" {
		// Not plugged in. Free the volume once the launcher does not hold it.
		if !l.holds(sva.Name) {
			if _, err := r.syncAttachmentPods(ctx, sva, "", nil); err != nil {
				return outcome{}, err
			}
		}
		setPhase(sva, phase, reason, msg)
		return outcome{}, nil
	}

	node := l.pod.Spec.NodeName
	st.NodeName = node
	att, err := r.syncAttachmentPods(ctx, sva, node, l.pod)
	if err != nil {
		return outcome{}, err
	}
	if att == nil || att.Status.Phase != corev1.PodRunning {
		what := "being created"
		if att != nil {
			what = string(att.Status.Phase)
		}
		setPhase(sva, swiftv1alpha1.VolumeAttachmentAttaching, swiftv1alpha1.VolumeAttachmentReasonAttachingToNode,
			fmt.Sprintf("putting the volume on node %s: attachment pod %s", node, what))
		return outcome{}, nil
	}
	// The claim binds when the attachment pod first uses it (WaitForFirstConsumer).
	if err := r.Get(ctx, types.NamespacedName{Namespace: sva.Namespace, Name: sva.Spec.PVCRef.Name}, &pvc); err != nil {
		return outcome{}, err
	}
	if pvc.Spec.VolumeName == "" {
		setPhase(sva, swiftv1alpha1.VolumeAttachmentAttaching, swiftv1alpha1.VolumeAttachmentReasonPVCNotBound,
			fmt.Sprintf("PersistentVolumeClaim %s is not bound", pvc.Name))
		return outcome{}, nil
	}
	var pv corev1.PersistentVolume
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		return outcome{}, err
	}
	if m := hotplug.PVRefusal(&pv); m != "" {
		if !l.holds(sva.Name) {
			if _, err := r.syncAttachmentPods(ctx, sva, "", nil); err != nil {
				return outcome{}, err
			}
		}
		setPhase(sva, swiftv1alpha1.VolumeAttachmentFailed, swiftv1alpha1.VolumeAttachmentReasonVolumeUnsupported, m)
		return outcome{}, nil
	}
	st.PVName = pv.Name
	entry := &hotplug.Volume{
		Name:             sva.Name,
		Serial:           st.Serial,
		PVName:           pv.Name,
		AttachmentPodUID: att.UID,
		ReadOnly:         sva.Spec.ReadOnly,
	}

	rep, reported := l.reported[sva.Name]
	switch {
	case reported && l.settled && rep.State == hotplug.StateAttached:
		if sva.Status.Phase != swiftv1alpha1.VolumeAttachmentAttached {
			r.event(sva, corev1.EventTypeNormal, "VolumeAttached", "volume %s attached to SwiftGuest %s as virtio serial %s",
				sva.Spec.PVCRef.Name, sva.Spec.GuestRef.Name, st.Serial)
		}
		setPhase(sva, swiftv1alpha1.VolumeAttachmentAttached, swiftv1alpha1.VolumeAttachmentReasonAttached,
			fmt.Sprintf("the guest sees the disk as /dev/disk/by-id/virtio-%s", st.Serial))
	case reported && l.settled && rep.State == hotplug.StateFailed:
		if sva.Status.Phase != swiftv1alpha1.VolumeAttachmentFailed {
			r.event(sva, corev1.EventTypeWarning, rep.Reason, "%s", rep.Message)
		}
		setPhase(sva, swiftv1alpha1.VolumeAttachmentFailed, swiftv1alpha1.VolumeAttachmentReasonLauncherFailed,
			strings.TrimSpace(rep.Reason+": "+rep.Message))
	default:
		setPhase(sva, swiftv1alpha1.VolumeAttachmentAttaching, swiftv1alpha1.VolumeAttachmentReasonPlugging,
			"the launcher is plugging the disk into the guest")
	}
	setInUse(sva, metav1.ConditionFalse, swiftv1alpha1.VolumeAttachmentReasonNotInUse, "")
	return outcome{entry: entry}, nil
}

// detach takes a deleting attachment out of the set and frees its volume once
// the launcher has let go of it.
func (r *Reconciler) detach(ctx context.Context, sva *swiftv1alpha1.SwiftVolumeAttachment, l *launcherView) (outcome, error) {
	force := sva.Annotations[swiftv1alpha1.AnnotationForceDetach] == "true"
	if l.holds(sva.Name) {
		msg := "the launcher is unplugging the disk"
		rep, ok := l.reported[sva.Name]
		switch {
		case force:
			msg = forcedDetachMessage
			if readyMessage(sva) != forcedDetachMessage {
				r.event(sva, corev1.EventTypeWarning, "ForcedDetach", "detaching without asking the guest (%s=true)", swiftv1alpha1.AnnotationForceDetach)
			}
		case ok && rep.State == hotplug.StateInUse:
			if !apimeta.IsStatusConditionTrue(sva.Status.Conditions, swiftv1alpha1.VolumeAttachmentConditionInUse) {
				r.event(sva, corev1.EventTypeWarning, "DetachBlocked", "the guest still uses the disk: %s", rep.Message)
			}
			setInUse(sva, metav1.ConditionTrue, swiftv1alpha1.VolumeAttachmentReasonDetachBlocked, rep.Message)
			msg = "the guest still uses the disk; unmount it in the guest, or set " +
				swiftv1alpha1.AnnotationForceDetach + "=true on the attachment"
		}
		setPhase(sva, swiftv1alpha1.VolumeAttachmentDetaching, swiftv1alpha1.VolumeAttachmentReasonDetaching, msg)
		return outcome{force: force}, nil
	}
	setInUse(sva, metav1.ConditionFalse, swiftv1alpha1.VolumeAttachmentReasonNotInUse, "")
	remaining, err := r.syncAttachmentPods(ctx, sva, "", nil)
	if err != nil {
		return outcome{}, err
	}
	if remaining != nil || len(sva.Status.AttachmentPods) > 0 {
		setPhase(sva, swiftv1alpha1.VolumeAttachmentDetaching, swiftv1alpha1.VolumeAttachmentReasonDetaching,
			"removing the volume from the node")
		return outcome{}, nil
	}
	setPhase(sva, swiftv1alpha1.VolumeAttachmentDetaching, swiftv1alpha1.VolumeAttachmentReasonDetaching, "detached")
	return outcome{done: true}, nil
}

// syncAttachmentPods keeps exactly one attachment pod, on node, and deletes
// every other pod of the attachment. node "" deletes them all. It returns
// the kept pod (nil if none yet), or, for node "", a pod still terminating.
//
// A pod counts as the attachment's only if its UID is recorded in
// status.attachmentPods: the controller records the UID the create returned,
// and hands only recorded UIDs to the launcher. Anyone who can create pods
// can make one that claims the attachment as its owner.
func (r *Reconciler) syncAttachmentPods(ctx context.Context, sva *swiftv1alpha1.SwiftVolumeAttachment, node string, launcher *corev1.Pod) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(sva.Namespace),
		client.MatchingLabels{swiftv1alpha1.LabelVolumeAttachment: sva.Name}); err != nil {
		return nil, err
	}
	byUID := map[types.UID]*corev1.Pod{}
	for i := range pods.Items {
		byUID[pods.Items[i].UID] = &pods.Items[i]
	}
	var kept *corev1.Pod
	var terminating *corev1.Pod
	recorded := sva.Status.AttachmentPods[:0:0]
	for _, ap := range sva.Status.AttachmentPods {
		p, ok := byUID[ap.UID]
		if !ok {
			continue // gone
		}
		delete(byUID, ap.UID)
		failed := p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded
		if node != "" && ap.NodeName == node && kept == nil && !failed && p.DeletionTimestamp.IsZero() {
			kept = p
			recorded = append(recorded, ap)
			continue
		}
		if err := r.deletePod(ctx, p); err != nil {
			return nil, err
		}
		terminating = p
		recorded = append(recorded, ap) // until it is gone
	}
	// Unrecorded pods: orphans of a status write that failed, if they are
	// ours. A foreign pod with the label is never used; leave it.
	for _, p := range byUID {
		if metav1.IsControlledBy(p, sva) {
			if err := r.deletePod(ctx, p); err != nil {
				return nil, err
			}
		}
	}
	sva.Status.AttachmentPods = recorded
	if node == "" {
		return terminating, nil
	}
	if kept != nil {
		return kept, nil
	}
	pod := buildAttachmentPod(sva, launcher, launcherImage(launcher))
	if err := r.Create(ctx, pod); err != nil {
		return nil, fmt.Errorf("create attachment pod: %w", err)
	}
	sva.Status.AttachmentPods = append(sva.Status.AttachmentPods, swiftv1alpha1.VolumeAttachmentPod{
		Name: pod.Name, UID: pod.UID, NodeName: node,
	})
	return pod, nil
}

func (r *Reconciler) deletePod(ctx context.Context, p *corev1.Pod) error {
	if !p.DeletionTimestamp.IsZero() {
		return nil
	}
	err := r.Delete(ctx, p, client.Preconditions{UID: &p.UID})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}

// writeSet puts the desired set and the force-detach list onto the launcher
// pod, when they differ from what is there.
func (r *Reconciler) writeSet(ctx context.Context, pod *corev1.Pod, desired []hotplug.Volume, force []string) error {
	set, err := hotplug.EncodeVolumes(desired)
	if err != nil {
		return err
	}
	forceList := hotplug.EncodeForceDetach(force)
	cur, has := pod.Annotations[hotplug.AnnotationVolumes]
	unchanged := (has && cur == set) || (!has && len(desired) == 0)
	if unchanged && pod.Annotations[hotplug.AnnotationForceDetach] == forceList {
		return nil
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		hotplug.AnnotationVolumes:     set,
		hotplug.AnnotationForceDetach: nilIfEmpty(forceList),
	}}}
	return r.Patch(ctx, pod, client.RawPatch(types.MergePatchType, mustJSON(patch)))
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func earlierThan(live []swiftv1alpha1.SwiftVolumeAttachment, sva *swiftv1alpha1.SwiftVolumeAttachment) []swiftv1alpha1.SwiftVolumeAttachment {
	i := slices.IndexFunc(live, func(o swiftv1alpha1.SwiftVolumeAttachment) bool { return o.UID == sva.UID })
	if i < 0 {
		return live
	}
	return live[:i]
}

func (r *Reconciler) event(sva *swiftv1alpha1.SwiftVolumeAttachment, eventType, reason, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(sva, eventType, reason, format, args...)
	}
}

// SetupWithManager registers the controller. Every event is mapped to the
// guest its attachments name.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &swiftv1alpha1.SwiftVolumeAttachment{}, GuestIndex,
		func(o client.Object) []string {
			return []string{o.(*swiftv1alpha1.SwiftVolumeAttachment).Spec.GuestRef.Name}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("swiftvolumeattachment").
		Watches(&swiftv1alpha1.SwiftVolumeAttachment{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				sva := o.(*swiftv1alpha1.SwiftVolumeAttachment)
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: sva.Namespace, Name: sva.Spec.GuestRef.Name}}}
			})).
		Watches(&swiftv1alpha1.SwiftGuest{}, handler.EnqueueRequestsFromMapFunc(r.guestIfAttached)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.podToGuest)).
		Complete(r)
}

// podToGuest maps an attachment pod, or a launcher pod of a guest with
// attachments, to the guest.
func (r *Reconciler) podToGuest(ctx context.Context, o client.Object) []reconcile.Request {
	if g := o.GetLabels()[labelAttachmentGuest]; g != "" {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: g}}}
	}
	g := o.GetLabels()[guestLabel]
	if g == "" {
		return nil
	}
	return r.ifAttached(ctx, o.GetNamespace(), g)
}

func (r *Reconciler) guestIfAttached(ctx context.Context, o client.Object) []reconcile.Request {
	return r.ifAttached(ctx, o.GetNamespace(), o.GetName())
}

func (r *Reconciler) ifAttached(ctx context.Context, namespace, guest string) []reconcile.Request {
	var list swiftv1alpha1.SwiftVolumeAttachmentList
	if err := r.List(ctx, &list, client.InNamespace(namespace), client.MatchingFields{GuestIndex: guest}); err != nil || len(list.Items) == 0 {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: guest}}}
}
