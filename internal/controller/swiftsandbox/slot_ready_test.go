package swiftsandbox

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// warmSlot is a warm slot pod of pool p whose launcher runs. stamped: created
// by a controller that waits for the guest agent; agent: swiftletd reported
// that the agent answers.
func warmSlot(pool *sandboxv1alpha1.SwiftSandboxPool, name string, stamped, agent bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: pool.Namespace, UID: types.UID("uid-" + name),
			Labels:      map[string]string{PoolLabelKey: pool.Name, SlotStateLabelKey: slotStateWarm, SlotNameLabelKey: name},
			Annotations: map[string]string{SlotProfileAnnotation: poolSlotProfile(pool)},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: launcherName, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	if stamped {
		p.Annotations[SlotReadinessAnnotation] = slotReadinessGuestAgent
	}
	if agent {
		p.Annotations[GuestAgentReadyAnnotation] = "2026-10-11T00:00:00.000Z"
	}
	return p
}

func testPool(minWarm int32) *sandboxv1alpha1.SwiftSandboxPool {
	return &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "pool-uid"},
		Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "busybox:1", MinWarm: minWarm},
		// A digest, so a reconcile that creates nothing asks no registry.
		Status: sandboxv1alpha1.SwiftSandboxPoolStatus{
			Rootfs: &sandboxv1alpha1.SandboxRootfsStatus{Digest: "sha256:deadbeef"},
		},
	}
}

func TestSlotReady(t *testing.T) {
	pool := testPool(1)
	deleting := warmSlot(pool, "deleting", true, true)
	deleting.DeletionTimestamp = &metav1.Time{}
	pending := warmSlot(pool, "pending", true, true)
	pending.Status = corev1.PodStatus{Phase: corev1.PodPending}
	initing := warmSlot(pool, "initing", true, false)
	initing.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{{
		Name: materializeInitName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}}
	ended := warmSlot(pool, "ended", true, true)
	ended.Status = corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
		Name: launcherName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
	}}}
	for _, tc := range []struct {
		pod  *corev1.Pod
		want bool
	}{
		// The old rule, which this fixes: a running launcher whose guest is
		// still booting was handed to a checkout, whose exec then failed.
		{warmSlot(pool, "booting", true, false), false},
		{warmSlot(pool, "agent-up", true, true), true},
		// Created before launchers reported the agent: ready when its
		// launcher runs, as before, so an upgrade does not stop checkouts
		// until every slot is replaced.
		{warmSlot(pool, "legacy", false, false), true},
		{initing, false},
		{pending, false},
		{ended, false},
		{deleting, false},
	} {
		if got := slotReady(tc.pod); got != tc.want {
			t.Errorf("%s: slotReady = %v, want %v", tc.pod.Name, got, tc.want)
		}
	}
}

// A new slot says it is ready only once its guest agent answers, and its
// launcher is told to report that.
func TestCreateWarmSlot_WaitsForTheGuestAgent(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool).Build()
	pr := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(10)}
	if err := pr.createWarmSlot(ctx, pool, defaultKernelProfile, resolvedImage{RootfsPath: "/cache/x.ext4"}, "", nil); err != nil {
		t.Fatal(err)
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace("ns")); err != nil || len(pods.Items) != 1 {
		t.Fatalf("want one slot pod, got %d (err=%v)", len(pods.Items), err)
	}
	if got := pods.Items[0].Annotations[SlotReadinessAnnotation]; got != slotReadinessGuestAgent {
		t.Errorf("slot readiness = %q, want %q", got, slotReadinessGuestAgent)
	}
	var cms corev1.ConfigMapList
	if err := c.List(ctx, &cms, client.InNamespace("ns")); err != nil || len(cms.Items) != 1 {
		t.Fatalf("want one intent ConfigMap, got %d (err=%v)", len(cms.Items), err)
	}
	for _, v := range cms.Items[0].Data {
		if !strings.Contains(v, `"sandboxWarmSlot":true`) {
			t.Errorf("warm slot intent does not ask for the agent report: %s", v)
		}
	}
}

