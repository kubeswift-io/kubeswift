package swiftguest

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

const (
	ConditionResolved     = "Resolved"
	ConditionPodScheduled = "PodScheduled"
)

// MapNetworkReadyCondition surfaces the guest's IP-acquisition outcome on the CR
// (#527). Call AFTER MapPodToStatus, which is what populates status.Network.
//
// The bug this closes: a guest whose NIC never came up sat at Running with
// GuestRunning=True and an empty IP indefinitely. swiftletd's lease poller gave
// up after ~4 minutes into a `log::warn!` and told nobody, so nothing on the CR
// distinguished "never going to get an IP" from "still booting". Diagnosing it
// meant reading launcher logs and attaching to a serial console.
//
// Takes the guest, not just the pod, because the most common cause is knowable
// from the spec: a disk-boot guest with no seedProfileRef gets no NoCloud seed,
// so cloud-init finds no datasource, never writes netplan, and the interface is
// never configured — no DHCP request is ever sent. Naming that in the message is
// the difference between a dead end and a fix.
//
// Deliberately NOT a webhook rejection of "disk-boot without a seed": an image
// that self-configures (baked-in static addressing, a different init) is a valid
// configuration. Make the failure visible; do not forbid the shape.
func MapNetworkReadyCondition(guest *swiftv1alpha1.SwiftGuest, pod *corev1.Pod, status *swiftv1alpha1.SwiftGuestStatus) {
	if pod == nil || guest == nil {
		return
	}
	hasIP := status.Network != nil && status.Network.PrimaryIP != ""
	if hasIP {
		// Covers a lease that landed after the poller had already reported a
		// timeout (a late DHCP, or an operator fixing the guest in place): the
		// condition must recover, not latch False forever.
		setNetworkCondition(status, swiftv1alpha1.ConditionNetworkReady, true,
			"IPAcquired", "guest acquired "+status.Network.PrimaryIP)
		return
	}
	raw, reported := pod.Annotations[PodAnnotationNetworkUnready]
	if !reported {
		// No IP and no timeout report yet — still within the poll window. Not a
		// failure, and asserting one here would make every booting guest look
		// broken for its first minute.
		return
	}
	setNetworkCondition(status, swiftv1alpha1.ConditionNetworkReady, false,
		"DHCPTimeout", dhcpTimeoutMessage(guest, raw))
}

// dhcpTimeoutMessage composes the operator-facing explanation.
func dhcpTimeoutMessage(guest *swiftv1alpha1.SwiftGuest, raw string) string {
	msg := "no DHCP lease"
	var report struct {
		AfterSeconds int64 `json:"afterSeconds"`
	}
	if err := json.Unmarshal([]byte(raw), &report); err == nil && report.AfterSeconds > 0 {
		msg += " after " + strconv.FormatInt(report.AfterSeconds, 10) + "s"
	}
	msg += "; the guest booted but never requested one"
	if guest.Spec.ImageRef != nil && guest.Spec.SeedProfileRef == nil {
		msg += ". This guest is disk-boot with no seedProfileRef, so it gets no " +
			"NoCloud seed: cloud-init finds no datasource, never writes netplan, " +
			"and the interface is never configured. Set spec.seedProfileRef, or " +
			"use an image that configures its own networking"
	}
	return msg
}

