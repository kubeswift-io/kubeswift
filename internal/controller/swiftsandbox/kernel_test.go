package swiftsandbox

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func swiftKernel(ns, name string, phase kernelv1alpha1.SwiftKernelPhase, nodes ...kernelv1alpha1.NodeKernelStatus) *kernelv1alpha1.SwiftKernel {
	return &kernelv1alpha1.SwiftKernel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     kernelv1alpha1.SwiftKernelStatus{Phase: phase, NodeStatuses: nodes},
	}
}

func readyKernel(ns, name string) *kernelv1alpha1.SwiftKernel {
	return swiftKernel(ns, name, kernelv1alpha1.SwiftKernelPhaseReady)
}

// testImage pushes a small image to an in-process registry and returns its
// reference, so a reconcile that resolves an image runs without the network.
func testImage(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(u.Host + "/test/sandbox:1")
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	return ref.String()
}

// kernelDirOf is the kernel directory a launcher pod mounts.
func kernelDirOf(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	for _, v := range pod.Spec.Volumes {
		if v.Name == "kernel-artifacts" && v.HostPath != nil {
			return v.HostPath.Path
		}
	}
	t.Fatalf("pod %s mounts no kernel-artifacts hostPath", pod.Name)
	return ""
}

func plainSandbox(image string) *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "default"},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image: image, Memory: resource.MustParse("512Mi"), Command: []string{"/bin/true"},
		},
	}
}

func sandboxReconciler(objs ...client.Object) (*SwiftSandboxReconciler, client.Client) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}, &gpuv1alpha1.SwiftGPUNode{}).Build()
	return &SwiftSandboxReconciler{Client: c, APIReader: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(20)}, c
}

func reconcileSB(t *testing.T, r *SwiftSandboxReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getSandbox(t *testing.T, c client.Client, name string) *sandboxv1alpha1.SwiftSandbox {
	t.Helper()
	var sb sandboxv1alpha1.SwiftSandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &sb); err != nil {
		t.Fatal(err)
	}
	return &sb
}

func assertNoLauncher(t *testing.T, c client.Client, name string) {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &pod); !apierrors.IsNotFound(err) {
		t.Errorf("a launcher pod was created without a bootable kernel (err=%v)", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name + intentConfigMapSuffix}, &cm); !apierrors.IsNotFound(err) {
		t.Errorf("an intent ConfigMap was created without a bootable kernel (err=%v)", err)
	}
}

