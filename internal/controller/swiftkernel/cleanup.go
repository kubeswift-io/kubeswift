package swiftkernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
)

// NodeCleanupFinalizer holds a SwiftKernel being deleted until its files are
// gone from every node it was pulled to. Deleting a SwiftKernel used to remove
// only the API object: its kernel and initramfs stayed under
// /var/lib/kubeswift/kernels/<namespace>/<name> on each of those nodes for
// good, and a later SwiftKernel of the same name found the old files there.
const NodeCleanupFinalizer = "kubeswift.io/swiftkernel-node-cleanup"

const (
	// cleanupImage only needs rm; the snapshot node cleanup uses the same.
	cleanupImage = "busybox:1.36.1"
	// cleanupMount is where kernelHostBasePath is mounted in the cleanup pod.
	cleanupMount = "/kernels"
	// cleanupPendingLimit is how long a cleanup pod may wait to run before
	// its node is given up on: a node that is down must not hold the kernel,
	// and with it a namespace being deleted, for good.
	cleanupPendingLimit = 5 * time.Minute

	cleanupPodPrefix  = "swift-kernel-cleanup-"
	maxCleanupPodName = 63

	cleanupRoleLabel     = "kernel.kubeswift.io/role"
	cleanupRoleValue     = "node-cleanup"
	kernelNameLabel      = "kernel.kubeswift.io/swiftkernel"
	kernelNamespaceLabel = "kernel.kubeswift.io/swiftkernel-namespace"
	kernelUIDLabel       = "kernel.kubeswift.io/swiftkernel-uid"

	// ReasonNodeCleanupSkipped is the Warning event a deletion emits for a
	// node whose kernel files it could not remove.
	ReasonNodeCleanupSkipped = "NodeCleanupSkipped"
	// ReasonKernelInUse is the event a deletion emits while running pods still
	// mount the kernel's directory.
	ReasonKernelInUse = "KernelInUse"

	// inUseRequeue is how often a deletion held by running pods looks again.
	inUseRequeue = 30 * time.Second
)

// ensureFinalizer adds NodeCleanupFinalizer to a SwiftKernel that is not being
// deleted. It updates sk in place, so a status write later in the same pass
// carries the new resourceVersion.
func (r *SwiftKernelReconciler) ensureFinalizer(ctx context.Context, sk *kernelv1alpha1.SwiftKernel) error {
	if sk.DeletionTimestamp != nil || controllerutil.ContainsFinalizer(sk, NodeCleanupFinalizer) {
		return nil
	}
	orig := sk.DeepCopy()
	controllerutil.AddFinalizer(sk, NodeCleanupFinalizer)
	return r.Patch(ctx, sk, client.MergeFrom(orig))
}

// kernelRelPath is the kernel's directory relative to kernelHostBasePath,
// "<namespace>/<name>", or false when it is not exactly that: the cleanup pod
// removes it with rm -rf, so it must never name the base or anything above.
func kernelRelPath(sk *kernelv1alpha1.SwiftKernel) (string, bool) {
	full := path.Clean(kernelv1alpha1.KernelLocalPath(sk.Namespace, sk.Name))
	rel, ok := strings.CutPrefix(full, kernelHostBasePath+"/")
	if !ok {
		return "", false
	}
	parts := strings.Split(rel, "/")
	if len(parts) != 2 {
		return "", false
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", false
		}
	}
	return rel, true
}