// ClearRunState drops what described a launcher that is gone.
//
// Run-scoped means "true of one launcher pod, and only while it runs":
// GuestRunning and PortsProgrammed are written by swiftletd from inside it,
// NetworkReady and EgressReady are observations of it, PodScheduled describes
// it, primaryIP (and its scope) is the lease the VM held, and podIP is the
// launcher's own address. None of them outlive it.
//
// Nothing used to clear them, so a stopped guest reported Running with an
// address, and a restarting one reported the previous run's until its new
// launcher overwrote them — the stale values the migration controller had to
// work around, because they would otherwise drive a false "Completed"
// (W-GPU-3, resuming.go). Only conditions the guest actually has are cleared:
// a guest that never ran gains nothing by being told it is not running.
func ClearRunState(status *swiftv1alpha1.SwiftGuestStatus, reason, message string) {
	for _, t := range []string{
		"GuestRunning",
		ConditionPodScheduled,
		swiftv1alpha1.ConditionNetworkReady,
		swiftv1alpha1.ConditionEgressReady,
		swiftv1alpha1.ConditionPortsProgrammed,
	} {
		c := findCondition(status, t)
		if c == nil {
			continue
		}
		// Already said, with the same reason: leave it alone. setCondition
		// stamps lastTransitionTime unconditionally, so re-clearing on every
		// pass would rewrite the status — and claim a transition — for a guest
		// that has not moved. A pod can sit Pending for a long time.
		if c.Status == metav1.ConditionFalse && c.Reason == reason {
			continue
		}
		setCondition(status, metav1.Condition{
			Type: t, Status: metav1.ConditionFalse, Reason: reason, Message: message,
		})
	}
	if status.Network != nil {
		status.Network.PrimaryIP = ""
		status.Network.PrimaryIPScope = ""
		status.Network.PodIP = ""
		// Per-interface addresses are leases of the same run, and readiness
		// and egress reachability were observations of it.
		status.Network.Interfaces = nil
		status.Network.Ready = false
		status.Network.Egress = ""
	}
	// The hypervisor process and its serial socket went with the launcher.
	// The hypervisor kind is not run-scoped: it says what the guest runs
	// under, and snapshots record it from here.
	if status.Runtime != nil {
		status.Runtime.PID = 0
	}
	status.Console = nil
}

// launcherHandedOff reports whether pod's launcher sent its VM away in a live
// migration. Its Cloud Hypervisor exited because the VM now runs in the
// migration's destination pod, and the plaintext-transport launcher exits 0
// right after -- a Succeeded pod that is not a guest shutdown. Until cutover
// makes the destination the guest's pod, nothing about this pod describes the
// guest.
func launcherHandedOff(pod *corev1.Pod) bool {
	return pod != nil && pod.Annotations[PodAnnotationMigrationStatus] == "complete"
}

