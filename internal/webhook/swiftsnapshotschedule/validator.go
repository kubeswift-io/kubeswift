// Package swiftsnapshotschedule contains the admission webhook validator for
// SwiftSnapshotSchedule (Phase 6). It validates the cron expression and the
// embedded SwiftSnapshot template — neither of which OpenAPI/CEL can check —
// plus a name-length guard for the names derived from the schedule.
package swiftsnapshotschedule

import (
	"context"
	"fmt"

	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftsnapshotwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsnapshot"
)

// maxScheduleNameLen bounds the schedule name so derived resource names stay
// within Kubernetes' 63-char limit: a scheduled snapshot is "<schedule>-<unix>"
// (+11), and that snapshot's own Jobs add up to "-s3-delete" (+10) — so
// len(name)+21 must be <= 63.
const maxScheduleNameLen = 40

// Validator validates SwiftSnapshotSchedule resources.
type Validator struct{}

func (v *Validator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	s, ok := obj.(*snapshotv1alpha1.SwiftSnapshotSchedule)
	if !ok {
		return nil, fmt.Errorf("expected SwiftSnapshotSchedule, got %T", obj)
	}
	return nil, validateSchedule(s)
}

// ValidateUpdate runs the same rules: every field of the spec is mutable (an
// operator may retune cadence/retention/template), and all rules are
// client-independent shape rules, so there is nothing to gate per-operation
// beyond re-validating the new object (per-operation discipline, Principle #10).
func (v *Validator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	s, ok := newObj.(*snapshotv1alpha1.SwiftSnapshotSchedule)
	if !ok {
		return nil, fmt.Errorf("expected SwiftSnapshotSchedule, got %T", newObj)
	}
	return nil, validateSchedule(s)
}

func (v *Validator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateSchedule(s *snapshotv1alpha1.SwiftSnapshotSchedule) error {
	if _, err := cron.ParseStandard(s.Spec.Schedule); err != nil {
		return fmt.Errorf("spec.schedule is not a valid 5-field cron expression: %w", err)
	}
	if len(s.Name) > maxScheduleNameLen {
		return fmt.Errorf("metadata.name must be at most %d characters (scheduled snapshot and Job names derive from it); got %d",
			maxScheduleNameLen, len(s.Name))
	}
	if r := s.Spec.Retention; r != nil && r.KeepLast != nil && *r.KeepLast < 1 {
		return fmt.Errorf("spec.retention.keepLast must be >= 1")
	}
	if d := s.Spec.StartingDeadlineSeconds; d != nil && *d < 0 {
		return fmt.Errorf("spec.startingDeadlineSeconds must be >= 0")
	}
	// The template must be a valid SwiftSnapshot (shape rules only — the source
	// guest need not exist at schedule-create time).
	tmpl := &snapshotv1alpha1.SwiftSnapshot{Spec: s.Spec.Template.Spec}
	if err := swiftsnapshotwebhook.ValidateShape(tmpl); err != nil {
		return fmt.Errorf("spec.template.spec is invalid: %w", err)
	}
	return nil
}
