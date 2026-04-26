// Package swiftsnapshot contains the admission webhook validator for
// SwiftSnapshot. It enforces backend-correctness, same-namespace, and the
// per-phase backend allowlist documented in docs/snapshots/csi-snapshots.md
// and docs/snapshots/local-snapshots.md.
package swiftsnapshot

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

// LocalBackendHostPathPrefix is the only filesystem prefix permitted for
// the local backend's hostPath. Operator-set paths anywhere else on the
// node are a security footgun: the snapshot directory is bind-mounted
// into a privileged launcher pod at restore time. Constraining the prefix
// limits blast radius to the kubeswift-managed subtree.
const LocalBackendHostPathPrefix = "/var/lib/kubeswift/snapshots/"

// Validator validates SwiftSnapshot resources.
type Validator struct{}

func (v *Validator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	snap, ok := obj.(*snapshotv1alpha1.SwiftSnapshot)
	if !ok {
		return nil, fmt.Errorf("expected SwiftSnapshot, got %T", obj)
	}
	return nil, validateSwiftSnapshot(snap)
}

func (v *Validator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	snap, ok := newObj.(*snapshotv1alpha1.SwiftSnapshot)
	if !ok {
		return nil, fmt.Errorf("expected SwiftSnapshot, got %T", newObj)
	}
	oldSnap, ok := oldObj.(*snapshotv1alpha1.SwiftSnapshot)
	if !ok {
		return nil, fmt.Errorf("expected SwiftSnapshot, got %T", oldObj)
	}
	if err := validateSwiftSnapshot(snap); err != nil {
		return nil, err
	}
	// Spec is immutable after creation: snapshots are point-in-time
	// captures, mutating them would break the contract callers rely on.
	if !specsEqual(&oldSnap.Spec, &snap.Spec) {
		return nil, fmt.Errorf("SwiftSnapshot spec is immutable")
	}
	return nil, nil
}

func (v *Validator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateSwiftSnapshot(snap *snapshotv1alpha1.SwiftSnapshot) error {
	if snap.Spec.GuestRef.Name == "" {
		return fmt.Errorf("spec.guestRef.name is required")
	}
	switch snap.Spec.Backend.Type {
	case snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot:
		// csi-volume-snapshot: disk-only. volumeSnapshotClassName is optional —
		// when empty the cluster's default VolumeSnapshotClass is used.
	case snapshotv1alpha1.SnapshotBackendLocal:
		if err := validateLocalBackend(snap); err != nil {
			return err
		}
	case snapshotv1alpha1.SnapshotBackendS3:
		return fmt.Errorf("spec.backend.type %q is not implemented in Phase 2; use csi-volume-snapshot or local", snap.Spec.Backend.Type)
	case "":
		return fmt.Errorf("spec.backend.type is required")
	default:
		return fmt.Errorf("spec.backend.type %q is not a recognised value", snap.Spec.Backend.Type)
	}
	// Carrier-only fields for not-yet-implemented backends, and for backends
	// other than the one selected, must be empty — otherwise the admission
	// tells the operator immediately rather than the controller silently
	// ignoring the values at runtime.
	if snap.Spec.Backend.Type != snapshotv1alpha1.SnapshotBackendLocal && snap.Spec.Backend.Local != nil {
		return fmt.Errorf("spec.backend.local is only valid when spec.backend.type=local")
	}
	if snap.Spec.Backend.S3 != nil {
		return fmt.Errorf("spec.backend.s3 is reserved for Phase 3 and must be unset")
	}
	return nil
}

// validateLocalBackend enforces the local-backend completeness rules:
//   - backend.local must be set (operator must declare where the snapshot lives)
//   - backend.local.hostPath must be set
//   - hostPath must live under LocalBackendHostPathPrefix
//   - hostPath must not contain ".." (parent-directory traversal)
func validateLocalBackend(snap *snapshotv1alpha1.SwiftSnapshot) error {
	if snap.Spec.Backend.Local == nil {
		return fmt.Errorf("spec.backend.local is required when spec.backend.type=local")
	}
	hp := snap.Spec.Backend.Local.HostPath
	if hp == "" {
		return fmt.Errorf("spec.backend.local.hostPath is required when spec.backend.type=local")
	}
	if !strings.HasPrefix(hp, LocalBackendHostPathPrefix) {
		return fmt.Errorf("spec.backend.local.hostPath must be under %s (got %q)", LocalBackendHostPathPrefix, hp)
	}
	if strings.Contains(hp, "..") {
		return fmt.Errorf("spec.backend.local.hostPath must not contain '..' (got %q)", hp)
	}
	return nil
}

func specsEqual(a, b *snapshotv1alpha1.SwiftSnapshotSpec) bool {
	if a.GuestRef != b.GuestRef {
		return false
	}
	if a.IncludeMemory != b.IncludeMemory || a.ResumeAfterSnapshot != b.ResumeAfterSnapshot {
		return false
	}
	if a.Backend.Type != b.Backend.Type {
		return false
	}
	if (a.Backend.CSIVolumeSnapshot == nil) != (b.Backend.CSIVolumeSnapshot == nil) {
		return false
	}
	if a.Backend.CSIVolumeSnapshot != nil &&
		*a.Backend.CSIVolumeSnapshot != *b.Backend.CSIVolumeSnapshot {
		return false
	}
	if (a.Backend.Local == nil) != (b.Backend.Local == nil) {
		return false
	}
	if a.Backend.Local != nil && *a.Backend.Local != *b.Backend.Local {
		return false
	}
	return true
}
