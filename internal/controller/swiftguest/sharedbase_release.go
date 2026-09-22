package swiftguest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sharedbase"
)

// Deleting a shared-base guest. Its disk is a thin device in its node's pool,
// outside Kubernetes, so nothing else frees it: without this, every deleted
// guest would leave everything it wrote allocated in the pool for good.
//
// The guest carries SharedBaseDiskFinalizer from before anything of it exists
// on a node. When it is deleted, the controller stops everything using the
// disk, runs a release Job on the disk's node, and lets the guest go once that
// has succeeded.

const (
	// SharedBaseDiskFinalizer holds a shared-base guest until its disk has been
	// released on its node.
	SharedBaseDiskFinalizer = "kubeswift.io/shared-base-disk"

	releaseJobPrefix = "basedisk-release-"

	reasonBaseDiskReleasing     = "BaseDiskReleasing"
	reasonBaseDiskReleaseFailed = "BaseDiskReleaseFailed"

	// controllerNamespaceEnv names the controller's own namespace; the chart
	// sets it from the downward API. A release Job runs there when the guest's
	// namespace is being deleted and accepts no new objects.
	controllerNamespaceEnv = "POD_NAMESPACE"

	releasePoll = 5 * time.Second
)

// ReleaseJobName is the name of a guest's release Job. By UID, because the Job
// may run in the controller's namespace, shared by every guest in the cluster.
func ReleaseJobName(guest *swiftv1alpha1.SwiftGuest) string {
	return releaseJobPrefix + string(guest.UID)
}

