package swiftsandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// testHold stands in for the kubelet: a pod with it stays (terminating) after
// a delete until the test removes it, as a real pod stays until its
// containers have stopped.
const testHold = "test.kubeswift.io/kubelet"

// deletedSandbox is a SwiftSandbox that has been deleted and still carries
// the termination finalizer.
func deletedSandbox(drain *time.Duration) *sandboxv1alpha1.SwiftSandbox {
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sb", Namespace: "ns", UID: "sb-uid",
			Finalizers:        []string{sandboxTerminationFinalizer},
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{Image: "busybox:1"},
		Status: sandboxv1alpha1.SwiftSandboxStatus{
			Phase: sandboxv1alpha1.SwiftSandboxRunning,
			Conditions: []metav1.Condition{{
				Type: sandboxv1alpha1.SwiftSandboxConditionWorkloadReady, Status: metav1.ConditionTrue,
				Reason: "ProbeSucceeded", LastTransitionTime: metav1.Now(),
			}},
		},
	}
	if drain != nil {
		sb.Spec.Termination = &sandboxv1alpha1.SandboxTermination{DrainPeriod: &metav1.Duration{Duration: *drain}}
	}
	return sb
}

// launcherFor is sb's running launcher pod, Ready through the workload gate.
func launcherFor(sb *sandboxv1alpha1.SwiftSandbox, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: sb.Namespace, UID: types.UID(name + "-uid"),
			Labels:     map[string]string{SandboxLabelKey: sb.Name},
			Finalizers: []string{testHold},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1alpha1.GroupName + "/v1alpha1", Kind: "SwiftSandbox",
				Name: sb.Name, UID: sb.UID, Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{ReadinessGates: []corev1.PodReadinessGate{{ConditionType: WorkloadReadyGate}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: WorkloadReadyGate, Status: corev1.ConditionTrue}},
		},
	}
}

// deleteCall is one Delete the reconciler made.
type deleteCall struct {
	name  string
	grace *int64
	uid   *types.UID
}

func terminationClient(deletes *[]deleteCall, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}, &corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				o := &client.DeleteOptions{}
				o.ApplyOptions(opts)
				call := deleteCall{name: obj.GetName(), grace: o.GracePeriodSeconds}
				if o.Preconditions != nil {
					call.uid = o.Preconditions.UID
				}
				*deletes = append(*deletes, call)
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
}

func reconcileDeletion(t *testing.T, c client.Client) ctrl.Result {
	t.Helper()
	// A fresh reconciler each pass: nothing may depend on memory, only on
	// the objects (a controller restart).
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(10)}
	var sb sandboxv1alpha1.SwiftSandbox
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &sb); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	res, err := r.handleDeletion(context.Background(), &sb)
	if err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	return res
}

func gateStatus(t *testing.T, c client.Client, name string) (corev1.ConditionStatus, string) {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: name}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == WorkloadReadyGate {
			return cond.Status, cond.Message
		}
	}
	return "", ""
}

func releasePod(t *testing.T, c client.Client, name string) {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: name}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Finalizers = nil
	if err := c.Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
}

// Deleting a sandbox whose workload runs: readiness is withdrawn first, the
// launcher pod is deleted with drain + stop allowance as its grace period
// (UID-preconditioned), and the sandbox stays until the pod is gone. A
// controller restart in the middle (a fresh reconciler) deletes nothing twice.
func TestTermination_RunningWorkloadDrainsThenGoes(t *testing.T) {
	drain := 5 * time.Second
	sb := deletedSandbox(&drain)
	pod := launcherFor(sb, "sb")
	var deletes []deleteCall
	c := terminationClient(&deletes, sb, pod)

	res := reconcileDeletion(t, c)
	if len(deletes) != 1 || deletes[0].name != "sb" {
		t.Fatalf("deletes = %+v, want the launcher pod once", deletes)
	}
	if g := deletes[0].grace; g == nil || *g != 20 {
		t.Errorf("grace = %v, want 20 (5s drain + 15s to stop)", g)
	}
	if u := deletes[0].uid; u == nil || *u != pod.UID {
		t.Errorf("delete precondition uid = %v, want %s", u, pod.UID)
	}
	if res.RequeueAfter < drain {
		t.Errorf("requeue %v, want at least the drain", res.RequeueAfter)
	}
	if st, _ := gateStatus(t, c, "sb"); st != corev1.ConditionFalse {
		t.Errorf("workload gate %q, want False before the pod is deleted", st)
	}
	var got sandboxv1alpha1.SwiftSandbox
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &got); err != nil {
		t.Fatalf("sandbox should stay while its launcher stops: %v", err)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionTerminating); cond == nil || cond.Reason != terminatingDraining {
		t.Errorf("Terminating = %+v, want reason Draining", cond)
	}
	if apimeta.IsStatusConditionTrue(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionWorkloadReady) {
		t.Error("WorkloadReady still True on a terminating sandbox")
	}

	// Restart: the pod is terminating; nothing is deleted again.
	reconcileDeletion(t, c)
	if len(deletes) != 1 {
		t.Errorf("deletes after restart = %+v, want still one", deletes)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &got); err != nil {
		t.Fatalf("sandbox released while its launcher still runs: %v", err)
	}

	// The kubelet has stopped the containers and removed the pod.
	releasePod(t, c, "sb")
	reconcileDeletion(t, c)
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &got)
	if !apierrors.IsNotFound(err) {
		t.Errorf("sandbox should be gone once its launcher is, got err=%v finalizers=%v", err, got.Finalizers)
	}
}

