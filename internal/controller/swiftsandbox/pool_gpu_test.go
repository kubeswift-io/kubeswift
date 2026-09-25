package swiftsandbox

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func gpuPool(name, ns, profileName string) *sandboxv1alpha1.SwiftSandboxPool {
	return &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: sandboxv1alpha1.SwiftSandboxPoolSpec{
			Image: "cuda:12", MinWarm: 1,
			GPUProfileRef: &corev1.LocalObjectReference{Name: profileName},
		},
	}
}

func TestAllocateSlotGPU_StampsSlotAndConsumesNode(t *testing.T) {
	node := oneGPUNode("worker-1")
	profile := pcieProfile("gtx", "default")
	pool := gpuPool("gp", "default", "gtx")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, workerNode(node.Name), profile).WithStatusSubresource(node).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}

	slot := r.slotTemplate(pool, "gp-slot-aaaaa")
	if err := r.allocateSlotGPU(context.Background(), pool, slot); err != nil {
		t.Fatalf("allocateSlotGPU: %v", err)
	}
	if slot.Spec.GPUProfileRef == nil || slot.Status.GPU == nil ||
		len(slot.Status.GPU.Devices) != 1 || slot.Status.GPU.Devices[0] != "0000:01:00.0" ||
		slot.Status.GPU.NodeName != "worker-1" {
		t.Fatalf("slot not stamped for GPU: spec.gpuProfileRef=%v status.gpu=%+v", slot.Spec.GPUProfileRef, slot.Status.GPU)
	}
	var after gpuv1alpha1.SwiftGPUNode
	_ = c.Get(context.Background(), client.ObjectKey{Name: "worker-1"}, &after)
	if after.Status.FreeGPUs != 0 || after.Status.GPUs[0].AllocatedTo != "sandbox:default/gp-slot-aaaaa" {
		t.Errorf("node not allocated to the slot: free=%d allocatedTo=%q", after.Status.FreeGPUs, after.Status.GPUs[0].AllocatedTo)
	}
}

func TestAllocateSlotGPU_RejectsHGXTier(t *testing.T) {
	node := oneGPUNode("worker-1")
	profile := pcieProfile("hgx", "default")
	profile.Spec.Tier = "hgx-shared"
	pool := gpuPool("gp", "default", "hgx")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, workerNode(node.Name), profile).WithStatusSubresource(node).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}

	err := r.allocateSlotGPU(context.Background(), pool, r.slotTemplate(pool, "gp-slot-x"))
	if err == nil {
		t.Fatal("hgx tier must be rejected for a warm GPU pool")
	}
}

// poolPod is a slot pod of pool gp (warm unless state says otherwise).
func poolPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "default",
		Labels: map[string]string{PoolLabelKey: "gp", SlotStateLabelKey: slotStateWarm},
	}}
}

func allocatedNode(allocatedTo string) *gpuv1alpha1.SwiftGPUNode {
	node := oneGPUNode("worker-1")
	node.Status.FreeGPUs = 0
	node.Status.GPUs[0].Allocated = true
	node.Status.GPUs[0].AllocatedTo = allocatedTo
	return node
}

func nodeAllocatedTo(t *testing.T, c client.Client) string {
	t.Helper()
	var n gpuv1alpha1.SwiftGPUNode
	if err := c.Get(context.Background(), client.ObjectKey{Name: "worker-1"}, &n); err != nil {
		t.Fatal(err)
	}
	return n.Status.GPUs[0].AllocatedTo
}

// The GC sweep releases the GPU of a slot whose pod is gone, and keeps the GPU
// of a slot whose pod still exists.
func TestReconcileSlotGPUGC_ReleasesOrphanedSlots(t *testing.T) {
	pool := gpuPool("gp", "default", "gtx")

	// No live pod → the dead slot's GPU is released.
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(allocatedNode("sandbox:default/gp-slot-dead0")).WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
	if held, err := r.reconcileSlotGPUGC(context.Background(), pool); err != nil || held {
		t.Fatalf("held=%v err=%v", held, err)
	}
	if got := nodeAllocatedTo(t, c); got != "" {
		t.Fatalf("orphaned slot GPU not released: allocatedTo=%q", got)
	}

	// A live pod → the GPU is KEPT, and reported held.
	c = fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(allocatedNode("sandbox:default/gp-slot-live0"), poolPod("gp-slot-live0")).
		WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).Build()
	r = &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
	held, err := r.reconcileSlotGPUGC(context.Background(), pool)
	if err != nil || !held {
		t.Fatalf("a live slot must be reported held: held=%v err=%v", held, err)
	}
	if got := nodeAllocatedTo(t, c); got != "sandbox:default/gp-slot-live0" {
		t.Errorf("a live slot's GPU must NOT be released, got allocatedTo=%q", got)
	}
}