// A cold sandbox's launcher does not wait for the agent: its workload runs
// from the config disk, and a report would be one more pod write.
func TestBuildIntent_OnlyWarmSlotsReportTheAgent(t *testing.T) {
	sb := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}}
	if buildIntent(sb, defaultKernelProfile, "/r", "", execSpec{Argv: []string{"true"}}, false).SandboxWarmSlot {
		t.Error("cold sandbox intent marked as a warm slot")
	}
	if !buildIntent(sb, defaultKernelProfile, "/r", "", execSpec{}, true).SandboxWarmSlot {
		t.Error("warm slot intent not marked")
	}
}

// The defect: a checkout right after a pool turned Ready took a slot whose
// guest was still booting, and failed with ExecFailed. It must take only a
// slot whose agent answers; with none, it gets no slot and boots cold.
func TestTryClaimWarmSlot_SkipsSlotsStillBooting(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	booting := warmSlot(pool, "p-slot-boot", true, false)
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool, booting).
		WithStatusSubresource(&corev1.Pod{}).Build()
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "sb-uid"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "busybox:1", PoolRef: &corev1.LocalObjectReference{Name: "p"}},
	}
	if err := c.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}
	slot, err := r.tryClaimWarmSlot(ctx, sb, poolSlotProfile(pool), nil)
	if err != nil || slot != nil {
		t.Fatalf("claimed a slot whose guest agent has not answered: slot=%v err=%v", slot, err)
	}
	var got corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(booting), &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels[SlotStateLabelKey] != slotStateWarm {
		t.Fatalf("booting slot was changed: %v", got.Labels)
	}

	// swiftletd reports the agent: now it is taken.
	got.Annotations[GuestAgentReadyAnnotation] = "2026-10-11T00:00:00.000Z"
	if err := c.Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	slot, err = r.tryClaimWarmSlot(ctx, sb, poolSlotProfile(pool), nil)
	if err != nil || slot == nil || slot.Name != booting.Name {
		t.Fatalf("ready slot not claimed: slot=%v err=%v", slot, err)
	}
}

// Several checkouts at once, more than there are ready slots: each ready
// slot goes to one of them, a booting slot to none.
func TestTryClaimWarmSlot_ConcurrentCheckoutsTakeOnlyReadySlots(t *testing.T) {
	ctx := context.Background()
	pool := testPool(3)
	objs := []client.Object{pool,
		warmSlot(pool, "p-slot-r1", true, true),
		warmSlot(pool, "p-slot-r2", true, true),
		warmSlot(pool, "p-slot-b1", true, false),
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}
	claimed := map[string]string{}
	misses := 0
	for _, n := range []string{"a", "b", "c", "d"} {
		sb := &sandboxv1alpha1.SwiftSandbox{
			ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "ns", UID: types.UID("uid-" + n)},
			Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "busybox:1", PoolRef: &corev1.LocalObjectReference{Name: "p"}},
		}
		if err := c.Create(ctx, sb); err != nil {
			t.Fatal(err)
		}
		slot, err := r.tryClaimWarmSlot(ctx, sb, poolSlotProfile(pool), nil)
		if err != nil {
			t.Fatal(err)
		}
		if slot == nil {
			misses++
			continue
		}
		if prev, dup := claimed[slot.Name]; dup {
			t.Fatalf("slot %s claimed by %s and %s", slot.Name, prev, n)
		}
		claimed[slot.Name] = n
	}
	if len(claimed) != 2 || misses != 2 {
		t.Fatalf("want the 2 ready slots claimed and 2 misses, got %v and %d misses", claimed, misses)
	}
	if _, ok := claimed["p-slot-b1"]; ok {
		t.Fatal("booting slot was claimed")
	}
}

