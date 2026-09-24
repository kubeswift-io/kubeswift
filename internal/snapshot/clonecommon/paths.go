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
	"strings"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
)

// HostPathBase is the kubeswift-managed subtree the node-local snapshot cache
// lives under (capture, upload-read, and download-write all use it). Mirrors
// swiftsnapshot.HostPathBaseDir; kept here so the shared primitives don't import
// a controller package.
const HostPathBase = "/var/lib/kubeswift/snapshots/"

// maxDirName is the longest file name Linux accepts (NAME_MAX).
const maxDirName = 255

// SnapshotDir is the node directory a snapshot captured from now on uses, on
// every backend: the capture, the s3/oci upload source, and the download cache
// on a restore or clone node. <HostPathBase><namespace>_<name>.
//
// It is derived from the snapshot's identity, never chosen by its author, and
// bound to its namespace. The old names were not: the local backend used a
// hostPath the author picked, so a tenant could name another tenant's
// directory and have it emptied by a capture or removed by a delete, and the
// s3/oci cache used "<namespace>-<name>", which is ambiguous because both parts
// may contain '-' (namespace "team" + "a-db" and "team-a" + "db" collide).
// '_' occurs in neither a namespace (an RFC 1123 label) nor an object name (an
// RFC 1123 subdomain), so here the namespace is everything before the first
// '_', and two snapshots never share a directory.
//
// One segment, not <namespace>/<name>: swiftletd since v0.14.1 accepts a
// capture destination only one segment below the root, and a running
// launcher keeps the swiftletd it started with, so a nested path would fail
// every capture of a guest started before the upgrade. The name is bounded to
// NAME_MAX; a shortened one keeps its "<namespace>_" prefix.
func SnapshotDir(namespace, name string) string {
	return HostPathBase + names.Bounded(namespace+"_"+name, "", maxDirName)
}

// SnapshotDirNamespaced reports whether dir is a SnapshotDir of namespace:
// directly under HostPathBase, named "<namespace>_...".
func SnapshotDirNamespaced(dir, namespace string) bool {
	seg, ok := strings.CutPrefix(strings.TrimSuffix(dir, "/"), HostPathBase)
	return ok && namespace != "" && strings.HasPrefix(seg, namespace+"_") && !strings.Contains(seg, "/")
}

// NodeDir is where a snapshot's artifacts live on a node: the capture node's
// copy, and the download cache on a restore or clone node.
//
// It is the directory recorded when the capture began
// (status.memorySnapshot.handle), so a snapshot keeps the directory it was
// captured into whatever a later version derives. Before a capture begins
// (no status.nodeName) it is SnapshotDir. A capture begun by a version that
// recorded the directory only on completion has a node and no handle; its
// directory is the one that version used (legacyNodeDir).
func NodeDir(snap *snapshotv1alpha1.SwiftSnapshot) string {
	if ms := snap.Status.MemorySnapshot; ms != nil && ms.Handle != "" {
		return strings.TrimSuffix(ms.Handle, "/")
	}
	if snap.Status.NodeName == "" {
		return SnapshotDir(snap.Namespace, snap.Name)
	}
	return legacyNodeDir(snap)
}

// legacyNodeDir is the directory versions up to v0.14.1 captured into: the
// local backend's spec.backend.local.hostPath, or "<namespace>-<name>" for
// s3 and oci.
func legacyNodeDir(snap *snapshotv1alpha1.SwiftSnapshot) string {
	if snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendLocal {
		if l := snap.Spec.Backend.Local; l != nil && l.HostPath != "" {
			return strings.TrimSuffix(l.HostPath, "/")
		}
	}
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