// pulledNodes lists the nodes this kernel may have files on: those in its
// status, and those its own pull Jobs ran on, which covers a node that has
// since lost the kernel-node label.
func (r *SwiftKernelReconciler) pulledNodes(ctx context.Context, sk *kernelv1alpha1.SwiftKernel) ([]string, error) {
	set := map[string]bool{}
	for _, ns := range sk.Status.NodeStatuses {
		if ns.NodeName != "" {
			set[ns.NodeName] = true
		}
	}
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(sk.Namespace)); err != nil {
		return nil, err
	}
	for i := range jobs.Items {
		if !metav1.IsControlledBy(&jobs.Items[i], sk) {
			continue
		}
		if n := jobs.Items[i].Spec.Template.Spec.NodeSelector[corev1.LabelHostname]; n != "" {
			set[n] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// cleanupRequeue is how often a deletion with cleanup pods in flight looks again.
const cleanupRequeue = 5 * time.Second

// handleDeletion removes the kernel's directory from every node it was pulled
// to, then drops NodeCleanupFinalizer. It returns when to look again: zero
// once the finalizer is gone.
//
// One cleanup pod per node, in the controller's namespace: a namespace being
// deleted admits no new pod, and its kernels are deleted with it. The pods are
// kept until every node has settled and then removed together; deleting each
// as it succeeded would have the next pass re-create it while another node was
// still running. A node that no longer exists has nothing to clean. A pod that
// fails, or cannot run within cleanupPendingLimit, gives its node up with a
// Warning event that names the directory left behind: kernel files are public
// artifacts, and a node that is down must not hold the deletion for good.
func (r *SwiftKernelReconciler) handleDeletion(ctx context.Context, sk *kernelv1alpha1.SwiftKernel) (time.Duration, error) {
	if !controllerutil.ContainsFinalizer(sk, NodeCleanupFinalizer) {
		return 0, nil
	}
	// Like a PVC in use: a running guest or sandbox launcher mounts the kernel
	// directory, and its hypervisor reads the kernel again when the guest
	// reboots. Its files stay until the last such pod is gone.
	if users, err := r.kernelUsers(ctx, sk); err != nil {
		return 0, err
	} else if len(users) > 0 {
		r.inUse(ctx, sk, users)
		return inUseRequeue, nil
	}
	rel, ok := kernelRelPath(sk)
	if !ok {
		r.warn(ctx, sk, fmt.Sprintf("kernel directory %q is not <namespace>/<name> under %s; left in place on every node",
			kernelv1alpha1.KernelLocalPath(sk.Namespace, sk.Name), kernelHostBasePath))
		return 0, r.finishDeletion(ctx, sk)
	}
	nodes, err := r.pulledNodes(ctx, sk)
	if err != nil {
		return 0, err
	}

	var skipped []string
	settled := true
	for _, node := range nodes {
		state, reason, err := r.cleanupNode(ctx, sk, node, rel)
		if err != nil {
			return 0, err
		}
		switch state {
		case nodeCleanupRunning:
			settled = false
		case nodeCleanupGaveUp:
			skipped = append(skipped, node+" ("+reason+")")
		}
	}
	if !settled {
		return cleanupRequeue, nil
	}
	if len(skipped) > 0 {
		r.warn(ctx, sk, fmt.Sprintf("could not remove %s from node(s) %s; remove it by hand",
			kernelv1alpha1.KernelLocalPath(sk.Namespace, sk.Name), strings.Join(skipped, ", ")))
	}
	return 0, r.finishDeletion(ctx, sk)
}

type nodeCleanupState int

const (
	nodeCleanupRunning nodeCleanupState = iota
	nodeCleanupDone
	nodeCleanupGaveUp
)

// cleanupNode drives the cleanup pod for one node and reports where it stands.
func (r *SwiftKernelReconciler) cleanupNode(ctx context.Context, sk *kernelv1alpha1.SwiftKernel, node, rel string) (nodeCleanupState, string, error) {
	var n corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		if apierrors.IsNotFound(err) {
			return nodeCleanupDone, "", nil // the node, and its files, are gone
		}
		return nodeCleanupRunning, "", err
	}
	key := client.ObjectKey{Namespace: r.cleanupPodNamespace(sk.Namespace), Name: cleanupPodName(sk, node)}
	var pod corev1.Pod
	err := r.Get(ctx, key, &pod)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, buildCleanupPod(sk, key, node, rel)); err != nil && !apierrors.IsAlreadyExists(err) {
			return nodeCleanupRunning, "", fmt.Errorf("create kernel cleanup pod for node %s: %w", node, err)
		}
		return nodeCleanupRunning, "", nil
	}
	if err != nil {
		return nodeCleanupRunning, "", err
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return nodeCleanupDone, "", nil
	case corev1.PodFailed:
		return nodeCleanupGaveUp, "the cleanup pod failed", nil
	case corev1.PodPending:
		if time.Since(pod.CreationTimestamp.Time) > cleanupPendingLimit {
			return nodeCleanupGaveUp, fmt.Sprintf("the cleanup pod did not run within %s", cleanupPendingLimit), nil
		}
	}
	return nodeCleanupRunning, "", nil
}

// finishDeletion deletes this kernel's cleanup pods and drops the finalizer.
// The pods go first: nothing owns them, so nothing else would delete them.
func (r *SwiftKernelReconciler) finishDeletion(ctx context.Context, sk *kernelv1alpha1.SwiftKernel) error {
	if err := r.deleteCleanupPods(ctx, sk.Namespace, func(uid string) bool { return uid == string(sk.UID) }); err != nil {
		return err
	}
	orig := sk.DeepCopy()
	controllerutil.RemoveFinalizer(sk, NodeCleanupFinalizer)
	return client.IgnoreNotFound(r.Patch(ctx, sk, client.MergeFrom(orig)))
}

// deleteOrphanCleanupPods deletes the cleanup pods of kernels in namespace that
// no longer exist, as when a finalizer is removed by hand mid-cleanup. Matched
// by UID, so a kernel recreated under the same name keeps its own pods.
func (r *SwiftKernelReconciler) deleteOrphanCleanupPods(ctx context.Context, namespace string) error {
	var kernels kernelv1alpha1.SwiftKernelList
	if err := r.List(ctx, &kernels, client.InNamespace(namespace)); err != nil {
		return err
	}
	live := make(map[string]bool, len(kernels.Items))
	for i := range kernels.Items {
		live[string(kernels.Items[i].UID)] = true
	}
	return r.deleteCleanupPods(ctx, namespace, func(uid string) bool { return !live[uid] })
}

