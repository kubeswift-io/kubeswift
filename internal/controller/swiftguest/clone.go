package swiftguest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
)

// prepareCloneFromSnapshot prepares a SwiftGuest that boots via
// spec.cloneFromSnapshot (Snapshot Phase 4). It resolves the referenced
// SwiftSnapshot and the LIVE source guest, self-stamps the restore-receive
// annotations (so the existing annotation-driven restore path in
// buildBasePod/RestoreParamsFromAnnotations + the runtime intent fire
// unchanged), and returns an "effective" guest carrying the SOURCE guest's spec
// for the resolver — the clone guest itself has no imageRef. Only the spec is
// overlaid (for resolution); the real guest keeps its identity
// (name/namespace/annotations) and is used everywhere else.
//
// Returns (effective, preResolved, failReason, requeue, err):
//   - effective != nil   → resolve THIS instead of the real guest.
//   - preResolved != nil → skip the resolver entirely and use THIS ResolvedGuest
//     (the source-independent path — the source guest/image/seedProfile are gone
//     and rg is built from the snapshot's captured surface via FromCapturedSpec).
//   - failReason != ""   → set Resolved=False / phase=Failed (terminal).
//   - requeue            → snapshot not Ready yet; re-reconcile.
//
// PR 3a handles Tier B (local, same-node) snapshots; Tier C (s3/oci) adds the
// download path. When the source guest still exists its live spec is used (the
// validated same-cluster path). When it is GONE and the snapshot is a full-state
// oci snapshot carrying the captured launcher-sufficient surface (SI PR1), the
// clone resolves source-independently — the fully cross-cluster path
// (docs/design/oras-cold-migration-source-independent.md).
func (r *SwiftGuestReconciler) prepareCloneFromSnapshot(
	ctx context.Context, guest *swiftv1alpha1.SwiftGuest,
) (*swiftv1alpha1.SwiftGuest, *resolved.ResolvedGuest, string, bool, error) {
	src := guest.Spec.CloneFromSnapshot

	var snap snapshotv1alpha1.SwiftSnapshot
	if err := r.Get(ctx, client.ObjectKey{Name: src.SnapshotRef.Name, Namespace: guest.Namespace}, &snap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, "SwiftSnapshot " + src.SnapshotRef.Name + " not found", false, nil
		}
		return nil, nil, "", false, err
	}
	if snap.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseReady {
		// Transient — the snapshot may still be Capturing/Uploading.
		return nil, nil, "", true, nil
	}

	// Resolve the on-node snapshot directory + the node the clone runs on.
	// Tier B (local): the snapshot already lives on its capture node. Tier C
	// (s3): a download Job — shared per (node, snapshot) across all clones that
	// land on the same node — pulls the artifacts into that node's cache first
	// (mirrors the SwiftRestore download path); the clone then boots once the
	// cache is populated.
	var snapshotPath, node string
	switch snap.Spec.Backend.Type {
	case snapshotv1alpha1.SnapshotBackendLocal:
		if snap.Status.NodeName == "" || snap.Spec.Backend.Local == nil || snap.Spec.Backend.Local.HostPath == "" {
			return nil, nil, "SwiftSnapshot " + snap.Name + " is missing status.nodeName or backend.local.hostPath", false, nil
		}
		snapshotPath, node = snap.Spec.Backend.Local.HostPath, snap.Status.NodeName
	case snapshotv1alpha1.SnapshotBackendS3:
		node = src.TargetNode
		if node == "" {
			return nil, nil, "cloneFromSnapshot from a Tier C (s3) snapshot requires spec.cloneFromSnapshot.targetNode", false, nil
		}
		if snap.Status.S3 == nil || snap.Status.S3.Location == "" {
			return nil, nil, "SwiftSnapshot " + snap.Name + " has no status.s3 — its upload is not complete", false, nil
		}
		done, failReason, derr := r.ensureCloneDownloadJob(ctx, guest, &snap, node)
		if derr != nil {
			return nil, nil, "", false, derr
		}
		if failReason != "" {
			return nil, nil, failReason, false, nil
		}
		if !done {
			// Still downloading the snapshot artifacts onto the target node.
			return nil, nil, "", true, nil
		}
		snapshotPath = clonecommon.S3LocalDir(&snap)
	case snapshotv1alpha1.SnapshotBackendOCI:
		node = src.TargetNode
		if node == "" {
			return nil, nil, "cloneFromSnapshot from an oci snapshot requires spec.cloneFromSnapshot.targetNode", false, nil
		}
		if snap.Status.OCI == nil || snap.Status.OCI.ManifestDigest == "" {
			return nil, nil, "SwiftSnapshot " + snap.Name + " has no status.oci — its push is not complete", false, nil
		}
		done, failReason, derr := r.ensureCloneDownloadJob(ctx, guest, &snap, node)
		if derr != nil {
			return nil, nil, "", false, derr
		}
		if failReason != "" {
			return nil, nil, failReason, false, nil
		}
		if !done {
			// Still pulling the snapshot artifacts onto the target node.
			return nil, nil, "", true, nil
		}
		snapshotPath = clonecommon.S3LocalDir(&snap)
	default:
		return nil, nil, "cloneFromSnapshot requires a memory snapshot (backend.type: local, s3, or oci); got " + string(snap.Spec.Backend.Type), false, nil
	}

	// The clone prefers the source guest's full spec (image/seed/class) to build
	// the launcher pod — a fresh disk from the source image plus the restored
	// memory. When the source is GONE, a full-state oci snapshot carrying the
	// captured launcher-sufficient surface (SI PR1) can still clone
	// source-independently — the disk comes from the oci disk artifact and rg is
	// built from the captured surface instead of a live spec.
	var source swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: snap.Spec.GuestRef.Name, Namespace: guest.Namespace}, &source); err != nil {
		if apierrors.IsNotFound(err) {
			return r.prepareSourceIndependentClone(ctx, guest, &snap, snapshotPath, node)
		}
		return nil, nil, "", false, err
	}

	// Self-stamp the clone-mode restore annotations (mirrors what the
	// SwiftRestore controller stamps on its clone targets) if not already set.
	annos := cloneRestoreAnnotations(guest, &snap, &source, snapshotPath, node)
	if err := r.stampCloneAnnotations(ctx, guest, annos); err != nil {
		return nil, nil, "", false, err
	}

	// Effective guest: the real guest's identity (name/namespace/annotations/
	// status) with the SOURCE guest's spec, for the resolver only. runPolicy is
	// clone-owned (it governs the clone's lifecycle via rg.Lifecycle), so keep
	// the clone's rather than inheriting the source's.
	effective := guest.DeepCopy()
	clonePolicy := guest.Spec.RunPolicy
	effective.Spec = source.Spec
	effective.Spec.RunPolicy = clonePolicy
	return effective, nil, "", false, nil
}