// #660. A sandbox whose SwiftKernel is missing or not Ready got a launcher pod
// anyway: the kernel directory is mounted DirectoryOrCreate, so the pod started
// on an empty directory and the sandbox failed with the hypervisor's "Cannot
// open initramfs file". No pod now, and the sandbox says which kernel it waits
// for. The image is never resolved, so the unreachable ref is not touched.
func TestSandboxReconcile_WaitsForItsKernel(t *testing.T) {
	cases := []struct {
		name       string
		kernel     *kernelv1alpha1.SwiftKernel
		wantReason string
		wantMsg    string
	}{
		{"missing", nil, sandboxv1alpha1.SwiftSandboxReasonKernelNotFound,
			`no SwiftKernel named "sandbox" in namespace "default"`},
		{"pulling", swiftKernel("default", "sandbox", kernelv1alpha1.SwiftKernelPhasePulling),
			sandboxv1alpha1.SwiftSandboxReasonKernelNotReady, `SwiftKernel "sandbox" is not Ready (phase: Pulling)`},
		{"failed", swiftKernel("default", "sandbox", kernelv1alpha1.SwiftKernelPhaseFailed),
			sandboxv1alpha1.SwiftSandboxReasonKernelNotReady, `SwiftKernel "sandbox" is not Ready (phase: Failed)`},
		{"no status yet", swiftKernel("default", "sandbox", ""),
			sandboxv1alpha1.SwiftSandboxReasonKernelNotReady, `SwiftKernel "sandbox" is not Ready`},
		// A kernel of that name in another namespace is not this sandbox's.
		{"other namespace", readyKernel("elsewhere", "sandbox"), sandboxv1alpha1.SwiftSandboxReasonKernelNotFound,
			`no SwiftKernel named "sandbox" in namespace "default"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{plainSandbox("registry.invalid/never/pulled:1")}
			if tc.kernel != nil {
				objs = append(objs, tc.kernel)
			}
			r, c := sandboxReconciler(objs...)

			res := reconcileSB(t, r, "sb")
			if res.RequeueAfter != kernelRecheckInterval {
				t.Errorf("requeue = %v, want %v: the kernel may still appear or finish pulling", res.RequeueAfter, kernelRecheckInterval)
			}
			assertNoLauncher(t, c, "sb")
			sb := getSandbox(t, c, "sb")
			if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxPending {
				t.Errorf("phase = %q, want Pending (waiting is not a failure)", sb.Status.Phase)
			}
			cond := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tc.wantReason {
				t.Fatalf("Resolved = %+v, want False/%s", cond, tc.wantReason)
			}
			if !strings.Contains(cond.Message, tc.wantMsg) || !strings.Contains(sb.Status.Message, tc.wantMsg) {
				t.Errorf("condition message %q / status message %q should contain %q", cond.Message, sb.Status.Message, tc.wantMsg)
			}
		})
	}
}

// Once the kernel is Ready the same sandbox gets its launcher, booting that
// kernel, and Resolved turns True.
func TestSandboxReconcile_LaunchesOnceKernelIsReady(t *testing.T) {
	r, c := sandboxReconciler(plainSandbox(testImage(t)))
	reconcileSB(t, r, "sb")
	assertNoLauncher(t, c, "sb")

	if err := c.Create(context.Background(), readyKernel("default", "sandbox")); err != nil {
		t.Fatal(err)
	}
	reconcileSB(t, r, "sb")

	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatalf("no launcher pod once the kernel is Ready: %v", err)
	}
	if got, want := kernelDirOf(t, &pod), kernelv1alpha1.KernelLocalPath("default", "sandbox"); got != want {
		t.Errorf("kernel dir = %q, want %q", got, want)
	}
	sb := getSandbox(t, c, "sb")
	if cond := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Resolved = %+v, want True", cond)
	}
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxMaterializing {
		t.Errorf("phase = %q, want Materializing", sb.Status.Phase)
	}
}

// A native GPU sandbox is pinned to its GPU's node, so the kernel only has to
// be there: another node still pulling does not hold it, and its own node
// still pulling does.
func TestSandboxReconcile_PinnedSandboxNeedsTheKernelOnItsNode(t *testing.T) {
	pulling := func(worker1 kernelv1alpha1.SwiftKernelPhase) *kernelv1alpha1.SwiftKernel {
		return swiftKernel("default", gpuSandboxKernelProfile, kernelv1alpha1.SwiftKernelPhasePulling,
			kernelv1alpha1.NodeKernelStatus{NodeName: "worker-1", Phase: worker1},
			kernelv1alpha1.NodeKernelStatus{NodeName: "worker-2", Phase: kernelv1alpha1.SwiftKernelPhasePulling})
	}
	newGPUSandbox := func(image string) *sandboxv1alpha1.SwiftSandbox {
		sb := nativeGPUSandbox("gpu-sb", "default", "gtx")
		sb.Spec.Image = image
		sb.Spec.Command = []string{"/bin/true"}
		return sb
	}

	t.Run("not ready on its node", func(t *testing.T) {
		r, c := sandboxReconciler(newGPUSandbox("registry.invalid/never/pulled:1"),
			oneGPUNode("worker-1"), workerNode("worker-1"), pcieProfile("gtx", "default"),
			pulling(kernelv1alpha1.SwiftKernelPhasePulling))
		reconcileSB(t, r, "gpu-sb")
		assertNoLauncher(t, c, "gpu-sb")
		cond := apimeta.FindStatusCondition(getSandbox(t, c, "gpu-sb").Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
		if cond == nil || cond.Reason != sandboxv1alpha1.SwiftSandboxReasonKernelNotReady ||
			!strings.Contains(cond.Message, "for node worker-1") {
			t.Errorf("Resolved = %+v, want KernelNotReady naming worker-1", cond)
		}
	})

	t.Run("ready on its node", func(t *testing.T) {
		r, c := sandboxReconciler(newGPUSandbox(testImage(t)),
			oneGPUNode("worker-1"), workerNode("worker-1"), pcieProfile("gtx", "default"),
			pulling(kernelv1alpha1.SwiftKernelPhaseReady))
		reconcileSB(t, r, "gpu-sb")
		var pod corev1.Pod
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gpu-sb"}, &pod); err != nil {
			t.Fatalf("the kernel is Ready on worker-1, where the sandbox is pinned, but no pod: %v", err)
		}
		if pod.Spec.NodeSelector[corev1.LabelHostname] != "worker-1" {
			t.Errorf("pod not pinned to worker-1: %v", pod.Spec.NodeSelector)
		}
		if got, want := kernelDirOf(t, &pod), kernelv1alpha1.KernelLocalPath("default", gpuSandboxKernelProfile); got != want {
			t.Errorf("kernel dir = %q, want %q", got, want)
		}
	})
}

// #659. The pool decided its kernel from kernelProfileRef alone, so a GPU pool
// warmed its slots on the base sandbox kernel, which has no CONFIG_MODULES and
// cannot load the NVIDIA driver. It now takes the rule a standalone sandbox
// uses.
func TestPoolKernelProfile_MatchesTheSandboxRule(t *testing.T) {
	gpu := &corev1.LocalObjectReference{Name: "gtx"}
	custom := &corev1.LocalObjectReference{Name: "custom"}
	cases := []struct {
		name          string
		gpu, override *corev1.LocalObjectReference
		want          string
	}{
		{"plain pool", nil, nil, defaultKernelProfile},
		{"GPU pool", gpu, nil, gpuSandboxKernelProfile},
		{"GPU pool with an explicit kernel", gpu, custom, "custom"},
		{"plain pool with an explicit kernel", nil, custom, "custom"},
	}
	r := &SwiftSandboxPoolReconciler{}
	for _, tc := range cases {
		pool := &sandboxv1alpha1.SwiftSandboxPool{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
			Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "cuda:12", GPUProfileRef: tc.gpu, KernelProfileRef: tc.override},
		}
		if got := resolveKernelProfile(r.slotTemplate(pool, "p-slot-x")); got != tc.want {
			t.Errorf("%s: kernel = %q, want %q", tc.name, got, tc.want)
		}
		// The same answer a sandbox of that shape gets.
		sb := &sandboxv1alpha1.SwiftSandbox{Spec: sandboxv1alpha1.SwiftSandboxSpec{GPUProfileRef: tc.gpu, KernelProfileRef: tc.override}}
		if got := resolveKernelProfile(sb); got != tc.want {
			t.Errorf("%s: sandbox kernel = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func poolReconciler(objs ...client.Object) (*SwiftSandboxPoolReconciler, client.Client) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandboxPool{}, &gpuv1alpha1.SwiftGPUNode{}).Build()
	return &SwiftSandboxPoolReconciler{Client: c, APIReader: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(20)}, c
}

func reconcilePool(t *testing.T, r *SwiftSandboxPoolReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func poolSlotPods(t *testing.T, c client.Client, pool string) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace("default"), client.MatchingLabels{PoolLabelKey: pool}); err != nil {
		t.Fatal(err)
	}
	return pods.Items
}

// End to end through the pool reconcile: the warm slot a GPU pool creates
// mounts the gpu-sandbox kernel, and an explicit kernelProfileRef still wins.
func TestPoolReconcile_GPUSlotBootsTheGPUSandboxKernel(t *testing.T) {
	cases := []struct {
		name     string
		override *corev1.LocalObjectReference
		want     string
	}{
		{"default", nil, gpuSandboxKernelProfile},
		{"explicit kernelProfileRef", &corev1.LocalObjectReference{Name: "custom"}, "custom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := gpuPool("gp", "default", "gtx")
			pool.Spec.Image = testImage(t)
			pool.Spec.KernelProfileRef = tc.override
			r, c := poolReconciler(pool, oneGPUNode("worker-1"), workerNode("worker-1"), pcieProfile("gtx", "default"),
				readyKernel("default", defaultKernelProfile), readyKernel("default", gpuSandboxKernelProfile),
				readyKernel("default", "custom"))
			reconcilePool(t, r, "gp")

			pods := poolSlotPods(t, c, "gp")
			if len(pods) != 1 {
				t.Fatalf("want one warm slot, got %d", len(pods))
			}
			if got, want := kernelDirOf(t, &pods[0]), kernelv1alpha1.KernelLocalPath("default", tc.want); got != want {
				t.Errorf("slot kernel dir = %q, want %q", got, want)
			}
		})
	}
}

// #660 on the pool path, which builds slots with the same buildPod. A slot
// without a bootable kernel failed at boot, and the census replaced it with
// another that failed the same way. The pool now warms nothing, allocates no
// GPU, and says which kernel it waits for; repeating the reconcile changes
// nothing.
func TestPoolReconcile_HoldsWarmingUntilTheKernelIsReady(t *testing.T) {
	cases := []struct {
		name       string
		pool       *sandboxv1alpha1.SwiftSandboxPool
		objs       []client.Object
		wantReason string
		wantMsg    string
	}{
		{
			// Only the base kernel exists: the GPU pool needs gpu-sandbox.
			name:       "GPU pool, gpu-sandbox missing",
			pool:       gpuPool("gp", "default", "gtx"),
			objs:       []client.Object{readyKernel("default", defaultKernelProfile)},
			wantReason: sandboxv1alpha1.SwiftSandboxReasonKernelNotFound,
			wantMsg:    `no SwiftKernel named "gpu-sandbox" in namespace "default"`,
		},
		{
			name: "plain pool, kernel pulling",
			pool: &sandboxv1alpha1.SwiftSandboxPool{
				ObjectMeta: metav1.ObjectMeta{Name: "gp", Namespace: "default"},
				Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "busybox:1", MinWarm: 2},
			},
			objs:       []client.Object{swiftKernel("default", defaultKernelProfile, kernelv1alpha1.SwiftKernelPhasePulling)},
			wantReason: sandboxv1alpha1.SwiftSandboxReasonKernelNotReady,
			wantMsg:    `SwiftKernel "sandbox" is not Ready (phase: Pulling)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{tc.pool, oneGPUNode("worker-1"), workerNode("worker-1"), pcieProfile("gtx", "default")}, tc.objs...)
			r, c := poolReconciler(objs...)
			for i := 0; i < 2; i++ {
				if res := reconcilePool(t, r, "gp"); res.RequeueAfter != kernelRecheckInterval {
					t.Errorf("reconcile %d: requeue = %v, want %v", i+1, res.RequeueAfter, kernelRecheckInterval)
				}
			}
			if pods := poolSlotPods(t, c, "gp"); len(pods) != 0 {
				t.Errorf("warmed %d slot(s) without a bootable kernel", len(pods))
			}
			if got := nodeAllocatedTo(t, c); got != "" {
				t.Errorf("a GPU was allocated for a slot that cannot boot (allocatedTo=%q)", got)
			}
			var p sandboxv1alpha1.SwiftSandboxPool
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gp"}, &p); err != nil {
				t.Fatal(err)
			}
			if p.Status.Phase != sandboxv1alpha1.SwiftSandboxPoolDegraded {
				t.Errorf("phase = %q, want Degraded", p.Status.Phase)
			}
			cond := apimeta.FindStatusCondition(p.Status.Conditions, sandboxv1alpha1.SwiftSandboxPoolConditionResolved)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tc.wantReason {
				t.Fatalf("Resolved = %+v, want False/%s", cond, tc.wantReason)
			}
			if !strings.Contains(cond.Message, tc.wantMsg) {
				t.Errorf("message %q should contain %q", cond.Message, tc.wantMsg)
			}
		})
	}
}