// Allocations that merely share the "<pool>-slot-" prefix belong to someone
// else and must never be freed: a standalone sandbox named like a slot, or the
// slots of a pool whose own name starts with "<pool>-slot-".
func TestReconcileSlotGPUGC_IgnoresOtherWorkloadsSharingThePrefix(t *testing.T) {
	pool := gpuPool("gp", "default", "gtx")
	for _, owner := range []string{
		"sandbox:default/gp-slot-x",              // standalone sandbox "gp-slot-x"
		"sandbox:default/gp-slot-b-slot-abcde",   // a slot of pool "gp-slot-b"
		"sandbox:default/gp-slot-toolongsuffix0", // not a 5-char slot suffix
	} {
		c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(allocatedNode(owner)).WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).Build()
		r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
		if _, err := r.reconcileSlotGPUGC(context.Background(), pool); err != nil {
			t.Fatal(err)
		}
		if got := nodeAllocatedTo(t, c); got != owner {
			t.Errorf("GPU of %q was freed by pool gp's GC", owner)
		}
	}

	// Even an exactly slot-shaped name is left alone while a standalone
	// SwiftSandbox by that name exists — its own controller releases it.
	sb := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "gp-slot-abcde", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(allocatedNode("sandbox:default/gp-slot-abcde"), sb).WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
	if _, err := r.reconcileSlotGPUGC(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if got := nodeAllocatedTo(t, c); got != "sandbox:default/gp-slot-abcde" {
		t.Error("the GPU of a standalone sandbox with a slot-shaped name was freed")
	}
}

// Deleting a GPU pool must not free the GPU of a claimed slot whose checkout is
// still running: the finalizer stays (requeue) while that pod exists, idle warm
// slots are deleted outright, and the GPU is released only once the claimed pod
// is gone. Deletion used to pass an EMPTY live set and free every slot's GPU,
// handing a device that was in use to the next consumer.
func TestPoolDeletion_WaitsForClaimedSlotBeforeReleasingItsGPU(t *testing.T) {
	ctx := context.Background()
	pool := gpuPool("gp", "default", "gtx")
	pool.Finalizers = []string{poolGPUFinalizer}
	claimed := poolPod("gp-slot-busy0")
	claimed.Labels[SlotStateLabelKey] = slotStateClaimed
	warm := poolPod("gp-slot-idle0")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(pool, claimed, warm, allocatedNode("sandbox:default/gp-slot-busy0")).
		WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
	if err := c.Delete(ctx, pool); err != nil { // sets deletionTimestamp (finalizer holds it)
		t.Fatal(err)
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}

	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Error("deletion should requeue while a claimed slot still holds its GPU")
	}
	if got := nodeAllocatedTo(t, c); got != "sandbox:default/gp-slot-busy0" {
		t.Fatalf("a running checkout's GPU was released during pool deletion (allocatedTo=%q)", got)
	}
	var p sandboxv1alpha1.SwiftSandboxPool
	if err := c.Get(ctx, req.NamespacedName, &p); err != nil {
		t.Fatalf("pool should still exist (finalizer held): %v", err)
	}
	var w corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(warm), &w); !apierrors.IsNotFound(err) {
		t.Errorf("idle warm slot should be deleted during pool deletion, got err=%v", err)
	}

	// The checkout ends: its pod goes away. Now the GPU is released and the
	// finalizer removed.
	if err := c.Delete(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got := nodeAllocatedTo(t, c); got != "" {
		t.Errorf("GPU should be released once the claimed pod is gone, allocatedTo=%q", got)
	}
	if err := c.Get(ctx, req.NamespacedName, &p); !apierrors.IsNotFound(err) {
		t.Errorf("pool should be gone once nothing is held, got err=%v", err)
	}
}

// A slot pod that has ended holds no GPU: the kubelet reports Succeeded or
// Failed only once the pod's containers have stopped. Such a pod used to count
// as live, and a failed slot kept its GPU for as long as the pod stayed.
func TestReconcileSlotGPUGC_ReleasesTheGPUOfAnEndedSlot(t *testing.T) {
	pool := gpuPool("gp", "default", "gtx")
	for _, tc := range []struct {
		phase    corev1.PodPhase
		released bool
	}{
		{corev1.PodFailed, true},
		{corev1.PodSucceeded, true},
		{corev1.PodRunning, false},
		{corev1.PodPending, false},
	} {
		slot := poolPod("gp-slot-aaaaa")
		slot.Status.Phase = tc.phase
		c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(allocatedNode("sandbox:default/gp-slot-aaaaa"), slot).
			WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).Build()
		r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
		held, err := r.reconcileSlotGPUGC(context.Background(), pool)
		if err != nil {
			t.Fatal(err)
		}
		if got := nodeAllocatedTo(t, c); (got == "") != tc.released {
			t.Errorf("slot %s: allocatedTo=%q, want released=%v", tc.phase, got, tc.released)
		}
		if held == tc.released {
			t.Errorf("slot %s: held=%v, want %v", tc.phase, held, !tc.released)
		}
	}
}

