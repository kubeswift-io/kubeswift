package swiftsandbox

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func readySlot(name, profile string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns", UID: types.UID("uid-" + name),
			Labels:      map[string]string{PoolLabelKey: "p", SlotStateLabelKey: slotStateWarm},
			Annotations: map[string]string{SlotProfileAnnotation: profile},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: launcherName, Ready: true}},
		},
	}
}

// shapedPool is a pool with every slot-shape field set.
func shapedPool() *sandboxv1alpha1.SwiftSandboxPool {
	return &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: sandboxv1alpha1.SwiftSandboxPoolSpec{
			Image:              "busybox:1",
			VerifyKeySecretRef: &sandboxv1alpha1.SecretObjectReference{Name: "cosign-pub"},
			RootfsMode:         sandboxv1alpha1.SandboxRootfsBlock,
			CPU:                1,
			Memory:             resource.MustParse("512Mi"),
			Network:            sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkRestricted},
			NodeSelector:       map[string]string{"zone": "a", "tier": "trusted"},
		},
	}
}

// matchingSandbox asks for exactly what shapedPool's slots have.
func matchingSandbox() *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "sb-uid"},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image:              "busybox:1",
			PoolRef:            &corev1.LocalObjectReference{Name: "p"},
			VerifyKeySecretRef: &sandboxv1alpha1.SecretObjectReference{Name: "cosign-pub"},
			CPU:                1,
			Memory:             resource.MustParse("512Mi"),
			NodeSelector:       map[string]string{"zone": "a", "tier": "trusted"},
			Command:            []string{"/bin/true"},
		},
	}
}

// Checkout claims only a slot booted under the pool's current shape: one from
// before a pool edit (here: open network, then a smaller memory) is skipped.
func TestTryClaimWarmSlot_OnlyASlotOfThePoolsCurrentShape(t *testing.T) {
	pool := shapedPool()
	wasOpen := shapedPool()
	wasOpen.Spec.Network.Mode = sandboxv1alpha1.SandboxNetworkOpen
	wasSmaller := shapedPool()
	wasSmaller.Spec.Memory = resource.MustParse("256Mi")
	sb := matchingSandbox()
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(sb, readySlot("p-slot-open1", poolSlotProfile(wasOpen)), readySlot("p-slot-small", poolSlotProfile(wasSmaller))).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}

	slot, err := r.tryClaimWarmSlot(context.Background(), sb, poolSlotProfile(pool))
	if err != nil {
		t.Fatal(err)
	}
	if slot != nil {
		t.Fatalf("claimed %s, booted under an earlier pool spec", slot.Name)
	}

	if err := c.Create(context.Background(), readySlot("p-slot-match", poolSlotProfile(pool))); err != nil {
		t.Fatal(err)
	}
	slot, err = r.tryClaimWarmSlot(context.Background(), sb, poolSlotProfile(pool))
	if err != nil || slot == nil || slot.Name != "p-slot-match" {
		t.Fatalf("want the current-shape slot claimed, got slot=%v err=%v", slot, err)
	}
}

