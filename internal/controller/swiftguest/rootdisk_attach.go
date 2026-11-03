package swiftguest

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// Root-disk reasons on StorageReady while the launcher waits for the disk on
// its node.
const (
	reasonRootDiskAttaching = "RootDiskAttaching"
	reasonRootDiskMounting  = "RootDiskMounting"
)

// rootDiskAttachRetry refreshes the condition while the launcher waits for its
// root disk. A VolumeAttachment changing does not wake this controller, and a
// CSI snapshot clone can wait minutes.
const rootDiskAttachRetry = 5 * time.Second

// reportRootDiskWait sets StorageReady=False while a scheduled launcher pod
// waits for its root disk PVC to be attached to and mounted on its node (#909).
//
// The root disk clone step is done once its PVC is Bound at the right size, but
// the storage may not let the volume attach yet: a Longhorn snapshot clone is
// copied after the PVC binds, and its attach is refused until the copy is done.
// The guest sat in Scheduling with StorageReady=True for minutes. The kubelet
// creates the pod sandbox only once every volume is mounted, so
// PodReadyToStartContainers (or any container having started) marks the end of
// the wait. Unscheduled pods are left alone: that wait is the scheduler's.
//
// Returns how soon to look again; 0 when the launcher is not waiting.
func (r *SwiftGuestReconciler) reportRootDiskWait(ctx context.Context, guest *swiftv1alpha1.SwiftGuest,
	pod *corev1.Pod, status *swiftv1alpha1.SwiftGuestStatus) (time.Duration, error) {
	if pod.Status.Phase != corev1.PodPending || pod.Spec.NodeName == "" || podVolumesMounted(pod) {
		return 0, nil
	}
	if c := findCondition(status, ConditionStorageReady); c != nil && c.Status == metav1.ConditionFalse &&
		c.Reason != reasonRootDiskAttaching && c.Reason != reasonRootDiskMounting {
		return 0, nil // a pre-flight or clone failure is the more basic problem
	}
	claim := rootDiskClaim(pod)
	if claim == "" {
		return 0, nil // shared-base or kernel boot: no root disk PVC
	}
	node := pod.Spec.NodeName
	reason := reasonRootDiskMounting
	msg := fmt.Sprintf("waiting for the kubelet on node %s to mount root disk PVC %s", node, claim)

	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: claim}, &pvc); err != nil {
		if !apierrors.IsNotFound(err) {
			return 0, err
		}
	} else if pv := pvc.Spec.VolumeName; pv != "" {
		va, err := r.volumeAttachment(ctx, pv, node)
		if err != nil {
			return 0, err
		}
		if va == nil || !va.Status.Attached {
			reason = reasonRootDiskAttaching
			msg = fmt.Sprintf("waiting for the storage to attach root disk PVC %s to node %s "+
				"(a CSI snapshot clone attaches only once the storage has copied it)", claim, node)
			if va != nil && va.Status.AttachError != nil && va.Status.AttachError.Message != "" {
				msg += ": " + va.Status.AttachError.Message
			}
		}
	}

	prev := findCondition(&guest.Status, ConditionStorageReady)
	SetStorageReadyCondition(status, false, reason, msg)
	if prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == reason {
		findCondition(status, ConditionStorageReady).LastTransitionTime = prev.LastTransitionTime
	}
	return rootDiskAttachRetry, nil
}

// podVolumesMounted reports whether the kubelet has mounted the pod's volumes:
// it creates the sandbox only after that, and starts containers after that.
func podVolumesMounted(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReadyToStartContainers && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range list {
			if cs.State.Running != nil || cs.State.Terminated != nil {
				return true
			}
		}
	}
	return false
}

// rootDiskClaim returns the PVC behind the launcher's root-disk volume.
func rootDiskClaim(pod *corev1.Pod) string {
	for _, v := range pod.Spec.Volumes {
		if v.Name == "root-disk" && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

// volumeAttachment returns the VolumeAttachment of pv to node, or nil.
func (r *SwiftGuestReconciler) volumeAttachment(ctx context.Context, pv, node string) (*storagev1.VolumeAttachment, error) {
	var vas storagev1.VolumeAttachmentList
	if err := r.List(ctx, &vas); err != nil {
		return nil, fmt.Errorf("list VolumeAttachments: %w", err)
	}
	for i := range vas.Items {
		va := &vas.Items[i]
		if va.Spec.NodeName == node && va.Spec.Source.PersistentVolumeName != nil && *va.Spec.Source.PersistentVolumeName == pv {
			return va, nil
		}
	}
	return nil, nil
}