// slotPodIn is slot gp-slot-dfjtk of pool gp in the given state: Running with
// its launcher ready, or Failed the way the lab's slot failed.
func slotPodIn(phase corev1.PodPhase, state string) *corev1.Pod {
	p := poolPod("gp-slot-dfjtk")
	p.Labels[SlotStateLabelKey] = state
	p.Status.Phase = phase
	launcher := corev1.ContainerStatus{Name: launcherName, Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	if phase == corev1.PodFailed {
		launcher = corev1.ContainerStatus{Name: launcherName, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "Cannot open initramfs file"},
		}}
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{launcher}
	return p
}

// Lab, round 4: a GPU pool's only GPU stayed allocated to a slot that had
// failed the day before, and the pool, Degraded on an image it could not
// resolve, never got it back. A failed warm slot is now deleted and its GPU
// released on every pass, whether or not the image resolves, and a
// replacement takes the GPU once it does. A running slot keeps its GPU. A
// failed claimed slot belongs to its sandbox, which deletes it; its GPU is
// released all the same.
func TestPoolReconcile_FailedSlotReleasesItsGPU(t *testing.T) {
	for _, tc := range []struct {
		name       string
		slot       *corev1.Pod
		resolvable bool
		keepSlot   bool   // the slot pod is still there
		wantGPU    string // "" free, "slot" still the slot's, "new" a new slot's
		wantPhase  sandboxv1alpha1.SwiftSandboxPoolPhase
		wantEvent  bool // a SlotEnded event names the failure
	}{
		{"failed warm slot, image does not resolve", slotPodIn(corev1.PodFailed, slotStateWarm), false,
			false, "", sandboxv1alpha1.SwiftSandboxPoolDegraded, true},
		{"failed warm slot, image resolves", slotPodIn(corev1.PodFailed, slotStateWarm), true,
			false, "new", sandboxv1alpha1.SwiftSandboxPoolWarming, true},
		{"running warm slot, image does not resolve", slotPodIn(corev1.PodRunning, slotStateWarm), false,
			true, "slot", sandboxv1alpha1.SwiftSandboxPoolDegraded, false},
		{"failed claimed slot, image does not resolve", slotPodIn(corev1.PodFailed, slotStateClaimed), false,
			true, "", sandboxv1alpha1.SwiftSandboxPoolDegraded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := gpuPool("gp", "default", "gtx")
			pool.Spec.Image = "!!! not a ref !!!" // fails in resolveImage where a 429 does
			if tc.resolvable {
				pool.Spec.Image = testImage(t)
			}
			tc.slot.Annotations = map[string]string{SlotProfileAnnotation: poolSlotProfile(pool)}
			r, c := poolReconciler(pool, tc.slot, allocatedNode("sandbox:default/gp-slot-dfjtk"),
				workerNode("worker-1"), pcieProfile("gtx", "default"), readyKernel("default", gpuSandboxKernelProfile))
			reconcilePool(t, r, "gp")

			var p corev1.Pod
			err := c.Get(ctx, client.ObjectKeyFromObject(tc.slot), &p)
			if tc.keepSlot && err != nil {
				t.Errorf("slot pod should be kept: %v", err)
			}
			if !tc.keepSlot && !apierrors.IsNotFound(err) {
				t.Errorf("ended warm slot should be deleted, got err=%v", err)
			}

			got := nodeAllocatedTo(t, c)
			switch tc.wantGPU {
			case "":
				if got != "" {
					t.Errorf("GPU should be released, allocatedTo=%q", got)
				}
			case "slot":
				if got != "sandbox:default/gp-slot-dfjtk" {
					t.Errorf("a running slot's GPU must be kept, allocatedTo=%q", got)
				}
			case "new":
				name, _ := strings.CutPrefix(got, "sandbox:default/")
				if name == "gp-slot-dfjtk" || !isPoolSlotName(pool, name) {
					t.Errorf("GPU should go to a replacement slot, allocatedTo=%q", got)
				}
				if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &p); err != nil {
					t.Errorf("replacement slot %q not created: %v", name, err)
				}
			}

			var after sandboxv1alpha1.SwiftSandboxPool
			if err := c.Get(ctx, client.ObjectKeyFromObject(pool), &after); err != nil {
				t.Fatal(err)
			}
			if after.Status.Phase != tc.wantPhase {
				t.Errorf("pool phase = %q, want %q (%s)", after.Status.Phase, tc.wantPhase, after.Status.Message)
			}

			rec := r.Recorder.(*record.FakeRecorder)
			var events []string
			for len(rec.Events) > 0 {
				events = append(events, <-rec.Events)
			}
			all := strings.Join(events, "\n")
			if got := strings.Contains(all, "SlotEnded") && strings.Contains(all, "Cannot open initramfs file"); got != tc.wantEvent {
				t.Errorf("SlotEnded event naming the failure = %v, want %v (events: %v)", got, tc.wantEvent, events)
			}
		})
	}
}