// Deleted before its workload was ready: no drain configured, the pod goes
// with the stop allowance alone, and the gate is written False.
func TestTermination_BeforeWorkloadReady(t *testing.T) {
	sb := deletedSandbox(nil)
	pod := launcherFor(sb, "sb")
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = nil
	var deletes []deleteCall
	c := terminationClient(&deletes, sb, pod)

	reconcileDeletion(t, c)
	if len(deletes) != 1 || deletes[0].grace == nil || *deletes[0].grace != 15 {
		t.Fatalf("deletes = %+v, want one with grace 15", deletes)
	}
	if st, _ := gateStatus(t, c, "sb"); st != corev1.ConditionFalse {
		t.Errorf("workload gate %q, want False", st)
	}
}

// A warm slot's spec grace period was fixed before its workload existed; the
// delete passes the sandbox's own, and the gate message names the workload.
func TestTermination_WarmSlotUsesTheSandboxDrain(t *testing.T) {
	drain := 30 * time.Second
	sb := deletedSandbox(&drain)
	sb.Status.PodRef = "pool-slot-abc"
	pod := launcherFor(sb, "pool-slot-abc")
	pod.Spec.TerminationGracePeriodSeconds = ptr.To(int64(15))
	pod.Annotations = map[string]string{annSandboxExecActionID: string(sb.UID)}
	var deletes []deleteCall
	c := terminationClient(&deletes, sb, pod)

	reconcileDeletion(t, c)
	if len(deletes) != 1 || deletes[0].name != "pool-slot-abc" || deletes[0].grace == nil || *deletes[0].grace != 45 {
		t.Fatalf("deletes = %+v, want the slot pod with grace 45", deletes)
	}
	if st, msg := gateStatus(t, c, "pool-slot-abc"); st != corev1.ConditionFalse || !strings.HasPrefix(msg, workloadPrefix(string(sb.UID))) {
		t.Errorf("gate %q %q, want False for this workload", st, msg)
	}
}

// A launcher that has already finished needs no stop: the sandbox goes at once.
func TestTermination_FinishedLauncher(t *testing.T) {
	sb := deletedSandbox(nil)
	pod := launcherFor(sb, "sb")
	pod.Status.Phase = corev1.PodSucceeded
	var deletes []deleteCall
	c := terminationClient(&deletes, sb, pod)

	reconcileDeletion(t, c)
	if len(deletes) != 0 {
		t.Errorf("deletes = %+v, want none", deletes)
	}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &sandboxv1alpha1.SwiftSandbox{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("sandbox should be gone, got %v", err)
	}
}

// A launcher still present well past its grace period (an unreachable node)
// does not hold the sandbox forever. Its NetworkPolicy is not the sandbox's
// to release (TestIsolation_*).
func TestTermination_StuckLauncherReleasesTheSandboxAfterItsDeadline(t *testing.T) {
	sb := deletedSandbox(nil)
	pod := launcherFor(sb, "sb")
	pod.DeletionTimestamp = &metav1.Time{Time: time.Now().Add(-time.Minute)}
	pod.DeletionGracePeriodSeconds = ptr.To(int64(15))
	var deletes []deleteCall
	c := terminationClient(&deletes, sb, pod)

	reconcileDeletion(t, c)
	if len(deletes) != 0 {
		t.Errorf("deletes = %+v, want none", deletes)
	}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &sandboxv1alpha1.SwiftSandbox{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("sandbox should be released past the deadline, got %v", err)
	}
}