// prepareSourceIndependentClone handles a cloneFromSnapshot whose SOURCE guest no
// longer exists — the fully cross-cluster path
// (docs/design/oras-cold-migration-source-independent.md). It requires a
// FULL-STATE oci snapshot (status.oci.disk — the frozen runtime disk is in the
// registry, so no SwiftImage is needed) carrying the captured
// launcher-sufficient surface (status.guestSpec.storage — SI PR1); anything else
// genuinely needs the live source spec and fails with the pre-SI message.
//
// The returned ResolvedGuest is built by resolved.FromCapturedSpec: the clone's
// own guestClass supplies the system-default shell, the captured surface
// supplies the resume-specific config, RootDisk.FromOCI routes the root disk to
// maybeRootDiskFromOCI, and — when the source had a seedProfile — a minimal
// placeholder NoCloud seed satisfies CH --restore's re-open of the seed.iso
// disk recorded in config.json (a resume never reads seed content; see design
// D3 — a later reboot has no real seed, the vsock agent is the identity path).
func (r *SwiftGuestReconciler) prepareSourceIndependentClone(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	snap *snapshotv1alpha1.SwiftSnapshot,
	snapshotPath, node string,
) (*swiftv1alpha1.SwiftGuest, *resolved.ResolvedGuest, string, bool, error) {
	captured := snap.Status.GuestSpec
	if snap.Status.OCI == nil || snap.Status.OCI.Disk == nil || snap.Status.OCI.Disk.ManifestDigest == "" ||
		captured == nil || captured.Storage == nil {
		// Not a full-state snapshot with the captured surface — the
		// pre-source-independence contract applies.
		return nil, nil, "source SwiftGuest " + snap.Spec.GuestRef.Name + " no longer exists; cloneFromSnapshot needs the source spec (only a full-state oci snapshot with a captured guest spec clones source-independently)", false, nil
	}
	// v1.1: a full-state snapshot may carry the source's data disks as their own
	// oci artifacts (status.oci.dataDisks). If the source HAD data disks but the
	// capture produced no such artifacts, it is a pre-v1.1 (root-disk-only)
	// snapshot — reject, the source spec is required. When artifacts are present
	// the import path (ensureCloneDataDisks) materializes + attaches them, so the
	// clone is fully source-independent.
	if captured.HasDataDisks && len(snap.Status.OCI.DataDisks) == 0 {
		return nil, nil, "source-independent clone: snapshot " + snap.Name + "'s source had data disks but the capture carries none (pre-v1.1 root-disk-only snapshot); the source spec is required", false, nil
	}
	var class swiftv1alpha1.SwiftGuestClass
	if err := r.Get(ctx, client.ObjectKey{Name: guest.Spec.GuestClassRef.Name}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, "SwiftGuestClass " + guest.Spec.GuestClassRef.Name + " not found", false, nil
		}
		return nil, nil, "", false, err
	}

	cpu, _ := strconv.Atoi(captured.CPU) // "" → 0; FromCapturedSpec output feeds pod limits only (CH --restore uses config.json)
	in := resolved.CapturedInput{
		CPU:              cpu,
		MemoryMi:         int(captured.MemoryMi),
		RootDiskSize:     captured.RootDiskSize,
		AccessMode:       captured.Storage.AccessMode,
		VolumeMode:       captured.Storage.VolumeMode,
		StorageClassName: captured.Storage.StorageClassName,
		Network:          captured.Network,
		OSType:           captured.OSType,
		InterfaceNames:   captured.InterfaceNames,
	}
	rg := resolved.FromCapturedSpec(guest, &class, in)
	if captured.HasSeed {
		rg.Seed = placeholderSeed(guest.Name)
	}

	// A stub source (identity + the captured interface names + agent opt-in)
	// makes the shared annotation builder work unchanged: MAC rewrites seed from
	// interface names, the runtime-dir FROM prefix derives from the source's
	// ns/name — which config.json records — and agent-enablement carries over.
	// Cross-cluster note: the FROM prefix uses snap.Namespace, so the snapshot
	// must be recreated in a namespace with the SAME NAME as the source's
	// original namespace (documented in the runbook).
	stub := stubSourceFromCaptured(snap, captured)
	annos := cloneRestoreAnnotations(guest, snap, stub, snapshotPath, node)
	if err := r.stampCloneAnnotations(ctx, guest, annos); err != nil {
		return nil, nil, "", false, err
	}
	return nil, rg, "", false, nil
}