// ensureSharedBaseFinalizer makes deleting the guest release its disk.
func (r *SwiftGuestReconciler) ensureSharedBaseFinalizer(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) error {
	if guest.DeletionTimestamp != nil || controllerutil.ContainsFinalizer(guest, SharedBaseDiskFinalizer) {
		return nil
	}
	patch := client.MergeFromWithOptions(guest.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.AddFinalizer(guest, SharedBaseDiskFinalizer)
	return r.Patch(ctx, guest, patch)
}

// reconcileDeletion handles a guest that is being deleted. It never falls
// through to the normal path, whose job is to give a guest without a launcher a
// new one.
func (r *SwiftGuestReconciler) reconcileDeletion(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (ctrl.Result, error) {
	// Stop the VM. The launcher is owned by the guest, but the garbage
	// collector keeps an owner's dependents for as long as the owner exists —
	// measured: a pod owned by a deleted object that a finalizer holds stays
	// Running — so while any finalizer holds the guest, nothing else stops it.
	launchers, err := r.deleteLauncherPods(ctx, guest)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !controllerutil.ContainsFinalizer(guest, SharedBaseDiskFinalizer) {
		return ctrl.Result{}, nil
	}
	return r.releaseSharedBaseDisk(ctx, guest, launchers)
}

// deleteLauncherPods deletes the guest's launcher pods and returns the names of
// those that still exist, terminating or not. Selected by the guest label, which
// a migration's renamed target pod carries too.
func (r *SwiftGuestReconciler) deleteLauncherPods(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) ([]string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(guest.Namespace),
		client.MatchingLabels{guestPodLabelKey: guest.Name}); err != nil {
		return nil, err
	}
	var names []string
	for i := range pods.Items {
		p := &pods.Items[i]
		names = append(names, p.Name)
		if p.DeletionTimestamp == nil {
			if err := r.Delete(ctx, p); client.IgnoreNotFound(err) != nil {
				return nil, err
			}
		}
	}
	return names, nil
}

// releaseSharedBaseDisk frees the guest's disk on its node and then lets the
// guest go. A pass that cannot finish says on StorageReady what it waits for.
func (r *SwiftGuestReconciler) releaseSharedBaseDisk(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, launchers []string) (ctrl.Result, error) {
	status := guest.Status.DeepCopy()

	matPod, err := r.jobPod(ctx, guest.Namespace, MaterialiseJobName(guest))
	if err != nil {
		return ctrl.Result{}, err
	}
	node := ""
	if sb := status.SharedBaseDisk; sb != nil {
		node = sb.Node
	}
	if node == "" && matPod != nil && matPod.Spec.NodeName != "" {
		// The node is recorded on the pass AFTER the materialise pod binds, so a
		// guest deleted in between has a disk on a node nothing recorded. The
		// pod still knows; record it before deleting the Job takes that away.
		node = matPod.Spec.NodeName
		if status.SharedBaseDisk == nil {
			status.SharedBaseDisk = &swiftv1alpha1.SharedBaseDiskStatus{}
		}
		status.SharedBaseDisk.Node = node
		if err := r.patchStatus(ctx, guest, status); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		// patchStatus leaves guest.Status sharing status's slices; a later
		// condition change on status would then compare equal and not be sent.
		status = guest.Status.DeepCopy()
	}
	if node == "" {
		// A pinned guest's materialise Job is created bound to the pin, so if
		// one ran and its pod is already gone, it ran there. A release on a node
		// that holds nothing of the guest's does nothing.
		if pinned, _, err := pinnedNode(guest); err == nil {
			node = pinned
		}
	}

	// Nothing may use the disk while it is released: not the launcher, and not
	// a materialise Job still building it.
	if err := r.deleteJob(ctx, guest.Namespace, MaterialiseJobName(guest)); err != nil {
		return ctrl.Result{}, err
	}
	busy := launchers
	if matPod != nil && !podFinished(matPod) {
		busy = append(busy, matPod.Name)
	}
	if len(busy) > 0 {
		return r.releaseWaiting(ctx, guest, status,
			"releasing the shared-base root disk: waiting for pod "+strings.Join(busy, ", ")+" to exit")
	}

	if node == "" {
		// The materialise Job's pod never bound to a node, so no node holds
		// anything of this guest's.
		return r.dropSharedBaseFinalizer(ctx, guest)
	}
	var n corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: node}, &n); apierrors.IsNotFound(err) {
		// The node has left the cluster, and its pool went with it: there is
		// nowhere to run a release and nothing reachable to release.
		log.FromContext(ctx).Info("shared-base root disk not released: its node no longer exists",
			"guest", guest.Namespace+"/"+guest.Name, "node", node)
		return r.dropSharedBaseFinalizer(ctx, guest)
	} else if err != nil {
		return ctrl.Result{}, err
	}

	job, err := r.findReleaseJob(ctx, guest)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job == nil {
		if job, err = r.createReleaseJob(ctx, guest, node); err != nil {
			return ctrl.Result{}, err
		}
	}
	where := job.Namespace + "/" + job.Name

	switch {
	case isJobComplete(job):
		// The finalizer first: were the Job deleted first and the finalizer
		// update then lost to a conflict, the retry would find no Job and run a
		// second release. Harmless, but pointless.
		res, err := r.dropSharedBaseFinalizer(ctx, guest)
		if err != nil {
			return res, err
		}
		// Best effort; the Job's TTL removes one this misses.
		_ = r.deleteJob(ctx, job.Namespace, job.Name)
		return res, nil

	case isJobFailed(job):
		pod, err := r.jobPod(ctx, job.Namespace, job.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		// Not retried automatically: the Job already retried in place, and what
		// is left — a read-only pool, a device something else holds open — needs
		// a person. Polled only to notice the Job being deleted.
		SetStorageReadyCondition(status, false, reasonBaseDiskReleaseFailed, fmt.Sprintf(
			"releasing the shared-base root disk on node %s failed: %s. Delete Job %s to retry. To give "+
				"the disk up instead, remove the finalizer %s; it then stays in the node's pool",
			node, terminationMessage(pod), where, SharedBaseDiskFinalizer))
		if err := r.patchStatus(ctx, guest, status); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{RequeueAfter: 6 * releasePoll}, nil
	}

	msg := fmt.Sprintf("releasing the shared-base root disk on node %s (Job %s)", node, where)
	if !nodeReady(&n) {
		msg += fmt.Sprintf("; node %s is not Ready, so it runs when the node returns", node)
	}
	if job.Status.Active == 0 && job.Status.Failed == 0 && time.Since(job.CreationTimestamp.Time) > time.Minute {
		// A Job whose pod was refused never says so on the Job's status.
		msg += fmt.Sprintf("; the Job has not started a pod: `kubectl -n %s describe job %s` says why "+
			"(a namespace that does not admit privileged pods refuses it)", job.Namespace, job.Name)
	}
	return r.releaseWaiting(ctx, guest, status, msg)
}

func (r *SwiftGuestReconciler) releaseWaiting(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, status *swiftv1alpha1.SwiftGuestStatus, msg string) (ctrl.Result, error) {
	SetStorageReadyCondition(status, false, reasonBaseDiskReleasing, msg)
	if err := r.patchStatus(ctx, guest, status); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{RequeueAfter: releasePoll}, nil
}

