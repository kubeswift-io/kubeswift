// Package clonecommon holds the backend-mechanism primitives shared by the
// snapshot clone/restore paths — the s3 download Job, the node-local cache
// layout, the per-clone MAC computation, and the runtime-dir prefix. It is
// imported by both the swiftrestore controller (SwiftRestore phase machine) and
// the swiftguest controller (SwiftGuest.spec.cloneFromSnapshot boot path,
// Snapshot Phase 4). It deliberately imports NEITHER controller package so both
// can depend on it without an import cycle; the restore/clone annotation maps
// (which reference swiftguest's annotation keys) stay in their owning
// controllers and call these primitives.
package clonecommon

import (
	"path"
	"path/filepath"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// HostPathBase is the kubeswift-managed subtree the node-local snapshot cache
// lives under (capture, upload-read, and download-write all use it). Mirrors
// swiftsnapshot.HostPathBaseDir; kept here so the shared primitives don't import
// a controller package.
const HostPathBase = "/var/lib/kubeswift/snapshots/"

// S3LocalDir is the node-local cache directory for a snapshot's s3 artifacts —
// where the upload Job reads from on the capture node and the download Job
// writes to on the restore/clone node. Derived deterministically from the
// snapshot's identity so it is stable across reconciles and consistent across
// nodes: <HostPathBase>/<namespace>-<name>.
func S3LocalDir(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return filepath.Join(HostPathBase, snap.Namespace+"-"+snap.Name)
}

// S3KeyPrefix is the object-key prefix a snapshot's artifacts live under,
// derived identically on the upload and download sides:
// <prefix>/<namespace>/<name>.
func S3KeyPrefix(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return path.Join(snap.Spec.Backend.S3.Prefix, snap.Namespace, snap.Name)
}

// RuntimeDirPrefix returns the per-pod runtime_dir prefix the snapshot-stager
// substitutes in disks[].path and serial.socket. Mirrors swift-runtime's
// create_runtime_dir naming (rust/swift-runtime/src/runtime_dir.rs): base is
// /var/lib/kubeswift/run, the per-guest directory is "<ns>-<name>" (slashes in
// guest_id become hyphens), trailing "/" required so the patcher's prefix match
// does not clip a longer name that starts with a shorter one.
func RuntimeDirPrefix(namespace, name string) string {
	return "/var/lib/kubeswift/run/" + namespace + "-" + name + "/"
}