// Every field a checkout cannot change is compared, and each difference is
// named with both values so the PoolColdFallback Event says what to fix.
func TestSlotMismatches_NamesEveryDifference(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*sandboxv1alpha1.SwiftSandbox)
		want string
	}{
		{"image", func(s *sandboxv1alpha1.SwiftSandbox) { s.Spec.Image = "busybox:2" }, "image (pool busybox:1, sandbox busybox:2)"},
		{"network", func(s *sandboxv1alpha1.SwiftSandbox) { s.Spec.Network.Mode = sandboxv1alpha1.SandboxNetworkOpen },
			"network mode (pool restricted, sandbox open)"},
		{"verify key", func(s *sandboxv1alpha1.SwiftSandbox) { s.Spec.VerifyKeySecretRef = nil },
			"verifyKeySecretRef (pool cosign-pub, sandbox none)"},
		{"rootfs", func(s *sandboxv1alpha1.SwiftSandbox) { s.Spec.RootfsMode = sandboxv1alpha1.SandboxRootfsVirtiofs },
			"rootfsMode (pool block, sandbox virtiofs)"},
		{"kernel", func(s *sandboxv1alpha1.SwiftSandbox) {
			s.Spec.KernelProfileRef = &corev1.LocalObjectReference{Name: "custom"}
		}, "kernel (pool sandbox, sandbox custom)"},
		{"cpu", func(s *sandboxv1alpha1.SwiftSandbox) { s.Spec.CPU = 2 }, "cpu (pool 1, sandbox 2)"},
		{"memory", func(s *sandboxv1alpha1.SwiftSandbox) { s.Spec.Memory = resource.MustParse("1Gi") },
			"memory (pool 512Mi, sandbox 1Gi)"},
		{"node selector", func(s *sandboxv1alpha1.SwiftSandbox) { s.Spec.NodeSelector = map[string]string{"zone": "b"} },
			"nodeSelector (the pool does not require zone=b)"},
		{"own GPU", func(s *sandboxv1alpha1.SwiftSandbox) {
			s.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gtx"}
		}, "gpu (a sandbox with its own GPU boots cold)"},
		{"own model", func(s *sandboxv1alpha1.SwiftSandbox) {
			s.Spec.Model = &sandboxv1alpha1.SandboxModel{ImageRef: "reg/llm:1"}
		}, "model (pool none, sandbox reg/llm:1 at /model)"},
		{"scratch disk", func(s *sandboxv1alpha1.SwiftSandbox) {
			s.Spec.ScratchDisk = &sandboxv1alpha1.SandboxScratchDisk{PVCRef: &corev1.LocalObjectReference{Name: "d"}}
		}, "scratchDisk (warm slots have none)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := matchingSandbox()
			tc.edit(sb)
			got := slotMismatches(shapedPool(), sb)
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("mismatches = %q, want exactly %q", got, tc.want)
			}
		})
	}
	if got := slotMismatches(shapedPool(), matchingSandbox()); len(got) != 0 {
		t.Errorf("an identical shape must match, got %q", got)
	}
}

// What the pool gives (its GPU, the GPU kernel that goes with it, its model)
// and what the sandbox leaves unconstrained must not send it cold; neither
// must two spellings of one value.
func TestSlotMismatches_InheritedAndEquivalentShapesMatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		editPool func(*sandboxv1alpha1.SwiftSandboxPool)
		editSB   func(*sandboxv1alpha1.SwiftSandbox)
	}{
		{"GPU pool: the sandbox inherits the GPU and its kernel", func(p *sandboxv1alpha1.SwiftSandboxPool) {
			p.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gtx"}
		}, func(*sandboxv1alpha1.SwiftSandbox) {}},
		{"model pool: the sandbox inherits the model", func(p *sandboxv1alpha1.SwiftSandboxPool) {
			p.Spec.Model = &sandboxv1alpha1.SandboxModel{ImageRef: "reg/llm:1"}
		}, func(*sandboxv1alpha1.SwiftSandbox) {}},
		{"model pool: the sandbox names the same model", func(p *sandboxv1alpha1.SwiftSandboxPool) {
			p.Spec.Model = &sandboxv1alpha1.SandboxModel{ImageRef: "reg/llm:1"}
		}, func(s *sandboxv1alpha1.SwiftSandbox) {
			s.Spec.Model = &sandboxv1alpha1.SandboxModel{ImageRef: "reg/llm:1", MountPath: "/model"}
		}},
		{"a narrower sandbox selector the pool implies", func(*sandboxv1alpha1.SwiftSandboxPool) {},
			func(s *sandboxv1alpha1.SwiftSandbox) { s.Spec.NodeSelector = map[string]string{"zone": "a"} }},
		{"no sandbox selector", func(*sandboxv1alpha1.SwiftSandboxPool) {},
			func(s *sandboxv1alpha1.SwiftSandbox) { s.Spec.NodeSelector = nil }},
		{"the default kernel, named", func(*sandboxv1alpha1.SwiftSandboxPool) {},
			func(s *sandboxv1alpha1.SwiftSandbox) {
				s.Spec.KernelProfileRef = &corev1.LocalObjectReference{Name: defaultKernelProfile}
			}},
		{"unset network and rootfs are the CRD defaults", func(*sandboxv1alpha1.SwiftSandboxPool) {},
			func(s *sandboxv1alpha1.SwiftSandbox) { s.Spec.Network.Mode, s.Spec.RootfsMode = "", "" }},
		{"0.5Gi is 512Mi", func(*sandboxv1alpha1.SwiftSandboxPool) {},
			func(s *sandboxv1alpha1.SwiftSandbox) { s.Spec.Memory = resource.MustParse("0.5Gi") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, sb := shapedPool(), matchingSandbox()
			tc.editPool(pool)
			tc.editSB(sb)
			if got := slotMismatches(pool, sb); len(got) != 0 {
				t.Errorf("want a match, got %q", got)
			}
		})
	}
}

