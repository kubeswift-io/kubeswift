package swiftguest

import (
	"encoding/json"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

const (
	ConditionResolved     = "Resolved"
	ConditionPodScheduled = "PodScheduled"
)

// MapPodToStatus updates status from pod phase and conditions.
func MapPodToStatus(pod *corev1.Pod, status *swiftv1alpha1.SwiftGuestStatus) {
	if pod == nil {
		return
	}

	// Set network from pod annotation (guest IP discovered by swiftletd)
	if ip, ok := pod.Annotations[PodAnnotationGuestIP]; ok && ip != "" {
		if status.Network == nil {
			status.Network = &swiftv1alpha1.GuestNetworkStatus{}
		}
		status.Network.PrimaryIP = ip
		status.Network.Interface = "eth0"
		status.Network.Ready = true
	}

	// Set network interfaces from pod annotation (set by swiftletd lease poller)
	if raw, ok := pod.Annotations[PodAnnotationGuestInterfaces]; ok && raw != "" {
		var ifaces []swiftv1alpha1.GuestNetworkInterface
		if err := json.Unmarshal([]byte(raw), &ifaces); err == nil {
			if status.Network == nil {
				status.Network = &swiftv1alpha1.GuestNetworkStatus{}
			}
			status.Network.Interfaces = ifaces
		}
	}

	// Egress reachability (service exposure §4): swiftletd reports whether the
	// pod netns can reach the cluster DNS ClusterIP. No silent failure — surface
	// it in status.network.egress + the EgressReady condition.
	if raw, ok := pod.Annotations[PodAnnotationEgress]; ok && raw != "" {
		if status.Network == nil {
			status.Network = &swiftv1alpha1.GuestNetworkStatus{}
		}
		reachable := raw == "true"
		if reachable {
			status.Network.Egress = "ClusterServices"
			setNetworkCondition(status, swiftv1alpha1.ConditionEgressReady, true,
				"ClusterServicesReachable", "cluster DNS ClusterIP reachable from the pod netns")
		} else {
			status.Network.Egress = "DirectOnly"
			setNetworkCondition(status, swiftv1alpha1.ConditionEgressReady, false,
				"ClusterIPUnreachableInPodNetns",
				"cluster DNS ClusterIP not reachable from the pod netns; on eBPF kube-proxy-free clusters the VM needs egressMode: clusterServices (roadmap)")
		}
	}

	// Set runtime from pod annotation (set by swiftletd on socket ready)
	if pidStr, ok := pod.Annotations[PodAnnotationGuestRuntimePID]; ok && pidStr != "" {
		if pid, err := strconv.ParseInt(pidStr, 10, 64); err == nil {
			hypervisor := "cloud-hypervisor"
			if h, ok := pod.Annotations[PodAnnotationGuestHypervisor]; ok && h != "" {
				hypervisor = h
			}
			status.Runtime = &swiftv1alpha1.GuestRuntimeStatus{
				PID:        pid,
				Hypervisor: hypervisor,
			}
		}
	}

	// Set console from pod annotation (set by swiftletd on socket ready)
	if socket, ok := pod.Annotations[PodAnnotationGuestSerialSocket]; ok && socket != "" {
		status.Console = &swiftv1alpha1.GuestConsoleStatus{
			SerialSocket: socket,
		}
	}

	// Set nodeName and podRef when scheduled
	if pod.Spec.NodeName != "" {
		status.NodeName = pod.Spec.NodeName
		status.PodRef = &corev1.ObjectReference{
			APIVersion: pod.APIVersion,
			Kind:       pod.Kind,
			Namespace:  pod.Namespace,
			Name:       pod.Name,
			UID:        pod.UID,
		}
	} else {
		status.PodRef = nil
	}

	switch pod.Status.Phase {
	case corev1.PodRunning:
		status.Phase = swiftv1alpha1.SwiftGuestPhaseRunning
		SetPodScheduledCondition(status, pod, true, "")
	case corev1.PodFailed:
		status.Phase = swiftv1alpha1.SwiftGuestPhaseFailed
		reason, msg := podFailureReason(pod)
		SetPodScheduledCondition(status, pod, false, reason+": "+msg)
	case corev1.PodSucceeded:
		status.Phase = swiftv1alpha1.SwiftGuestPhaseStopped
		SetPodScheduledCondition(status, pod, true, "")
	case corev1.PodPending:
		unschedulable := findUnschedulableCondition(pod)
		if unschedulable != nil {
			status.Phase = swiftv1alpha1.SwiftGuestPhasePending
			SetPodScheduledCondition(status, pod, false, unschedulable.Reason+": "+unschedulable.Message)
		} else {
			status.Phase = swiftv1alpha1.SwiftGuestPhaseScheduling
			SetPodScheduledCondition(status, pod, false, "Scheduling")
		}
	default:
		status.Phase = swiftv1alpha1.SwiftGuestPhaseScheduling
		SetPodScheduledCondition(status, pod, false, "Scheduling")
	}
}

func podFailureReason(pod *corev1.Pod) (string, string) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			reason := string(cs.State.Terminated.Reason)
			if reason == "" {
				reason = "Failed"
			}
			return reason, cs.State.Terminated.Message
		}
	}
	reason := pod.Status.Reason
	if reason == "" {
		reason = "Failed"
	}
	return reason, ""
}

func findUnschedulableCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodScheduled &&
			pod.Status.Conditions[i].Status == corev1.ConditionFalse &&
			pod.Status.Conditions[i].Reason == corev1.PodReasonUnschedulable {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// ConditionStorageReady captures whether the controller's per-driver
// pre-flight check for the resolved storage spec has succeeded. Today
// the only check is the Longhorn migratable-parameter check for
// RWX+Block guests on a Longhorn StorageClass; other CSI drivers
// pass through. The condition is informational — it does NOT gate pod
// creation, since the check is best-effort and storage classes can be
// fixed by the cluster admin without restarting the SwiftGuest.
const ConditionStorageReady = "StorageReady"

// EchoResolvedStorage writes the resolved storage spec onto the guest
// status as an informational mirror. liveMigrationCapable is intentionally
// not stored — it is recomputed from this echo at the SwiftMigration
// validation webhook (write-back-race avoidance; see
// api/swift/v1alpha1.ResolvedStorageStatus's doc comment).
func EchoResolvedStorage(status *swiftv1alpha1.SwiftGuestStatus, accessMode, volumeMode, storageClassName string) {
	status.Storage = &swiftv1alpha1.ResolvedStorageStatus{
		AccessMode:       corev1.PersistentVolumeAccessMode(accessMode),
		VolumeMode:       corev1.PersistentVolumeMode(volumeMode),
		StorageClassName: storageClassName,
	}
}

// SetStorageReadyCondition sets the StorageReady condition. When ok is
// false, reason+message names the per-driver pre-flight failure (today:
// Longhorn migratable parameter missing on a RWX+Block StorageClass).
func SetStorageReadyCondition(status *swiftv1alpha1.SwiftGuestStatus, ok bool, reason, message string) {
	cond := metav1.Condition{Type: ConditionStorageReady}
	if ok {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "StorageReady"
		cond.Message = message
		if cond.Message == "" {
			cond.Message = "Storage spec is ready for controller-created PVCs"
		}
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reason
		cond.Message = message
	}
	setCondition(status, cond)
}

// ConditionDataDisksReady is True once every secondary VM data disk
// (image-backed, blank, or attached) the guest declares is provisioned and
// its backing PVC is Bound. Unlike StorageReady, this DOES gate pod creation:
// a guest must not boot with a missing data disk (no silent failures), so the
// reconcile holds the guest in Scheduling until all data disks are ready.
const ConditionDataDisksReady = "DataDisksReady"

// SetDataDisksReadyCondition sets the DataDisksReady condition. When ok is
// false, reason+message names the not-yet-ready data disk (e.g. a blank PVC
// still binding, or a Filesystem PVC requested for an attachAsDisk disk).
func SetDataDisksReadyCondition(status *swiftv1alpha1.SwiftGuestStatus, ok bool, reason, message string) {
	cond := metav1.Condition{Type: ConditionDataDisksReady}
	if ok {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "DataDisksReady"
		cond.Message = message
		if cond.Message == "" {
			cond.Message = "All secondary data disks are ready"
		}
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reason
		cond.Message = message
	}
	setCondition(status, cond)
}

// SetResolvedCondition sets the Resolved condition.
func SetResolvedCondition(status *swiftv1alpha1.SwiftGuestStatus, ok bool, reason string) {
	cond := metav1.Condition{
		Type: ConditionResolved,
	}
	if ok {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Resolved"
		cond.Message = "Resolution succeeded"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "ResolutionFailed"
		cond.Message = reason
	}
	setCondition(status, cond)
}

// SetPodScheduledCondition sets the PodScheduled condition.
func SetPodScheduledCondition(status *swiftv1alpha1.SwiftGuestStatus, pod *corev1.Pod, scheduled bool, message string) {
	cond := metav1.Condition{
		Type: ConditionPodScheduled,
	}
	if scheduled {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "PodScheduled"
		cond.Message = "Pod is scheduled and running"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "PodNotScheduled"
		cond.Message = message
	}
	setCondition(status, cond)
}

func setCondition(status *swiftv1alpha1.SwiftGuestStatus, cond metav1.Condition) {
	cond.ObservedGeneration = 0 // Status has no generation; controller sets when updating
	now := metav1.Now()
	cond.LastTransitionTime = now

	found := false
	for i := range status.Conditions {
		if status.Conditions[i].Type == cond.Type {
			status.Conditions[i] = cond
			found = true
			break
		}
	}
	if !found {
		status.Conditions = append(status.Conditions, cond)
	}
}
