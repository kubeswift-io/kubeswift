package swiftvolumeattachment

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

const (
	// labelAttachmentGuest names the guest on an attachment pod, so a pod
	// event finds the guest to reconcile. Not kubeswift.io/guest: selectors
	// for a guest's launcher use that.
	labelAttachmentGuest = "kubeswift.io/volume-attachment-guest"

	// attachmentDevicePath is where kubelet puts the volume in the
	// attachment pod. Nothing reads it there.
	attachmentDevicePath = "/dev/kubeswift-vol"

	// attachmentUID is the nonroot uid of the attachment container.
	attachmentUID int64 = 65532
)

// buildAttachmentPod is the pod that keeps the attachment's volume on the
// guest's node: kubelet attaches the claim through its CSI driver for the
// pod and publishes the device for it, which is where the launcher reads it.
// It runs the launcher's own image, already on the node, and only sleeps.
// Unprivileged: kubelet makes the device node, not the pod.
//
// restartPolicy Always: the volume stays published across a restart of the
// container. With Never, a container that died would end the pod and
// kubelet would take the volume away from a running VM.
func buildAttachmentPod(sva *swiftv1alpha1.SwiftVolumeAttachment, launcher *corev1.Pod, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: sva.Name + "-att-",
			Namespace:    sva.Namespace,
			Labels: map[string]string{
				swiftv1alpha1.LabelVolumeAttachment: sva.Name,
				labelAttachmentGuest:                sva.Spec.GuestRef.Name,
				"app.kubernetes.io/managed-by":      "kubeswift",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         swiftv1alpha1.GroupName + "/" + swiftv1alpha1.Version,
				Kind:               "SwiftVolumeAttachment",
				Name:               sva.Name,
				UID:                sva.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			NodeName:                     launcher.Spec.NodeName,
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: ptr.To(false),
			EnableServiceLinks:           ptr.To(false),
			PriorityClassName:            launcher.Spec.PriorityClassName,
			Tolerations:                  launcher.Spec.Tolerations,
			ImagePullSecrets:             launcher.Spec.ImagePullSecrets,
			// Nothing to drain: kill it at once on delete.
			TerminationGracePeriodSeconds: ptr.To[int64](0),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr.To(true),
				RunAsUser:      ptr.To(attachmentUID),
				RunAsGroup:     ptr.To(attachmentUID),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "hold",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", "infinity"},
				VolumeDevices:   []corev1.VolumeDevice{{Name: "vol", DevicePath: attachmentDevicePath}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false),
					ReadOnlyRootFilesystem:   ptr.To(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1m"),
						corev1.ResourceMemory: resource.MustParse("8Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "vol",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						// Not ReadOnly here even for a read-only attachment:
						// Cloud Hypervisor opens the disk read-only, and some
						// CSI drivers refuse a read-only block publish.
						ClaimName: sva.Spec.PVCRef.Name,
					},
				},
			}},
		},
	}
}

// launcherImage is the image of the launcher pod's launcher container.
func launcherImage(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		if c.Name == "launcher" {
			return c.Image
		}
	}
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Image
	}
	return ""
}