func (r *SwiftGuestReconciler) dropSharedBaseFinalizer(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (ctrl.Result, error) {
	patch := client.MergeFromWithOptions(guest.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.RemoveFinalizer(guest, SharedBaseDiskFinalizer)
	if err := r.Patch(ctx, guest, patch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// deleteJob deletes a Job and, in the background, its pods.
func (r *SwiftGuestReconciler) deleteJob(ctx context.Context, namespace, name string) error {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	return client.IgnoreNotFound(r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)))
}

// releaseNamespaces are where a guest's release Job can be: its own namespace,
// or the controller's.
func releaseNamespaces(guest *swiftv1alpha1.SwiftGuest) []string {
	out := []string{guest.Namespace}
	if ns := os.Getenv(controllerNamespaceEnv); ns != "" && ns != guest.Namespace {
		out = append(out, ns)
	}
	return out
}

func (r *SwiftGuestReconciler) findReleaseJob(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*batchv1.Job, error) {
	for _, ns := range releaseNamespaces(guest) {
		var job batchv1.Job
		err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ReleaseJobName(guest)}, &job)
		if err == nil {
			return &job, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return nil, nil
}

// createReleaseJob creates the release Job in the guest's namespace, which is
// known to admit the privileged pod it runs: the guest's launcher ran there. A
// namespace being deleted admits no new objects, though, and deleting its
// namespace is an ordinary way to delete a guest, so the Job then runs in the
// controller's namespace instead.
func (r *SwiftGuestReconciler) createReleaseJob(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, node string) (*batchv1.Job, error) {
	job := releaseJob(guest, node, guest.Namespace)
	err := r.Create(ctx, job)
	if apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
		ns := os.Getenv(controllerNamespaceEnv)
		if ns == "" || ns == guest.Namespace {
			return nil, fmt.Errorf("namespace %s is being deleted and accepts no release Job, and %s does not name "+
				"another namespace to run it in: %w", guest.Namespace, controllerNamespaceEnv, err)
		}
		job = releaseJob(guest, node, ns)
		err = r.Create(ctx, job)
	}
	if apierrors.IsAlreadyExists(err) {
		err = r.Get(ctx, client.ObjectKeyFromObject(job), job)
	}
	if err != nil {
		return nil, err
	}
	return job, nil
}

// releaseJob builds the Job that frees a guest's disk on its node.
//
// No owner reference, deliberately. One cannot cross namespaces, and in the
// guest's own namespace it would let a foreground deletion of the guest
// garbage-collect the very Job releasing its disk. The controller deletes the
// Job once it has succeeded.
func releaseJob(guest *swiftv1alpha1.SwiftGuest, node, namespace string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ReleaseJobName(guest),
			Namespace: namespace,
			// On the Job, not its pod template: launcher lookups select pods by
			// swift.kubeswift.io/guest.
			Labels: map[string]string{
				"swift.kubeswift.io/guest":           guest.Name,
				"swift.kubeswift.io/guest-namespace": guest.Namespace,
				"swift.kubeswift.io/role":            "root-disk-release",
			},
		},
		Spec: batchv1.JobSpec{
			// Retried in place: the pod is bound to the node, and a release is
			// idempotent, so a retry finishes what the last attempt started.
			BackoffLimit: ptr.To(int32(2)),
			// For a Job the controller never gets to delete: one whose guest's
			// finalizer was removed by hand.
			TTLSecondsAfterFinished: ptr.To(int32(3600)),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				NodeName:                     node,
				RestartPolicy:                corev1.RestartPolicyNever,
				AutomountServiceAccountToken: ptr.To(false),
				ImagePullSecrets:             LauncherImagePullSecrets(),
				// The disk is released wherever it is. A taint put on the node
				// since the guest was placed must not keep it allocated.
				Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
				Containers: []corev1.Container{{
					Name:            "release",
					Image:           LauncherImage(),
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{basediskCommand},
					Args: []string{
						"--mode=release",
						"--root=" + sharedbase.StateRoot,
						"--pool=" + sharedbase.Pool,
						"--guest-key=" + sharedbase.GuestKey(guest.Namespace, guest.Name, guest.UID),
						"--device=" + sharedbase.DeviceName(guest.UID),
					},
					SecurityContext:          privilegedContext(),
					TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					VolumeMounts:             nodeStateMounts(),
				}},
				Volumes: nodeStateVolumes(),
			}},
		},
	}
}

func podFinished(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
