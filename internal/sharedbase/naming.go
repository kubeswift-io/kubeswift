package sharedbase

import "k8s.io/apimachinery/pkg/types"

// Names and keys shared by the controller, the resolver and the node command.
// They live in one place because a guest's disk is found by these values alone:
// if two components disagreed about a device name, the launcher would look for
// a disk the materialise Job never made.

const (
	// Pool is the device-mapper name of a node's thin pool.
	Pool = "kubeswift-pool"
	// StateRoot is where a node keeps its pool's backing files, registry and
	// locks. It must survive the pod that writes it — the registry is what
	// maps a guest to its disk across launcher restarts and node reboots.
	StateRoot = "/var/lib/kubeswift"
	// DeviceDir is where device-mapper nodes appear.
	DeviceDir = "/dev/mapper"
)

// DeviceName is the device-mapper name of a guest's root disk.
//
// Keyed by UID, not name. A guest deleted and recreated under the same name is a
// different guest, and must never be handed its predecessor's disk — keying by
// name would do exactly that. A UID is 36 characters of [0-9a-f-], which
// device-mapper accepts, so the name stays readable.
func DeviceName(uid types.UID) string { return "ks-g-" + string(uid) }

// DevicePath is where the guest's disk is mapped on its node.
func DevicePath(uid types.UID) string { return DeviceDir + "/" + DeviceName(uid) }

// GuestKey identifies a guest in the node's registry. It carries the UID for the
// same reason DeviceName does.
func GuestKey(namespace, name string, uid types.UID) string {
	return namespace + "/" + name + "/" + string(uid)
}

// BaseKey is the content identity of an image's prepared artifact.
//
// The prepared PVC's UID changes whenever the image is re-imported. The
// SwiftImage's UID is added in front for the one case the PVC UID misses:
// delete a SwiftImage while guests still run from it, and pvc-protection keeps
// its PVC Terminating; recreate the image under the same name, and the import
// controller treats AlreadyExists as success and imports the NEW image into the
// OLD PVC, under its old UID. A node already holding a base for that UID would
// then give new guests the old image. A recreated SwiftImage always has a new
// UID, so this key moves with it.
func BaseKey(imageUID, pvcUID types.UID) string { return string(imageUID) + "/" + string(pvcUID) }
