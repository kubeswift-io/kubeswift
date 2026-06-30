// Package swiftrestore contains the admission webhook validator for
// SwiftRestore. It enforces same-namespace references, the
// snapshotRef/targetGuest contract, identity-regeneration rules, and
// the spec-immutability invariant documented in the snapshots design.
package swiftrestore

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// Validator validates SwiftRestore resources.
//
// Client is optional: when set, the validator looks up the referenced
// SwiftSnapshot to enforce the "macAddresses required when cloning a
// memory snapshot to a different name" rule. Without it, that rule is
// skipped (defense in depth: the controller re-checks at restore time).
type Validator struct {
	Client client.Client
}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	r, ok := obj.(*snapshotv1alpha1.SwiftRestore)
	if !ok {
		return nil, fmt.Errorf("expected SwiftRestore, got %T", obj)
	}
	return nil, v.validateSwiftRestore(ctx, r)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	r, ok := newObj.(*snapshotv1alpha1.SwiftRestore)
	if !ok {
		return nil, fmt.Errorf("expected SwiftRestore, got %T", newObj)
	}
	oldR, ok := oldObj.(*snapshotv1alpha1.SwiftRestore)
	if !ok {
		return nil, fmt.Errorf("expected SwiftRestore, got %T", oldObj)
	}
	if err := v.validateSwiftRestore(ctx, r); err != nil {
		return nil, err
	}
	// Spec is immutable: restores are one-shot operations. Re-running
	// with different inputs creates a new SwiftRestore rather than
	// mutating one in flight.
	if oldR.Spec.SnapshotRef != r.Spec.SnapshotRef ||
		oldR.Spec.TargetGuest != r.Spec.TargetGuest ||
		oldR.Spec.ResumeAfterRestore != r.Spec.ResumeAfterRestore ||
		!identityEqual(oldR.Spec.Identity, r.Spec.Identity) {
		return nil, fmt.Errorf("SwiftRestore spec is immutable")
	}
	return nil, nil
}

func (v *Validator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *Validator) validateSwiftRestore(ctx context.Context, r *snapshotv1alpha1.SwiftRestore) error {
	if r.Spec.SnapshotRef.Name == "" {
		return fmt.Errorf("spec.snapshotRef.name is required")
	}
	if r.Spec.TargetGuest.Name == "" {
		return fmt.Errorf("spec.targetGuest.name is required")
	}
	if r.Spec.SnapshotRef.Name == r.Spec.TargetGuest.Name {
		// Naming the target identically to the snapshot is almost always
		// an operator typo. Reject upfront.
		return fmt.Errorf("spec.targetGuest.name must differ from spec.snapshotRef.name")
	}
	if err := validateIdentity(r); err != nil {
		return err
	}
	if v.Client != nil {
		if err := v.validateMacAddressesOnClone(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// validateIdentity enforces:
//
//   - Each Regenerate item is a known IdentityRegenerationItem (the
//     CRD's enum already does this, but defense in depth).
//   - No duplicates (operator confusion: "did I mean it twice?").
func validateIdentity(r *snapshotv1alpha1.SwiftRestore) error {
	if r.Spec.Identity == nil || len(r.Spec.Identity.Regenerate) == 0 {
		return nil
	}
	known := map[snapshotv1alpha1.IdentityRegenerationItem]bool{
		snapshotv1alpha1.RegenHostname:     true,
		snapshotv1alpha1.RegenMachineID:    true,
		snapshotv1alpha1.RegenSSHHostKeys:  true,
		snapshotv1alpha1.RegenMACAddresses: true,
	}
	seen := map[snapshotv1alpha1.IdentityRegenerationItem]bool{}
	for _, item := range r.Spec.Identity.Regenerate {
		if !known[item] {
			return fmt.Errorf("spec.identity.regenerate contains unknown value %q", item)
		}
		if seen[item] {
			return fmt.Errorf("spec.identity.regenerate contains duplicate %q", item)
		}
		seen[item] = true
	}
	return nil
}

// validateMacAddressesOnClone enforces architect rule #6: when restoring
// a memory snapshot into a target SwiftGuest with a *different* name
// (true clone, not in-place restore), MAC addresses must be regenerated.
// Two VMs with the same MAC on the same L2 is unrecoverable from inside
// the guest — must be a hypervisor-level change at restore time.
//
// Source SwiftSnapshot may not exist yet; defer to the controller in
// that case.
func (v *Validator) validateMacAddressesOnClone(ctx context.Context, r *snapshotv1alpha1.SwiftRestore) error {
	var snap snapshotv1alpha1.SwiftSnapshot
	err := v.Client.Get(ctx, client.ObjectKey{Name: r.Spec.SnapshotRef.Name, Namespace: r.Namespace}, &snap)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up SwiftSnapshot: %w", err)
	}
	// Rule applies only to memory snapshots being cloned (not in-place). Both
	// the local (Tier B) and s3 (Tier C) backends capture via the same
	// CH-pause path, so a clone of either resumes byte-for-byte and collides
	// on MAC/identity without regeneration.
	hasMemory := snap.Status.MemorySnapshot != nil ||
		snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendLocal ||
		snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendS3
	if !hasMemory {
		return nil
	}
	isClone := r.Spec.TargetGuest.Name != snap.Spec.GuestRef.Name
	if !isClone {
		return nil
	}
	// Clone of a memory snapshot — macAddresses regeneration is required.
	hasMACRegen := false
	if r.Spec.Identity != nil {
		for _, item := range r.Spec.Identity.Regenerate {
			if item == snapshotv1alpha1.RegenMACAddresses {
				hasMACRegen = true
				break
			}
		}
	}
	if !hasMACRegen {
		return fmt.Errorf("cloning memory snapshot %s into a different target (%s != source %s) requires "+
			"spec.identity.regenerate to include macAddresses; without MAC regeneration the clone shares "+
			"network identity with the source and will conflict on the same L2 segment",
			snap.Name, r.Spec.TargetGuest.Name, snap.Spec.GuestRef.Name)
	}
	return nil
}

func identityEqual(a, b *snapshotv1alpha1.IdentityRegeneration) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a.Regenerate) != len(b.Regenerate) {
		return false
	}
	// Order-sensitive comparison — operators who reorder Regenerate
	// items are presumed to mean a change. The CRD doesn't sort.
	for i := range a.Regenerate {
		if a.Regenerate[i] != b.Regenerate[i] {
			return false
		}
	}
	return true
}