// Through the reconciler: a sandbox asking for more than the pool's slots
// boots cold, names every difference in the Event, and leaves the warm slot
// for a sandbox it fits. One that matches is checked out warm.
func TestReconcilePooled_ShapeMismatchBootsColdAndSaysWhy(t *testing.T) {
	ctx := context.Background()
	pool := shapedPool()
	pool.Namespace = "default"
	slot := readySlot("p-slot-aaaaa", poolSlotProfile(pool))
	slot.Namespace = "default"

	big := matchingSandbox()
	big.Namespace, big.Name, big.UID = "default", "big", "big-uid"
	big.Spec.CPU, big.Spec.Memory = 2, resource.MustParse("1Gi")
	r, c := sandboxReconciler(pool, slot, big)
	rec := r.Recorder.(*record.FakeRecorder)
	reconcileSB(t, r, "big")

	var event string
	for len(rec.Events) > 0 {
		if e := <-rec.Events; strings.Contains(e, "PoolColdFallback") {
			event = e
		}
	}
	for _, want := range []string{"cpu (pool 1, sandbox 2)", "memory (pool 512Mi, sandbox 1Gi)"} {
		if !strings.Contains(event, want) {
			t.Errorf("PoolColdFallback event %q does not name %q", event, want)
		}
	}
	if got := getSandbox(t, c, "big"); got.Status.PodRef != "big" {
		t.Errorf("status.podRef = %q, want the cold pod %q", got.Status.PodRef, "big")
	}
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(slot), &p); err != nil || p.Labels[SlotStateLabelKey] != slotStateWarm {
		t.Fatalf("the warm slot must stay warm for a sandbox it fits (err=%v, labels=%v)", err, p.Labels)
	}

	fits := matchingSandbox()
	fits.Namespace, fits.Name, fits.UID = "default", "fits", "fits-uid"
	if err := c.Create(ctx, fits); err != nil {
		t.Fatal(err)
	}
	reconcileSB(t, r, "fits")
	if got := getSandbox(t, c, "fits"); got.Status.PodRef != slot.Name {
		t.Errorf("a sandbox of the pool's shape should check out %s, got podRef %q", slot.Name, got.Status.PodRef)
	}
}

// A pool edit used to leave already-warm slots running the old settings until
// claimed. They are recycled now, for every shape field, not just the three
// checkout used to compare.
func TestPoolReconcile_RecyclesSlotsBootedUnderAnEarlierSpec(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "busybox:1", MinWarm: 1, CPU: 2},
		Status: sandboxv1alpha1.SwiftSandboxPoolStatus{
			Rootfs: &sandboxv1alpha1.SandboxRootfsStatus{Digest: "sha256:deadbeef"},
		},
	}
	wasOneCPU := pool.DeepCopy()
	wasOneCPU.Spec.CPU = 1
	stale := readySlot("p-slot-stale", poolSlotProfile(wasOneCPU))
	legacy := readySlot("p-slot-lgacy", "image=busybox:1;network=restricted;verifyKey=") // the old format
	current := readySlot("p-slot-curnt", poolSlotProfile(pool))
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool, stale, legacy, current).
		WithStatusSubresource(pool).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "p"}}); err != nil {
		t.Fatal(err)
	}
	var p corev1.Pod
	for _, gone := range []*corev1.Pod{stale, legacy} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(gone), &p); !apierrors.IsNotFound(err) {
			t.Errorf("slot %s, booted under an earlier spec, should be recycled, err=%v", gone.Name, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(current), &p); err != nil {
		t.Errorf("a slot matching the current spec must be kept: %v", err)
	}
}