// stampCloneAnnotations idempotently patches the clone-mode restore annotations
// onto the real guest (in-cluster + in-memory).
func (r *SwiftGuestReconciler) stampCloneAnnotations(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, annos map[string]string) error {
	if cloneAnnotationsMatch(guest.Annotations, annos) {
		return nil
	}
	patched := guest.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	for k, v := range annos {
		patched.Annotations[k] = v
	}
	if err := r.Patch(ctx, patched, client.MergeFrom(guest)); err != nil {
		return err
	}
	// Reflect the stamp in-memory so the rest of this reconcile sees it.
	guest.Annotations = patched.Annotations
	return nil
}

// stubSourceFromCaptured synthesizes a minimal source SwiftGuest from the
// snapshot's captured surface, carrying exactly what cloneRestoreAnnotations
// consumes: identity (ns/name → runtime-dir FROM prefix), interface names (MAC
// rewrites), and the guest-agent opt-in.
func stubSourceFromCaptured(snap *snapshotv1alpha1.SwiftSnapshot, captured *snapshotv1alpha1.CapturedGuestSpec) *swiftv1alpha1.SwiftGuest {
	stub := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: snap.Spec.GuestRef.Name, Namespace: snap.Namespace},
	}
	for _, n := range captured.InterfaceNames {
		stub.Spec.Interfaces = append(stub.Spec.Interfaces, swiftv1alpha1.GuestInterface{Name: n})
	}
	if captured.GuestAgent {
		stub.Spec.GuestAgent = &swiftv1alpha1.GuestAgentSpec{Enabled: true}
	}
	return stub
}