// The pool reports a booting slot as warming, not warm, and is Ready only
// when minWarm slots can take a checkout.
func TestPoolReconcile_CountsOnlyGuestReadySlotsAsWarm(t *testing.T) {
	ctx := context.Background()
	pool := testPool(2)
	ready := warmSlot(pool, "p-slot-r1", true, true)
	booting := warmSlot(pool, "p-slot-b1", true, false)
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool, ready, booting).
		WithStatusSubresource(pool).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(10)}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "p"}}
	status := func() sandboxv1alpha1.SwiftSandboxPoolStatus {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		var got sandboxv1alpha1.SwiftSandboxPool
		if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
			t.Fatal(err)
		}
		return got.Status
	}

	st := status()
	if st.WarmReplicas != 1 || st.WarmingReplicas != 1 || st.Phase != sandboxv1alpha1.SwiftSandboxPoolWarming {
		t.Fatalf("one ready, one booting: warm=%d warming=%d phase=%s", st.WarmReplicas, st.WarmingReplicas, st.Phase)
	}

	var b corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(booting), &b); err != nil {
		t.Fatal(err)
	}
	b.Annotations[GuestAgentReadyAnnotation] = "2026-10-11T00:00:01.000Z"
	if err := c.Update(ctx, &b); err != nil {
		t.Fatal(err)
	}
	st = status()
	if st.WarmReplicas != 2 || st.WarmingReplicas != 0 || st.Phase != sandboxv1alpha1.SwiftSandboxPoolReady {
		t.Fatalf("both ready: warm=%d warming=%d phase=%s", st.WarmReplicas, st.WarmingReplicas, st.Phase)
	}
}

// Upgrade: slots booted before this change carry no readiness stamp and
// their launchers never report the agent. They stay warm.
func TestPoolReconcile_LegacySlotsStayWarmAcrossUpgrade(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	legacy := warmSlot(pool, "p-slot-old", false, false)
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool, legacy).
		WithStatusSubresource(pool).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(10)}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "p"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var got sandboxv1alpha1.SwiftSandboxPool
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.WarmReplicas != 1 || got.Status.WarmingReplicas != 0 || got.Status.Phase != sandboxv1alpha1.SwiftSandboxPoolReady {
		t.Fatalf("legacy slot: warm=%d warming=%d phase=%s", got.Status.WarmReplicas, got.Status.WarmingReplicas, got.Status.Phase)
	}
}

// A slot's launcher keeps writing after the slot is ready (the guest IP lands
// up to a poll interval after DHCP). A claim that conflicts with such a write
// must retry the slot, not miss it and boot cold.
func TestTryClaimWarmSlot_RetriesASlotItsLauncherJustWrote(t *testing.T) {
	ctx := context.Background()
	pool := testPool(1)
	slot := warmSlot(pool, "p-slot-r1", true, true)
	var base client.Client
	launcherWrote := false
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool, slot).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if !launcherWrote {
					// The launcher's lease report lands between the read and the claim.
					launcherWrote = true
					var cur corev1.Pod
					if err := base.Get(ctx, client.ObjectKeyFromObject(obj), &cur); err != nil {
						return err
					}
					cur.Annotations["kubeswift.io/guest-ip"] = "192.168.99.10"
					if err := cl.Update(ctx, &cur); err != nil {
						return err
					}
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	base = c
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "sb-uid"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "busybox:1", PoolRef: &corev1.LocalObjectReference{Name: "p"}},
	}
	if err := c.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	r := &SwiftSandboxReconciler{Client: c, APIReader: c, Scheme: scheme.Scheme}
	got, err := r.tryClaimWarmSlot(ctx, sb, poolSlotProfile(pool), nil)
	if err != nil || got == nil {
		t.Fatalf("ready slot missed after a launcher write: slot=%v err=%v", got, err)
	}
	if got.Labels[SlotStateLabelKey] != slotStateClaimed || got.Annotations["kubeswift.io/guest-ip"] != "192.168.99.10" {
		t.Fatalf("claim lost the launcher's write or did not claim: labels=%v annotations=%v", got.Labels, got.Annotations)
	}
}
