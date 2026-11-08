// Package hotplug is the contract between the SwiftVolumeAttachment
// controller and swiftletd for hot-plugged volumes (docs/storage/volume-hotplug.md).
//
// The controller writes the guest's desired set of hot-plugged volumes onto
// its launcher pod; swiftletd plugs into, and out of, Cloud Hypervisor until
// the VM matches it and reports each volume's state back on the pod. Both are
// declarative: a new launcher converges as soon as it reads the set, and
// "not in the set" is how a volume is detached.
//
// The launcher never takes a device number from the set. It reads the device
// kubelet published for the attachment pod named in the set, under
// <kubelet root>/plugins/kubernetes.io/csi/volumeDevices/<pv>/dev/<pod UID>,
// which only kubelet writes, and only for a pod that has that volume on this
// node.
package hotplug

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

const (
	// AnnotationVolumes is the desired set on the launcher pod: a JSON array
	// of [Volume], sorted by name. Written by the controller.
	AnnotationVolumes = "kubeswift.io/hotplug-volumes"
	// AnnotationForceDetach lists, comma-separated, the volumes the launcher
	// detaches without asking the guest agent whether they are in use.
	// Written by the controller.
	AnnotationForceDetach = "kubeswift.io/hotplug-force-detach"
	// AnnotationStatus is the launcher's report: a JSON array of [Status].
	// Written by swiftletd.
	AnnotationStatus = "kubeswift.io/hotplug-volumes-status"
	// AnnotationObserved is SetHash of the desired set the launcher's report
	// reflects. Written by swiftletd in the same patch as AnnotationStatus.
	// Until it matches the set on the pod, a volume missing from the report
	// may be one the launcher is plugging in right now.
	AnnotationObserved = "kubeswift.io/hotplug-volumes-observed"

	// LauncherFeature is advertised in kubeswift.io/launcher-features by a
	// launcher that converges on the desired set. The controller writes no
	// set to a launcher without it.
	LauncherFeature = "hotplug-volumes"

	// DefaultKubeletRootDir is kubelet's --root-dir default.
	DefaultKubeletRootDir = "/var/lib/kubelet"
	// VolumeDevicesSubdir is where kubelet publishes CSI block volumes, per
	// PV and consuming pod, under its root dir.
	VolumeDevicesSubdir = "plugins/kubernetes.io/csi/volumeDevices"
	// LauncherVolumeDevicesPath is where the launcher mounts that directory,
	// read-only.
	LauncherVolumeDevicesPath = "/var/run/kubeswift/volume-devices"
	// LauncherVolumeDevicesVolume names that mount's volume in the pod.
	LauncherVolumeDevicesVolume = "hotplug-volume-devices"
)

// Volume is one entry of the desired set.
type Volume struct {
	// Name is the SwiftVolumeAttachment's name.
	Name string `json:"name"`
	// Serial is the virtio serial the guest sees.
	Serial string `json:"serial"`
	// PVName is the PersistentVolume to plug in.
	PVName string `json:"pvName"`
	// AttachmentPodUID is the attachment pod, on the launcher's node, that
	// kubelet published the volume for.
	AttachmentPodUID types.UID `json:"attachmentPodUID"`
	// ReadOnly attaches the disk read-only.
	ReadOnly bool `json:"readOnly,omitempty"`
}

// Launcher states for a volume, in [Status].
const (
	StateAttaching = "Attaching"
	StateAttached  = "Attached"
	StateDetaching = "Detaching"
	StateInUse     = "InUse"
	StateFailed    = "Failed"
)

// Status is one volume as the launcher reports it. A volume the launcher has
// fully detached is not listed.
type Status struct {
	Name   string `json:"name"`
	Serial string `json:"serial,omitempty"`
	State  string `json:"state"`
	// Reason is a short code for Failed (NoPCISlot, DeviceRefused, ...) or
	// InUse; Message has the detail.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Attempt is the Cloud Hypervisor device id suffix in use.
	Attempt int `json:"attempt,omitempty"`
}

// EncodeVolumes renders the desired set, sorted by name, so an unchanged set
// is an unchanged annotation.
func EncodeVolumes(vols []Volume) (string, error) {
	sorted := append([]Volume(nil), vols...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	if sorted == nil {
		sorted = []Volume{}
	}
	b, err := json.Marshal(sorted)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeVolumes parses a desired set. An empty annotation is an empty set.
func DecodeVolumes(s string) ([]Volume, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var vols []Volume
	if err := json.Unmarshal([]byte(s), &vols); err != nil {
		return nil, fmt.Errorf("parse %s: %w", AnnotationVolumes, err)
	}
	return vols, nil
}

// DecodeStatus parses the launcher's report, keyed by volume name.
func DecodeStatus(s string) (map[string]Status, error) {
	out := map[string]Status{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	var sts []Status
	if err := json.Unmarshal([]byte(s), &sts); err != nil {
		return nil, fmt.Errorf("parse %s: %w", AnnotationStatus, err)
	}
	for _, st := range sts {
		out[st.Name] = st
	}
	return out, nil
}

// EncodeForceDetach renders the force-detach list, sorted.
func EncodeForceDetach(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// HostVolumeDevicesDir is the host directory the launcher mounts.
func HostVolumeDevicesDir(kubeletRootDir string) string {
	if kubeletRootDir == "" {
		kubeletRootDir = DefaultKubeletRootDir
	}
	return path.Join(kubeletRootDir, VolumeDevicesSubdir)
}

// HasLauncherFeature reports whether a launcher-features annotation value
// lists the hotplug feature.
func HasLauncherFeature(features string) bool {
	for _, f := range strings.Fields(features) {
		if f == LauncherFeature {
			return true
		}
	}
	return false
}

// SetHash identifies a desired-set annotation value: the first 16 hex digits
// of its SHA-256. swiftletd computes the same over the exact string it read.
func SetHash(set string) string {
	sum := sha256.Sum256([]byte(set))
	return hex.EncodeToString(sum[:8])
}

// Settled reports whether the launcher's report reflects the desired set now
// on its pod. A pod with neither a set nor a report (a new launcher) is
// settled: it has plugged in nothing.
func Settled(annotations map[string]string) bool {
	set, ok := annotations[AnnotationVolumes]
	if !ok && annotations[AnnotationStatus] == "" {
		return true
	}
	return annotations[AnnotationObserved] == SetHash(set)
}