// placeholderSeed is the minimal NoCloud seed for a source-independent clone
// whose source had a seedProfile. CH --restore re-opens the seed.iso disk path
// recorded in config.json and refuses to restore when the file is missing; the
// launcher rebuilds seed.iso from the seed ConfigMap, so giving rg a minimal
// seed makes the existing ConfigMap + ISO machinery produce a file at the right
// path. The resumed guest never reads it (cloud-init already ran in the
// captured state). Content is deliberately inert.
func placeholderSeed(cloneName string) *resolved.Seed {
	return &resolved.Seed{
		Datasource: "NoCloud",
		MetaData:   "instance-id: " + cloneName + "\nlocal-hostname: " + cloneName + "\n",
		UserData:   "#cloud-config\n# Placeholder seed for a source-independent full-state clone.\n# The resumed guest never reads this (cloud-init already ran before capture).\n",
	}
}

// cloneRestoreAnnotations builds the clone-mode restore-receive annotations for
// a cloneFromSnapshot guest (Tier B local). MAC rewrites + runtime-dir prefixes
// use the clonecommon primitives shared with the SwiftRestore clone path.
func cloneRestoreAnnotations(
	guest *swiftv1alpha1.SwiftGuest,
	snap *snapshotv1alpha1.SwiftSnapshot,
	source *swiftv1alpha1.SwiftGuest,
	snapshotPath, node string,
) map[string]string {
	annos := map[string]string{
		AnnotationActiveRestore:               snap.Name,
		AnnotationRestoreSnapshotPath:         snapshotPath,
		AnnotationRestoreNodeName:             node,
		AnnotationRestoreMode:                 RestoreModeClone,
		AnnotationRestoreMACRewrites:          clonecommon.ComputeMACRewrites(guest.Namespace, guest.Name, source),
		AnnotationRestoreRuntimeDirFromPrefix: clonecommon.RuntimeDirPrefix(source.Namespace, source.Name),
		AnnotationRestoreRuntimeDirToPrefix:   clonecommon.RuntimeDirPrefix(guest.Namespace, guest.Name),
		AnnotationRestoreNullifyHostMAC:       "true",
		// CH v52: resume the clone on restore (it loads paused otherwise) so we
		// don't need the resumeCloneIfNeeded action round-trip (Bug #73). Only
		// the cloneFromSnapshot path sets this; SwiftRestore drives resume itself.
		AnnotationRestoreAutoResume: "true",
		// CH v52: userfaultfd demand-paged restore. cloneFromSnapshot defaults
		// to ondemand — fast pool scale-up is the goal and the latency win is
		// the point; the snapshot file is local + mounted read-only for the
		// pod's lifetime, so lazy paging is safe (snapshot-ch-v52-efficiency.md).
		AnnotationRestoreMemoryMode: "ondemand",
	}
	// In-guest identity agent: if the SOURCE opted in (and the device is in the
	// snapshot), the agent regenerates identity in place over vsock once the
	// clone is Running (ensureCloneIdentityRegen). Record it on the clone so the
	// controller knows to drive the action — and SKIP the legacy reboot-bootcmd
	// cmdline marker, since the agent owns regeneration (the two must not both
	// fire). When the agent is absent, fall back to the marker (broken on CH
	// v52; docs/snapshots/identity-regeneration.md).
	agentEnabled := source.Spec.GuestAgent != nil && source.Spec.GuestAgent.Enabled
	if agentEnabled {
		annos[AnnotationCloneAgentEnabled] = "true"
	} else if cloneRegenIncludesNonMAC(guest.Spec.CloneFromSnapshot) {
		annos[AnnotationRestoreAppendCmdlineMarker] = "true"
	}
	return annos
}