// MapPodToStatus updates status from pod phase and conditions. guest is the
// SwiftGuest the pod runs: its spec says which network the primary interface
// is on, which the pod alone does not show.
func MapPodToStatus(guest *swiftv1alpha1.SwiftGuest, pod *corev1.Pod, status *swiftv1alpha1.SwiftGuestStatus) {
	if pod == nil {
		return
	}
	// Mapping a handed-off launcher's exit would report the guest stopped and
	// clear the GuestRunning=True the destination already wrote (swiftletd
	// writes it once), leaving the migration waiting in Resuming until
	// spec.timeout.
	if launcherHandedOff(pod) {
		return
	}

	// A different launcher than the one this status describes: nothing the last
	// one reported is true of this one. Before PodRef is overwritten below, and
	// before anything this pod reports is read, so a pod that has already said
	// something wins.
	if status.PodRef != nil && pod.UID != "" && status.PodRef.UID != pod.UID {
		ClearRunState(status, "GuestStarting", "a new launcher is starting; nothing from the previous run applies")
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

	// Model A (primary OVN-K UDN): swiftletd cannot reach the apiserver from a
	// primary-UDN pod (the UDN is bridged to the guest; eth0 is infrastructure-locked),
	// so it never writes the guest-ip annotation. The guest's IP IS the pod's
	// OVN-assigned UDN IP — derive it from the OVN pod-networks annotation when swiftletd
	// has not (and cannot) provide one.
	if udn := pod.Annotations[PodAnnotationPrimaryUDNIface]; udn != "" {
		if status.Network == nil || status.Network.PrimaryIP == "" {
			if ip := primaryUDNIPFromPod(pod); ip != "" {
				if status.Network == nil {
					status.Network = &swiftv1alpha1.GuestNetworkStatus{}
				}
				status.Network.PrimaryIP = ip
				status.Network.Interface = udn
				status.Network.Ready = true
			}
		}
	}

	// Say where primaryIP can be reached from: a nat guest's is private to its
	// launcher pod and repeats across guests. Derived on every pass rather
	// than only when an address is mapped, so a status written before the
	// field existed gains it too.
	if status.Network != nil {
		status.Network.PrimaryIPScope = ""
		if status.Network.PrimaryIP != "" {
			status.Network.PrimaryIPScope = primaryIPScope(guest, pod)
		}
	}

	// The launcher's own address: where a nat guest's declared ports are
	// reachable, and unique in the cluster where a Pod-scope primaryIP is not.
	// A new launcher's run state was cleared above, so the previous launcher's
	// IP does not outlive it. A live migration's cutover moves podRef, UID
	// included, to the destination pod, which is then the pod mapped here.
	if pod.Status.PodIP != "" {
		if status.Network == nil {
			status.Network = &swiftv1alpha1.GuestNetworkStatus{}
		}
		status.Network.PodIP = pod.Status.PodIP
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
		// The launcher is gone, so the VM is too: say so rather than leave the
		// last "running with an address" standing on a failed guest.
		ClearRunState(status, "LauncherExited", "the launcher exited; the VM is not running")
		SetPodScheduledCondition(status, pod, false, reason+": "+msg)
	case corev1.PodSucceeded:
		status.Phase = swiftv1alpha1.SwiftGuestPhaseStopped
		ClearRunState(status, "LauncherExited", "the launcher exited; the VM is not running")
		SetPodScheduledCondition(status, pod, true, "")
	case corev1.PodPending:
		// A Pending pod whose launcher has not started has no VM behind it —
		// whatever the last launcher reported. This catches the run state a
		// launcher change alone does not: an upgrade can arrive with podRef
		// already naming the current pod, leaving a guest stuck Pending on an
		// unattachable volume reporting GuestRunning=True with an address.
		//
		// Pending does NOT mean nothing started, though: the pod stays Pending
		// while ANY container is still waiting, so a running launcher next to a
		// sidecar that is still pulling or starting (the migration stunnel
		// server) reads Pending too. swiftletd reports GuestRunning=True once,
		// so clearing it then left the guest reading not-running for good.
		if !launcherContainerRunning(pod) {
			ClearRunState(status, "GuestStarting", "the launcher has not started; the VM is not running")
		}
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

	// GuestRunning for a Model A guest: swiftletd can't patch it (no apiserver), so the
	// controller derives it from launcher readiness (the CH-socket readiness probe the
	// pod-builder adds). For non-Model-A guests this is untouched — swiftletd owns it.
	if udn := pod.Annotations[PodAnnotationPrimaryUDNIface]; udn != "" {
		if isLauncherReady(pod) {
			setCondition(status, metav1.Condition{
				Type:    "GuestRunning",
				Status:  metav1.ConditionTrue,
				Reason:  "GuestRunning",
				Message: "Cloud Hypervisor running (derived from launcher readiness; primary-UDN guest)",
			})
		} else {
			setCondition(status, metav1.Condition{
				Type:    "GuestRunning",
				Status:  metav1.ConditionFalse,
				Reason:  "GuestStarting",
				Message: "waiting for Cloud Hypervisor (launcher not ready; primary-UDN guest)",
			})
		}
	}
}

// primaryIPScope says where the guest's primaryIP can be reached from. It is
// on a network outside the launcher pod when the primary interface rides a
// multi-node NAD (the address comes from the NAD's IPAM) or the namespace's
// primary OVN-Kubernetes UDN (the pod's UDN address, handed to the guest).
// Otherwise the guest sits behind the launcher's nat, on the in-pod bridge,
// whose subnet and DHCP range are the same in every launcher.
// spec.network.binding is not read: the datapath follows the primary
// interface (network-init.sh), so a bridge binding without a NAD primary
// still leaves the guest on the in-pod bridge.
func primaryIPScope(guest *swiftv1alpha1.SwiftGuest, pod *corev1.Pod) swiftv1alpha1.PrimaryIPScope {
	if pod.Annotations[PodAnnotationPrimaryUDNIface] != "" {
		return swiftv1alpha1.PrimaryIPScopeNetwork
	}
	if guest != nil && guest.PrimaryIPPreservedCrossNode() {
		return swiftv1alpha1.PrimaryIPScopeNetwork
	}
	return swiftv1alpha1.PrimaryIPScopePod
}

// launcherContainerRunning reports whether the pod's launcher (swiftletd)
// container is currently running.
func launcherContainerRunning(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == LauncherContainerName {
			return cs.State.Running != nil
		}
	}
	return false
}

// primaryUDNIPFromPod extracts the guest's UDN IP from the pod's OVN-Kubernetes
// pod-networks annotation: the entry that is NOT the cluster default network ("default",
// role infrastructure-locked on a primary-UDN pod). Returns "" when absent/unparseable.
func primaryUDNIPFromPod(pod *corev1.Pod) string {
	raw := pod.Annotations[OVNPodNetworksAnnotation]
	if raw == "" {
		return ""
	}
	var nets map[string]struct {
		IPAddresses []string `json:"ip_addresses"`
		Role        string   `json:"role"`
	}
	if err := json.Unmarshal([]byte(raw), &nets); err != nil {
		return ""
	}
	for name, n := range nets {
		if name == "default" || n.Role == "infrastructure-locked" {
			continue
		}
		if len(n.IPAddresses) > 0 {
			ip := n.IPAddresses[0]
			if i := strings.IndexByte(ip, '/'); i >= 0 {
				ip = ip[:i]
			}
			return ip
		}
	}
	return ""
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

// Root-disk clone reasons on StorageReady.
const (
	reasonRootDiskCloning     = "RootDiskCloning"
	reasonRootDiskCloneFailed = "RootDiskCloneFailed"
)

// setRootDiskCloneCondition reports on StorageReady why the root disk is not
// ready yet: RootDiskCloneFailed for a failure retrying will not fix,
// RootDiskCloning otherwise. A storage pre-flight failure already on the
// condition is left in place: it is the more basic problem.
//
// The pre-flight sets StorageReady=True earlier in the same pass, so this
// flips it back every time; keeping the stored transition time for an
// unchanged reason stops that from rewriting the status on every requeue.
func setRootDiskCloneCondition(status, stored *swiftv1alpha1.SwiftGuestStatus, err error) {
	if c := findCondition(status, ConditionStorageReady); c != nil && c.Status == metav1.ConditionFalse &&
		c.Reason != reasonRootDiskCloning && c.Reason != reasonRootDiskCloneFailed {
		return
	}
	reason := reasonRootDiskCloning
	var failure *rootDiskFailure
	if errors.As(err, &failure) {
		reason = reasonRootDiskCloneFailed
	}
	SetStorageReadyCondition(status, false, reason, err.Error())
	if prev := findCondition(stored, ConditionStorageReady); prev != nil &&
		prev.Status == metav1.ConditionFalse && prev.Reason == reason {
		findCondition(status, ConditionStorageReady).LastTransitionTime = prev.LastTransitionTime
	}
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

// ConditionCloneIdentityRegenerated is set on a cloneFromSnapshot guest whose
// source opted into the in-guest identity agent: True once the agent has
// regenerated the clone's identity in place (machine-id / SSH keys / hostname /
// MAC + re-DHCP), False with reason GuestAgentUnreachable when the agent does
// not answer (the clone still runs as a warm replica sharing the source's
// identity — a loud, never-silent fallback). Absent when the guest is not an
// agent-enabled clone.
const ConditionCloneIdentityRegenerated = "CloneIdentityRegenerated"

// SetCloneIdentityRegeneratedCondition sets CloneIdentityRegenerated.
func SetCloneIdentityRegeneratedCondition(status *swiftv1alpha1.SwiftGuestStatus, ok bool, reason, message string) {
	cond := metav1.Condition{Type: ConditionCloneIdentityRegenerated}
	if ok {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Regenerated"
		cond.Message = message
		if cond.Message == "" {
			cond.Message = "Clone identity regenerated in place by the in-guest agent"
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

// setCondition sets or updates the condition of cond.Type.
//
// lastTransitionTime moves only when the condition's status does, as the
// field means (apimeta.SetStatusCondition's rule). It used to be restamped on
// every call, so every reconcile of every guest changed its status and wrote
// it -- the unchanged-status check before the write could never hold -- and
// the timestamps said nothing about when anything happened.
func setCondition(status *swiftv1alpha1.SwiftGuestStatus, cond metav1.Condition) {
	cond.ObservedGeneration = 0 // Status has no generation; controller sets when updating
	for i := range status.Conditions {
		existing := &status.Conditions[i]
		if existing.Type != cond.Type {
			continue
		}
		cond.LastTransitionTime = existing.LastTransitionTime
		if existing.Status != cond.Status || cond.LastTransitionTime.IsZero() {
			cond.LastTransitionTime = metav1.Now()
		}
		*existing = cond
		return
	}
	cond.LastTransitionTime = metav1.Now()
	status.Conditions = append(status.Conditions, cond)
}