func (r *SwiftKernelReconciler) deleteCleanupPods(ctx context.Context, kernelNamespace string, match func(uid string) bool) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.cleanupPodNamespace(kernelNamespace)), client.MatchingLabels{
		cleanupRoleLabel:     cleanupRoleValue,
		kernelNamespaceLabel: kernelNamespace,
	}); err != nil {
		return err
	}
	for i := range pods.Items {
		if !match(pods.Items[i].Labels[kernelUIDLabel]) {
			continue
		}
		if err := r.Delete(ctx, &pods.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// cleanupPodNamespace is where the cleanup pods of the kernels in namespace run:
// the controller's namespace, or the kernel's own when it is not known.
func (r *SwiftKernelReconciler) cleanupPodNamespace(namespace string) string {
	if r.ControllerNamespace != "" {
		return r.ControllerNamespace
	}
	return namespace
}

// cleanupPodName is stable per (kernel, node), so a re-run finds its pod; the
// hash keeps it unique in the controller's namespace, where kernels of every
// namespace meet.
func cleanupPodName(sk *kernelv1alpha1.SwiftKernel, node string) string {
	sum := sha256.Sum256([]byte(sk.Namespace + "/" + sk.Name + "/" + string(sk.UID) + "/" + node))
	return names.Bounded(cleanupPodPrefix+sk.Name, "-"+hex.EncodeToString(sum[:5]), maxCleanupPodName)
}

// buildCleanupPod is a one-shot pod on node that removes the kernel's
// directory. It mounts the kernels base and removes one <namespace>/<name>
// below it, never the base itself. No shell: the path is rm's argv operand.
func buildCleanupPod(sk *kernelv1alpha1.SwiftKernel, key client.ObjectKey, node, rel string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "kubeswift",
				"app.kubernetes.io/component": "swiftkernel-cleanup",
				cleanupRoleLabel:              cleanupRoleValue,
				kernelNameLabel:               names.LabelValue(sk.Name),
				kernelNamespaceLabel:          sk.Namespace,
				kernelUIDLabel:                string(sk.UID),
			},
		},
		Spec: corev1.PodSpec{
			NodeName:                     node,
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			// Bound to the node by name, so NoSchedule taints do not apply;
			// NoExecute ones would evict it, as on a GPU or control-plane node.
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:    "rm",
				Image:   cleanupImage,
				Command: []string{"rm", "-rf", "--"},
				Args:    []string{cleanupMount + "/" + rel},
				// Root, to remove what the pull Job wrote as root; nothing else.
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "kernels", MountPath: cleanupMount}},
			}},
			Volumes: []corev1.Volume{{
				Name: "kernels",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
					Path: kernelHostBasePath,
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				}},
			}},
		},
	}
}

func (r *SwiftKernelReconciler) warn(ctx context.Context, sk *kernelv1alpha1.SwiftKernel, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(sk, corev1.EventTypeWarning, ReasonNodeCleanupSkipped, msg)
	}
	log.FromContext(ctx).Info(msg, "swiftkernel", sk.Namespace+"/"+sk.Name)
}

// kernelUsers lists the pods in the kernel's namespace, not yet finished, that
// mount its directory: guest and sandbox launchers, and warm pool slots.
func (r *SwiftKernelReconciler) kernelUsers(ctx context.Context, sk *kernelv1alpha1.SwiftKernel) ([]string, error) {
	dir := path.Clean(kernelv1alpha1.KernelLocalPath(sk.Namespace, sk.Name))
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(sk.Namespace)); err != nil {
		return nil, err
	}
	var users []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.HostPath != nil && path.Clean(v.HostPath.Path) == dir {
				users = append(users, p.Name)
				break
			}
		}
	}
	sort.Strings(users)
	return users, nil
}

// inUse reports a deletion held by running pods; it looks again every
// inUseRequeue, and the recorder folds the repeated event.
func (r *SwiftKernelReconciler) inUse(ctx context.Context, sk *kernelv1alpha1.SwiftKernel, users []string) {
	shown := users
	if len(shown) > 3 {
		shown = append(shown[:3:3], fmt.Sprintf("and %d more", len(users)-3))
	}
	msg := fmt.Sprintf("deletion waits for %d running pod(s) using the kernel: %s", len(users), strings.Join(shown, ", "))
	if r.Recorder != nil {
		r.Recorder.Event(sk, corev1.EventTypeNormal, ReasonKernelInUse, msg)
	}
	log.FromContext(ctx).Info(msg, "swiftkernel", sk.Namespace+"/"+sk.Name)
}