// cloneRegenIncludesNonMAC reports whether the clone requests regeneration of a
// non-MAC identity item (hostname/machineId/sshHostKeys) — these fire on the
// clone's first reboot via the seed bootcmd. An empty Regenerate defaults to
// all four. macAddresses is handled separately (always forced via MAC rewrites).
func cloneRegenIncludesNonMAC(src *swiftv1alpha1.CloneFromSnapshotSource) bool {
	if src == nil || len(src.Regenerate) == 0 {
		return true
	}
	for _, item := range src.Regenerate {
		switch item {
		case swiftv1alpha1.CloneRegenHostname, swiftv1alpha1.CloneRegenMachineID, swiftv1alpha1.CloneRegenSSHHostKeys:
			return true
		}
	}
	return false
}

// cloneAnnotationsMatch reports whether every want key is already present in
// have with the same value (so the stamp is idempotent — no re-patch).
func cloneAnnotationsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// cloneDownloadJobName is the deterministic name of the Tier C clone download
// Job, keyed by (node, snapshot) — NOT by the guest. Every clone that lands on
// the same node from the same snapshot resolves to the SAME Job name, so they
// share one downloader instead of racing concurrent writers on the shared
// node-local cache hostPath (S3LocalDir is snapshot-keyed and identical on a
// given node). The (namespace, name, node) tuple is hashed to a short, always
// DNS-1123-valid suffix (node names can be long or contain characters invalid
// in a resource name). The snapshot lives in the guest's namespace, so the
// namespace is constant across the clones that could collide; it is folded into
// the hash for completeness.
func cloneDownloadJobName(snap *snapshotv1alpha1.SwiftSnapshot, node string) string {
	sum := sha256.Sum256([]byte(snap.Namespace + "/" + snap.Name + "@" + node))
	return "clone-dl-" + hex.EncodeToString(sum[:8])
}

// ensureCloneDownloadJob creates (idempotently) a node-pinned download Job that
// pulls a Tier C snapshot's artifacts into the target node's cache, and reports
// whether it has completed. Returns (done, failReason, err):
//   - done=true       → the cache is populated; proceed to boot the clone.
//   - failReason != "" → terminal (image unset, or the Job failed).
//   - otherwise        → still downloading (caller requeues).
//
// Dedup per (node, snapshot): the Job name (cloneDownloadJobName) and the cache
// dir (clonecommon.S3LocalDir) are both functions of the snapshot, not the
// guest, so every clone on a given node from the same snapshot converges on ONE
// download Job. Concurrent reconciles all Get-then-Create the same name; the
// first wins and the rest get AlreadyExists (swallowed below). That guarantees a
// single writer to the snapshot-keyed hostPath, eliminating the concurrent-write
// race a SwiftGuestPool would otherwise hit when it places more replicas than
// nodes — which is what lifts the prior "keep replicas <= schedulable nodes"
// constraint. (A second clone arriving after the cache is already populated is a
// fast checksum-verified no-op via the snapshot-s3 binary's idempotency.)
//
// Ownership is the requesting clone (the race-winner that first creates it),
// matching how every other guest-owned Job in this controller works (e.g. the
// rootdisk clone Job) — so this adds NO new finalizers-RBAC surface (a
// SwiftSnapshot owner would, via OwnerReferencesPermissionEnforcement on
// hardened clusters, a 403 the default dev cluster wouldn't catch). A sibling
// clone that finds the Job already present just reads it; it is not an owner and
// is woken by its own 5s requeue, not the Owns(Job) watch. If the owning clone
// is deleted mid-download the Job is GC'd, but a surviving sibling's next
// reconcile recreates it by the same name and the snapshot-s3 binary resumes
// idempotently (the same recreate-on-missing self-heal the SwiftRestore download
// path uses); once the cache is populated the Job is no longer load-bearing —
// the artifacts live on the node-local hostPath, not in the Job.
func (r *SwiftGuestReconciler) ensureCloneDownloadJob(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	snap *snapshotv1alpha1.SwiftSnapshot,
	node string,
) (bool, string, error) {
	// The Job lives in the snapshot's namespace (== the guest's namespace).
	name := cloneDownloadJobName(snap, node)
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: snap.Namespace}, &job)
	if apierrors.IsNotFound(err) {
		j, failReason := r.buildCloneDownloadJob(snap, node, name)
		if failReason != "" {
			return false, failReason, nil
		}
		if cerr := ctrl.SetControllerReference(guest, j, r.Scheme); cerr != nil {
			return false, "", cerr
		}
		if cerr := r.Create(ctx, j); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return false, "", cerr
		}
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			// Record the clone download's wire traffic once per shared
			// (node, snapshot) Job (the SwiftGuest controller re-reads the
			// completed Job every reconcile; clones have no status byte field).
			if metrics.MarkCloneDownloadObserved(name + "@" + node) {
				if rep, ok, rerr := clonecommon.JobTransferReport(ctx, r.Client, snap.Namespace, name); rerr == nil && ok {
					metrics.RestoreDownloadBytesTotal.Add(float64(rep.TransferredBytes))
				}
			}
			return true, "", nil
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return false, "snapshot download Job failed: " + c.Message, nil
		}
	}
	return false, "", nil
}

