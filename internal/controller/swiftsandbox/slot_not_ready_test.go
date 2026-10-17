package swiftsandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func startedSlot(p *corev1.Pod, at time.Time) *corev1.Pod {
	p.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(at)
	return p
}

// A warm slot whose guest agent never answers is replaced once its launcher
// has run past the timeout; the timeout backs off per pool and resets once
// a slot gets ready. Slots that do not wait for the agent, or are within the
// timeout, are left alone (#781).
func TestPoolReconcile_ReplacesASlotThatNeverGetsReady(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	pool := testPool(4) // nothing is surplus: only the timeout deletes
	hung := startedSlot(warmSlot(pool, "p-slot-hung", true, false), now.Add(-6*time.Minute))
	young := startedSlot(warmSlot(pool, "p-slot-young", true, false), now.Add(-time.Minute))
	legacy := startedSlot(warmSlot(pool, "p-slot-legacy", false, false), now.Add(-time.Hour))
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool, hung, young, legacy).WithStatusSubresource(pool).Build()
	rec := record.NewFakeRecorder(20)
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme, Recorder: rec, SlotReadyTimeout: 5 * time.Minute, now: func() time.Time { return now }}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "p"}}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	gone := func(p *corev1.Pod) bool {
		var got corev1.Pod
		err := c.Get(ctx, client.ObjectKeyFromObject(p), &got)
		return apierrors.IsNotFound(err) || (err == nil && got.DeletionTimestamp != nil)
	}
	reconcile()
	if !gone(hung) || gone(young) || gone(legacy) {
		t.Fatalf("hung gone=%v young gone=%v legacy gone=%v", gone(hung), gone(young), gone(legacy))
	}
	ev := drainRecorder(rec)
	if !strings.Contains(ev, "SlotNotReady") || !strings.Contains(ev, "next timeout is 10m0s") {
		t.Errorf("events %q", ev)
	}
	if d := r.slotReadyTimeout(req.NamespacedName); d != 10*time.Minute {
		t.Errorf("backed-off timeout %s", d)
	}

	// Within the backed-off timeout (10m), a 7-minute-old unready slot stays.
	seven := startedSlot(warmSlot(pool, "p-slot-seven", true, false), now.Add(-7*time.Minute))
	if err := c.Create(ctx, seven); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if gone(seven) {
		t.Error("replaced within the backed-off timeout")
	}

	// A slot started before the replacement getting ready says nothing
	// about the new ones: no reset.
	var y corev1.Pod
	_ = c.Get(ctx, client.ObjectKeyFromObject(young), &y)
	y.Annotations[GuestAgentReadyAnnotation] = "2026-10-17T00:00:00.000Z"
	if err := c.Update(ctx, &y); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if d := r.slotReadyTimeout(req.NamespacedName); d != 10*time.Minute {
		t.Errorf("an older slot reset the backoff: %s", d)
	}
	// A slot started after it gets ready: reset.
	fresh := startedSlot(warmSlot(pool, "p-slot-fresh", true, true), now.Add(time.Second))
	if err := c.Create(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if d := r.slotReadyTimeout(req.NamespacedName); d != 5*time.Minute {
		t.Errorf("timeout after a ready slot: %s", d)
	}

	// The backoff is capped.
	r.notReadyStreak.Store(req.NamespacedName, notReadyStreak{n: 50, last: now})
	if d := r.slotReadyTimeout(req.NamespacedName); d != MaxSlotReadyTimeout {
		t.Errorf("cap: %s", d)
	}
	// 0 never replaces.
	r.SlotReadyTimeout = 0
	if _, never := slotNeverReady(hung, r.slotReadyTimeout(req.NamespacedName), now); never {
		t.Error("timeout 0 replaced a slot")
	}
}

func drainRecorder(rec *record.FakeRecorder) string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return strings.Join(out, "\n")
		}
	}
}