// Past the drain, the condition says the microVM is being stopped.
func TestTermination_StoppingAfterTheDrain(t *testing.T) {
	drain := 2 * time.Second
	sb := deletedSandbox(&drain)
	pod := launcherFor(sb, "sb")
	// Deleted 5s ago with a 17s grace period: the drain ended 3s ago.
	pod.DeletionGracePeriodSeconds = ptr.To(int64(17))
	pod.DeletionTimestamp = &metav1.Time{Time: time.Now().Add(12 * time.Second)}
	var deletes []deleteCall
	c := terminationClient(&deletes, sb, pod)

	res := reconcileDeletion(t, c)
	var got sandboxv1alpha1.SwiftSandbox
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &got); err != nil {
		t.Fatal(err)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionTerminating); cond == nil || cond.Reason != terminatingStopping {
		t.Errorf("Terminating = %+v, want reason Stopping", cond)
	}
	if res.RequeueAfter <= 0 {
		t.Error("want a requeue at the deadline")
	}
}

func TestLauncherGracePeriodSeconds(t *testing.T) {
	for _, tc := range []struct {
		drain *time.Duration
		want  int64
	}{
		{nil, 15},
		{ptr.To(time.Duration(0)), 15},
		{ptr.To(1500 * time.Millisecond), 17},
		{ptr.To(10 * time.Minute), 615},
	} {
		sb := deletedSandbox(tc.drain)
		if got := launcherGracePeriodSeconds(sb); got != tc.want {
			t.Errorf("drain %v: grace %d, want %d", tc.drain, got, tc.want)
		}
		if got := *buildPod(sb, defaultKernelProfile).Spec.TerminationGracePeriodSeconds; got != tc.want {
			t.Errorf("drain %v: pod grace %d, want %d", tc.drain, got, tc.want)
		}
	}
	if terminationIntent(deletedSandbox(nil)) != nil {
		t.Error("no drain should send no termination intent")
	}
	if ti := terminationIntent(deletedSandbox(ptr.To(1500 * time.Millisecond))); ti == nil || ti.DrainMs != 1500 {
		t.Errorf("termination intent %+v, want drainMs 1500", ti)
	}
}

// --- isolation ---

func sandboxPolicy(owner string, deleting bool) *networkingv1.NetworkPolicy {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sb-restricted", Namespace: "ns",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxv1alpha1.GroupName + "/v1alpha1", Kind: owner,
				Name: "sb", UID: "sb-uid", Controller: ptr.To(true),
			}},
		},
		Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{SandboxLabelKey: "sb"}}},
	}
	if deleting {
		np.Finalizers = []string{launcherIsolationFinalizer}
		np.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	}
	return np
}

func reconcileIsolation(t *testing.T, c client.Client) ctrl.Result {
	t.Helper()
	r := &IsolationReconciler{Client: c, APIReader: c}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "sb-restricted"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func getPolicy(c client.Client) (*networkingv1.NetworkPolicy, error) {
	var np networkingv1.NetworkPolicy
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb-restricted"}, &np)
	return &np, err
}

// A policy from before the finalizer existed gets it, so a running sandbox is
// covered from the upgrade on.
func TestIsolation_ExistingPolicyGetsTheFinalizer(t *testing.T) {
	for _, owner := range []string{"SwiftSandbox", "SwiftSandboxPool"} {
		c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sandboxPolicy(owner, false)).Build()
		reconcileIsolation(t, c)
		np, err := getPolicy(c)
		if err != nil || len(np.Finalizers) != 1 || np.Finalizers[0] != launcherIsolationFinalizer {
			t.Errorf("%s: finalizers %v err %v, want the isolation finalizer", owner, np.Finalizers, err)
		}
	}
}

// Somebody else's NetworkPolicy is left alone.
func TestIsolation_IgnoresOtherPolicies(t *testing.T) {
	np := sandboxPolicy("Deployment", false)
	np.OwnerReferences[0].APIVersion = "apps/v1"
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(np).Build()
	reconcileIsolation(t, c)
	got, _ := getPolicy(c)
	if len(got.Finalizers) != 0 {
		t.Errorf("finalizers %v on a foreign policy", got.Finalizers)
	}
}