// buildCloneDownloadJob builds the s3 or oci clone download Job for the
// snapshot's backend. Returns (job, failReason): a non-empty failReason is
// terminal (the required transfer image is not configured).
func (r *SwiftGuestReconciler) buildCloneDownloadJob(snap *snapshotv1alpha1.SwiftSnapshot, node, name string) (*batchv1.Job, string) {
	labels := map[string]string{"kubeswift.io/snapshot": snap.Name}
	if snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendOCI {
		if r.SnapshotORASImage == "" {
			return nil, "snapshot-oras image not configured (set KUBESWIFT_SNAPSHOT_ORAS_IMAGE)"
		}
		oci := snap.Spec.Backend.OCI
		credName := ""
		if oci.CredentialsSecretRef != nil {
			credName = oci.CredentialsSecretRef.Name
		}
		return clonecommon.BuildOCIDownloadJob(clonecommon.OCIDownloadJobParams{
			Snapshot:              snap,
			Repository:            oci.Repository,
			Tag:                   cloneOCITag(snap),
			Digest:                snap.Status.OCI.ManifestDigest,
			Insecure:              oci.Insecure,
			CredentialsSecretName: credName,
			Image:                 r.SnapshotORASImage,
			Name:                  name,
			Namespace:             snap.Namespace,
			Node:                  node,
			Component:             "snapshot-oci-clone-download",
			ExtraLabels:           labels,
		}), ""
	}
	if r.SnapshotS3Image == "" {
		return nil, "snapshot-s3 image not configured (set KUBESWIFT_SNAPSHOT_S3_IMAGE)"
	}
	return clonecommon.BuildDownloadJob(clonecommon.DownloadJobParams{
		Snapshot:    snap,
		Image:       r.SnapshotS3Image,
		Name:        name,
		Namespace:   snap.Namespace,
		Node:        node,
		Component:   "snapshot-s3-clone-download",
		ExtraLabels: labels,
	}), ""
}

// cloneOCITag resolves the artifact tag: the operator-supplied tag, or the
// "<namespace>-<name>" default (matching the capture + restore sides).
func cloneOCITag(snap *snapshotv1alpha1.SwiftSnapshot) string {
	if snap.Spec.Backend.OCI != nil && snap.Spec.Backend.OCI.Tag != "" {
		return snap.Spec.Backend.OCI.Tag
	}
	return snap.Namespace + "-" + snap.Name
}

