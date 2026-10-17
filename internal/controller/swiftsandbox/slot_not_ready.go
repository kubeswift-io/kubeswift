package swiftsandbox

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// A warm slot that waits for its guest agent (slot-readiness: guest-agent)
// and whose launcher has run for longer than its not-ready timeout without
// the agent answering is replaced: a guest hung in boot, a kernel without the
// agent, or a launcher older than the controller would otherwise sit in
// status.warmingReplicas for good, in place of a working slot (#781).
//
// The timeout doubles with each consecutive replacement in the pool, up to
// MaxSlotReadyTimeout, so a failure every slot shares (version skew, a bad
// image) does not churn. It resets when a slot started after the last
// replacement gets ready: a slot that was ready all along (an older launcher
// next to broken new ones) says nothing about the new ones.

// DefaultSlotReadyTimeout is the not-ready timeout of a warm slot, from its
// launcher's start: generous, for GPU and model-preload slots.
const DefaultSlotReadyTimeout = 5 * time.Minute

// MaxSlotReadyTimeout caps the backed-off timeout.
const MaxSlotReadyTimeout = time.Hour

// launcherStartedAt is when a slot's launcher container started running.
func launcherStartedAt(p *corev1.Pod) (time.Time, bool) {
	for i := range p.Status.ContainerStatuses {
		cs := &p.Status.ContainerStatuses[i]
		if cs.Name == launcherName && cs.State.Running != nil {
			return cs.State.Running.StartedAt.Time, true
		}
	}
	return time.Time{}, false
}

// slotReadyTimeout is the pool's current not-ready timeout: the base doubled
// once per consecutive replacement, capped. 0 disables replacement.
func (r *SwiftSandboxPoolReconciler) slotReadyTimeout(pool types.NamespacedName) time.Duration {
	base := r.SlotReadyTimeout
	if base <= 0 {
		return 0
	}
	n := 0
	if v, ok := r.notReadyStreak.Load(pool); ok {
		n = v.(notReadyStreak).n
	}
	d := base
	for i := 0; i < n && d < MaxSlotReadyTimeout; i++ {
		d *= 2
	}
	return min(d, MaxSlotReadyTimeout)
}

// slotNeverReady reports whether a warm slot has waited for its guest agent
// past timeout, and for how long.
func slotNeverReady(p *corev1.Pod, timeout time.Duration, now time.Time) (time.Duration, bool) {
	if timeout <= 0 || p.Annotations[SlotReadinessAnnotation] != slotReadinessGuestAgent || slotReady(p) {
		return 0, false
	}
	started, ok := launcherStartedAt(p)
	if !ok || started.IsZero() {
		return 0, false
	}
	waited := now.Sub(started)
	return waited, waited > timeout
}

// notReadyStreak is a pool's consecutive replacements, and when the last was.
type notReadyStreak struct {
	n    int
	last time.Time
}

// recordNotReady counts a replacement at now.
func (r *SwiftSandboxPoolReconciler) recordNotReady(pool types.NamespacedName, now time.Time) {
	st := notReadyStreak{n: 1, last: now}
	if v, ok := r.notReadyStreak.Load(pool); ok {
		st.n = v.(notReadyStreak).n + 1
	}
	r.notReadyStreak.Store(pool, st)
}

// slotGotReady resets the streak when p, a ready slot, was started after the
// pool's last replacement.
func (r *SwiftSandboxPoolReconciler) slotGotReady(pool types.NamespacedName, p *corev1.Pod) {
	v, ok := r.notReadyStreak.Load(pool)
	if !ok {
		return
	}
	if started, ok := launcherStartedAt(p); ok && started.After(v.(notReadyStreak).last) {
		r.notReadyStreak.Delete(pool)
	}
}