// The deleted policy stays while a pod it selects may still run (also a
// terminating one), and goes once that pod has finished or is gone.
func TestIsolation_HeldUntilTheProtectedPodCannotRun(t *testing.T) {
	sb := deletedSandbox(nil)
	pod := launcherFor(sb, "sb")
	pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(sandboxPolicy("SwiftSandbox", true), pod).
		WithStatusSubresource(&corev1.Pod{}).Build()

	if res := reconcileIsolation(t, c); res.RequeueAfter == 0 {
		t.Error("want a recheck while the pod runs")
	}
	if _, err := getPolicy(c); err != nil {
		t.Fatalf("policy removed while the launcher is terminating: %v", err)
	}

	// Its containers have stopped.
	var p corev1.Pod
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "sb"}, &p)
	p.Status.Phase = corev1.PodFailed
	if err := c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcileIsolation(t, c)
	if _, err := getPolicy(c); !apierrors.IsNotFound(err) {
		t.Errorf("policy should go once the launcher has stopped, got %v", err)
	}
}

func TestIsolation_ReleasedWhenNoPodMatches(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sandboxPolicy("SwiftSandbox", true)).Build()
	reconcileIsolation(t, c)
	if _, err := getPolicy(c); !apierrors.IsNotFound(err) {
		t.Errorf("policy should go, got %v", err)
	}
}

// A pod event reaches the held policies that select it.
func TestIsolation_PolicyForPod(t *testing.T) {
	other := sandboxPolicy("SwiftSandbox", true)
	other.Name = "other-restricted"
	other.Spec.PodSelector.MatchLabels = map[string]string{SandboxLabelKey: "other"}
	live := sandboxPolicy("SwiftSandbox", false)
	live.Name = "live-restricted"
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(sandboxPolicy("SwiftSandbox", true), other, live).Build()
	r := &IsolationReconciler{Client: c, APIReader: c}
	reqs := r.policiesForPod(context.Background(), launcherFor(deletedSandbox(nil), "sb"))
	if len(reqs) != 1 || reqs[0].Name != "sb-restricted" {
		t.Errorf("requests %v, want sb-restricted only", reqs)
	}
}

// A policy by the launcher's name that is still being deleted, or belongs to
// another owner, belongs to a previous launcher: no pod is created against it.
func TestEnsureLauncherPolicy(t *testing.T) {
	ctx := context.Background()
	owner := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "sb-uid"}}
	newPolicy := func() *networkingv1.NetworkPolicy {
		np := buildNetworkPolicy(owner)
		np.OwnerReferences = []metav1.OwnerReference{{APIVersion: sandboxv1alpha1.GroupName + "/v1alpha1", Kind: "SwiftSandbox", Name: "sb", UID: "sb-uid", Controller: ptr.To(true)}}
		return np
	}

	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	if ok, _, err := ensureLauncherPolicy(ctx, c, owner, newPolicy()); !ok || err != nil {
		t.Fatalf("create: ok=%v err=%v", ok, err)
	}
	np, _ := getPolicy(c)
	if len(np.Finalizers) != 1 || np.Finalizers[0] != launcherIsolationFinalizer {
		t.Errorf("finalizers %v, want the isolation finalizer from creation", np.Finalizers)
	}
	if ok, _, err := ensureLauncherPolicy(ctx, c, owner, newPolicy()); !ok || err != nil {
		t.Errorf("own policy again: ok=%v err=%v", ok, err)
	}

	previous := sandboxPolicy("SwiftSandbox", true)
	previous.OwnerReferences[0].UID = "old-uid"
	c = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(previous).Build()
	if ok, msg, err := ensureLauncherPolicy(ctx, c, owner, newPolicy()); ok || err != nil || msg == "" {
		t.Errorf("terminating previous policy: ok=%v msg=%q err=%v, want a wait", ok, msg, err)
	}

	foreign := sandboxPolicy("SwiftSandbox", false)
	foreign.OwnerReferences[0].UID = "old-uid"
	c = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(foreign).Build()
	if ok, _, err := ensureLauncherPolicy(ctx, c, owner, newPolicy()); ok || err != nil {
		t.Errorf("previous owner's policy: ok=%v err=%v, want a wait", ok, err)
	}
}

// A warm slot's NetworkPolicy exists before its pod does.
func TestCreateWarmSlot_PolicyBeforePod(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "pool-uid"},
		Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "busybox:1"},
	}
	var order []string
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				switch obj.(type) {
				case *corev1.Pod:
					order = append(order, "pod")
				case *networkingv1.NetworkPolicy:
					order = append(order, "policy")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	pr := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(10)}
	if err := pr.createWarmSlot(ctx, pool, defaultKernelProfile, resolvedImage{RootfsPath: "/cache/x.ext4"}, "", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "policy,pod" {
		t.Errorf("create order %v, want the policy first", order)
	}
}