// maybeRootDiskFromSourceClone materializes the root disk for a MEMORY-ONLY
// cloneFromSnapshot (Tier B local, or Tier C s3 without includeDisk) as a CSI
// clone (spec.dataSource) of the SOURCE guest's live root PVC — a byte copy of
// the source's actual disk, so its on-disk partition+filesystem geometry (and
// data) match the RAM the clone resumes.
//
// Returns (handled, result, err), mirroring maybeRootDiskFromOCI:
//   - handled=false  → not a memory-only clone (no cloneFromSnapshot, or a
//     full-state oci clone maybeRootDiskFromOCI already took); the caller falls
//     through to the image-clone paths.
//   - handled=true, err=nil, result set → the CSI clone is Bound and ready.
//   - handled=true, err!=nil → transient "requeue and retry" progress signal
//     (PVC just created / not yet Bound) OR a terminal failure (source disk gone).
//
// WHY (the bug this fixes): the legacy path copies the pristine SwiftImage, whose
// partition is the original (small) cloud-image size. A normal guest grows it via
// cloud-init growpart/resize2fs on FIRST BOOT — but a clone RESUMES captured RAM,
// so cloud-init never re-runs. The resumed guest's in-RAM ext4 is the source's
// already-grown filesystem; synced onto the small-partition image copy it leaves
// fs > partition, which the next reboot's initramfs rejects ("EXT4-fs: bad
// geometry"), dropping the guest to the emergency shell (looked like a firmware
// hang). Cloning the source's real disk keeps geometry consistent. See
// docs/design/known-issues-clone-reboot-firmware-hang.md.
func (r *SwiftGuestReconciler) maybeRootDiskFromSourceClone(
	ctx context.Context, guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest,
) (bool, *RootDiskCloneResult, error) {
	if guest.Spec.CloneFromSnapshot == nil {
		return false, nil, nil
	}
	var snap snapshotv1alpha1.SwiftSnapshot
	if err := r.Get(ctx, client.ObjectKey{Name: guest.Spec.CloneFromSnapshot.SnapshotRef.Name, Namespace: guest.Namespace}, &snap); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil, nil // prepareCloneFromSnapshot surfaces the missing snapshot
		}
		return true, nil, err
	}
	// Full-state (includeDisk oci) clones carry their disk in the registry and are
	// handled by maybeRootDiskFromOCI (called before this). Only memory-only
	// snapshots reach here needing the source guest's disk.
	if snap.Status.OCI != nil && snap.Status.OCI.Disk != nil && snap.Status.OCI.Disk.ManifestDigest != "" {
		return false, nil, nil
	}

	sourceRootPVC := RootDiskCloneName(snap.Spec.GuestRef.Name)
	cloneName := RootDiskCloneName(guest.Name)

	// The source guest's root PVC supplies the disk. A memory-only clone is
	// same-cluster and needs the live source (its RAM references that disk). If it
	// is gone, we cannot build a consistent disk — fail loudly (Principle #6).
	var srcPVC corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: sourceRootPVC, Namespace: guest.Namespace}, &srcPVC); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil, fmt.Errorf("cloneFromSnapshot: source root PVC %s not found — a memory-only clone needs the source guest's disk (use a full-state includeDisk snapshot for source-independent clones)", sourceRootPVC)
		}
		return true, nil, err
	}

	var pvc corev1.PersistentVolumeClaim
	perr := r.Get(ctx, client.ObjectKey{Name: cloneName, Namespace: guest.Namespace}, &pvc)
	if apierrors.IsNotFound(perr) {
		// CSI-clone the source's live disk. Same size/class/mode as the source so
		// it is a byte clone (CSI cloning requires matching capacity + class);
		// Longhorn clones an attached source volume without detaching it.
		// RestoreSeeded so the copy path (ensureRootDiskCloneFromCopy) skips it.
		newPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cloneName,
				Namespace: guest.Namespace,
				Labels: map[string]string{
					"swift.kubeswift.io/guest": guest.Name,
					"swift.kubeswift.io/role":  "root-disk",
					RestoreSeededLabel:         "true",
				},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, swiftGuestGVK)},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      srcPVC.Spec.AccessModes,
				VolumeMode:       srcPVC.Spec.VolumeMode,
				StorageClassName: srcPVC.Spec.StorageClassName,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: srcPVC.Spec.Resources.Requests[corev1.ResourceStorage],
					},
				},
				DataSource: &corev1.TypedLocalObjectReference{
					Kind: "PersistentVolumeClaim",
					Name: sourceRootPVC,
				},
			},
		}
		if err := r.Create(ctx, newPVC); err != nil && !apierrors.IsAlreadyExists(err) {
			return true, nil, fmt.Errorf("create clone-from-source PVC %s: %w", cloneName, err)
		}
		return true, nil, fmt.Errorf("clone-from-source PVC %s created (CSI clone of %s), waiting for Bound", cloneName, sourceRootPVC)
	}
	if perr != nil {
		return true, nil, perr
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		return true, nil, fmt.Errorf("clone-from-source PVC %s not yet Bound (phase=%s)", cloneName, pvc.Status.Phase)
	}
	// Byte clone of the source's already-grown disk → geometry is consistent;
	// never grow-init (that would desync partition vs the resumed fs).
	return true, &RootDiskCloneResult{PVCName: cloneName, NeedsGrowInit: false}, nil
}
