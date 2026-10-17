package swiftsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

const (
	plDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	plDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func testPlacement(digest string) *sandboxv1alpha1.SwiftArtifactPlacement {
	return &sandboxv1alpha1.SwiftArtifactPlacement{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "default", UID: "pl-uid", Generation: 1},
		Spec: sandboxv1alpha1.SwiftArtifactPlacementSpec{
			PoolRef:   corev1.LocalObjectReference{Name: "pool"},
			Artifacts: []sandboxv1alpha1.PlacementArtifact{{Name: "a", Ref: "reg.example/art@" + digest, Layout: "unpacked"}},
		},
	}
}

func poolSlot(name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{PoolLabelKey: "pool", SlotStateLabelKey: slotStateWarm}},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// answers serves every digest reference with its own digest, unless err is
// set for it.
func answers(errs map[string]error) func(context.Context, resolveKind, materialize.Options) resolveResult {
	return func(_ context.Context, _ resolveKind, o materialize.Options) resolveResult {
		if err := errs[o.ImageRef]; err != nil {
			return resolveResult{err: err}
		}
		return resolveResult{repository: repoOf(o.ImageRef), digest: digestOf(o.ImageRef)}
	}
}

type placementEnv struct {
	t     *testing.T
	c     client.Client
	now   time.Time
	fetch func(context.Context, resolveKind, materialize.Options) resolveResult
	res   *registryResolver
}

func newPlacementEnv(t *testing.T, objs ...client.Object) *placementEnv {
	pool := &sandboxv1alpha1.SwiftSandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(append(objs, pool)...).
		WithStatusSubresource(&sandboxv1alpha1.SwiftArtifactPlacement{}, &corev1.Pod{}).Build()
	e := &placementEnv{t: t, c: c, now: time.Unix(1_800_000_000, 0)}
	e.res = stubResolver(answers(nil))
	return e
}

// reconcile runs one pass with a fresh reconciler: nothing may depend on
// memory but the resolver's cache (a controller restart).
func (e *placementEnv) reconcile() ctrl.Result {
	e.t.Helper()
	r := &ArtifactPlacementReconciler{Client: e.c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(50), resolver: e.res, now: func() time.Time { return e.now }}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "cache"}})
	if err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (e *placementEnv) placement() *sandboxv1alpha1.SwiftArtifactPlacement {
	e.t.Helper()
	var p sandboxv1alpha1.SwiftArtifactPlacement
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "cache"}, &p); err != nil {
		e.t.Fatal(err)
	}
	return &p
}

func (e *placementEnv) preparePods() map[string]*corev1.Pod {
	e.t.Helper()
	var pods corev1.PodList
	if err := e.c.List(context.Background(), &pods, client.InNamespace("default"), client.MatchingLabels{placementLabel: "cache"}); err != nil {
		e.t.Fatal(err)
	}
	out := map[string]*corev1.Pod{}
	for i := range pods.Items {
		out[pods.Items[i].Spec.NodeName] = &pods.Items[i]
	}
	return out
}

// finish completes the prepare pod on node: each container with result
// (nil: prepared).
func (e *placementEnv) finish(node string, skip *materialize.Skip) {
	e.t.Helper()
	pod := e.preparePods()[node]
	if pod == nil {
		e.t.Fatalf("no prepare pod on %s", node)
	}
	msg, _ := json.Marshal(materialize.Result{Digest: "sha256:x", CacheHit: true, Skipped: skip})
	pod.Status.Phase = corev1.PodSucceeded
	for _, c := range pod.Spec.Containers {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{Name: c.Name,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: string(msg),
				FinishedAt: metav1.NewTime(e.now)}}})
	}
	if err := e.c.Status().Update(context.Background(), pod); err != nil {
		e.t.Fatal(err)
	}
}

func preparedCondition(p *sandboxv1alpha1.SwiftArtifactPlacement) *metav1.Condition {
	return apimeta.FindStatusCondition(p.Status.Conditions, sandboxv1alpha1.PlacementConditionPrepared)
}

// One prepare pod per node that runs a slot of the pool, pinned to it, built
// from the checkout's own containers, owned by the placement. A node is
// prepared once its pod reports every artifact prepared; then the pod goes.
func TestPlacement_PreparesEverySlotNode(t *testing.T) {
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"), poolSlot("s2", "node-b"), poolSlot("s3", "node-b"))
	e.reconcile()

	pods := e.preparePods()
	if len(pods) != 2 || pods["node-a"] == nil || pods["node-b"] == nil {
		t.Fatalf("prepare pods %v, want one on node-a and one on node-b", keys(pods))
	}
	pod := pods["node-a"]
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != artifactInitPrefix+"a" {
		t.Fatalf("containers %v, want the artifact's materializer", pod.Spec.Containers)
	}
	args := strings.Join(pod.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--image reg.example/art@"+plDigestA) || !strings.Contains(args, "--optional") {
		t.Errorf("args %q: want the pinned digest, optional (a classified result)", args)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || !metav1.IsControlledBy(pod, e.placement()) {
		t.Error("a prepare pod needs a deadline and the placement as owner")
	}
	if c := preparedCondition(e.placement()); c == nil || c.Status != metav1.ConditionFalse || c.Reason != sandboxv1alpha1.PlacementReasonPreparing {
		t.Errorf("Prepared = %+v, want False/Preparing", c)
	}

	e.finish("node-a", nil)
	e.reconcile()
	p := e.placement()
	if p.Status.PreparedNodes != 1 || p.Status.TargetNodes != 2 {
		t.Errorf("prepared %d/%d, want 1/2", p.Status.PreparedNodes, p.Status.TargetNodes)
	}
	if _, ok := e.preparePods()["node-a"]; ok {
		t.Error("a finished prepare pod should be deleted")
	}
	e.finish("node-b", nil)
	e.reconcile()
	p = e.placement()
	if c := preparedCondition(p); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Prepared = %+v, want True", c)
	}
	if p.Status.Artifacts[0].PreparedNodes != 2 || p.Status.Artifacts[0].Digest != plDigestA {
		t.Errorf("artifact status %+v", p.Status.Artifacts[0])
	}

	// Prepared nodes stay so until their recheck: no new pods.
	e.reconcile()
	if len(e.preparePods()) != 0 {
		t.Error("a prepared node got a prepare pod before its recheck")
	}
	e.now = e.now.Add(placementRecheck + time.Second)
	e.reconcile()
	if len(e.preparePods()) != 2 {
		t.Error("prepared nodes should be checked again after placementRecheck")
	}
}

// A node that starts hosting a slot is prepared; one that stops stops
// counting and loses its prepare pod.
func TestPlacement_FollowsNodeChurn(t *testing.T) {
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"), poolSlot("s2", "node-b"))
	e.reconcile()
	e.finish("node-a", nil)
	e.reconcile()

	// node-b leaves the pool (its slot is gone); node-c joins.
	if err := e.c.Delete(context.Background(), poolSlot("s2", "node-b")); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(context.Background(), poolSlot("s3", "node-c")); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	pods := e.preparePods()
	if _, ok := pods["node-b"]; ok {
		t.Error("node-b's prepare pod should go with its last slot")
	}
	if pods["node-c"] == nil {
		t.Error("node-c should get a prepare pod")
	}
	p := e.placement()
	if p.Status.TargetNodes != 2 || p.Status.PreparedNodes != 1 {
		t.Errorf("prepared %d/%d, want 1/2 (node-a prepared, node-c preparing)", p.Status.PreparedNodes, p.Status.TargetNodes)
	}
	for _, n := range p.Status.Nodes {
		if n.Name == "node-b" {
			t.Error("node-b is still in the status")
		}
	}
}

// A new digest is a new generation: the old prepare pods go, no node counts
// as prepared for it until it holds it.
func TestPlacement_DigestChange(t *testing.T) {
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"))
	e.reconcile()
	e.finish("node-a", nil)
	e.reconcile()
	e.reconcile() // a running pod for the old generation, if any, is what we check below

	p := e.placement()
	p.Spec.Artifacts[0].Ref = "reg.example/art@" + plDigestB
	p.Generation = 2
	if err := e.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	p = e.placement()
	if p.Status.PreparedNodes != 0 || preparedCondition(p).Status != metav1.ConditionFalse {
		t.Errorf("a new digest cannot be prepared yet: %d prepared, %+v", p.Status.PreparedNodes, preparedCondition(p))
	}
	pod := e.preparePods()["node-a"]
	if pod == nil || !strings.Contains(strings.Join(pod.Spec.Containers[0].Args, " "), plDigestB) {
		t.Fatalf("want a prepare pod for the new digest, got %v", pod)
	}
}

// A registry refusal (with the placement's own credentials) prepares
// nothing: the placement says so. A transient failure keeps waiting.
func TestPlacement_RegistryRefusedAndUnavailable(t *testing.T) {
	refused := &transport.Error{StatusCode: 404}
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"))
	e.res = stubResolver(answers(map[string]error{"reg.example/art@" + plDigestA: refused}))
	e.reconcile()
	p := e.placement()
	if len(e.preparePods()) != 0 {
		t.Error("a refused artifact must not be fetched")
	}
	if c := preparedCondition(p); c.Reason != sandboxv1alpha1.PlacementReasonArtifactRefused {
		t.Errorf("Prepared = %+v, want ArtifactRefused", c)
	}

	e = newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"))
	e.res = stubResolver(answers(map[string]error{"reg.example/art@" + plDigestA: errors.New("connection refused")}))
	e.reconcile()
	p = e.placement()
	if len(e.preparePods()) != 0 {
		t.Error("an artifact the registry has not authorized must not be fetched")
	}
	if p.Status.Artifacts[0].Reason != sandboxv1alpha1.PlacementReasonResolving {
		t.Errorf("artifact %+v, want Resolving", p.Status.Artifacts[0])
	}
}

// A node whose artifact fails verification is not prepared, says why, and is
// retried only after a backoff that doubles; another node is unaffected.
func TestPlacement_VerificationFailureBacksOff(t *testing.T) {
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"), poolSlot("s2", "node-b"))
	e.reconcile()
	e.finish("node-a", &materialize.Skip{Reason: "VerificationFailed", Message: "no matching signatures"})
	e.finish("node-b", nil)
	e.reconcile()

	p := e.placement()
	a := nodeStatus(p, "node-a")
	if a.Prepared || a.Reason != sandboxv1alpha1.PlacementReasonVerificationFailed || a.Failures != 1 || !strings.Contains(a.Message, "no matching signatures") {
		t.Errorf("node-a %+v, want VerificationFailed, 1 failure", a)
	}
	if b := nodeStatus(p, "node-b"); !b.Prepared {
		t.Errorf("node-b %+v, want prepared despite node-a", b)
	}
	if c := preparedCondition(p); c.Status != metav1.ConditionFalse || c.Reason != sandboxv1alpha1.PlacementReasonVerificationFailed {
		t.Errorf("Prepared = %+v, want False/VerificationFailed", c)
	}

	e.reconcile()
	if _, ok := e.preparePods()["node-a"]; ok {
		t.Error("node-a retried before its backoff")
	}
	e.now = e.now.Add(placementBackoff + time.Second)
	e.reconcile()
	if _, ok := e.preparePods()["node-a"]; !ok {
		t.Fatal("node-a not retried after its backoff")
	}
	e.finish("node-a", &materialize.Skip{Reason: "VerificationFailed", Message: "again"})
	e.reconcile()
	if a := nodeStatus(e.placement(), "node-a"); a.Failures != 2 {
		t.Errorf("failures %d, want 2", a.Failures)
	}
	e.now = e.now.Add(placementBackoff + time.Second) // less than the doubled backoff
	e.reconcile()
	if _, ok := e.preparePods()["node-a"]; ok {
		t.Error("the second retry should wait the doubled backoff")
	}
}

// No pool, no nodes: the placement says so and holds no pods.
func TestPlacement_PoolNotFoundAndNoNodes(t *testing.T) {
	p := testPlacement(plDigestA)
	p.Spec.PoolRef.Name = "missing"
	e := newPlacementEnv(t, p)
	e.reconcile()
	if c := preparedCondition(e.placement()); c.Reason != sandboxv1alpha1.PlacementReasonPoolNotFound {
		t.Errorf("Prepared = %+v, want PoolNotFound", c)
	}

	e = newPlacementEnv(t, testPlacement(plDigestA))
	e.reconcile()
	if c := preparedCondition(e.placement()); c.Reason != sandboxv1alpha1.PlacementReasonNoTargetNodes {
		t.Errorf("Prepared = %+v, want NoTargetNodes", c)
	}
}

// A pod by a prepare pod's name that the placement did not create is never
// read for a result.
func TestPlacement_ForeignPodIsNotTrusted(t *testing.T) {
	pl := testPlacement(plDigestA)
	fakePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: preparePodName(pl, "node-a", ""), Namespace: "default",
		Labels: map[string]string{placementLabel: "cache"}, Annotations: map[string]string{placementNodeAnnotation: "node-a"}},
		Spec:   corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: artifactInitPrefix + "a"}}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	e := newPlacementEnv(t, pl, poolSlot("s1", "node-a"), fakePod)
	e.reconcile()
	if n := nodeStatus(e.placement(), "node-a"); n.Prepared {
		t.Errorf("node-a %+v prepared from a pod the placement did not create", n)
	}
	var pod corev1.Pod
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(fakePod), &pod); apierrors.IsNotFound(err) {
		t.Error("the placement must not delete a pod it does not own")
	}
}

// A checkout prefers a warm slot on a node prepared for every artifact it
// mounts, and ignores a placement for another digest or pool.
func TestPreparedNodes(t *testing.T) {
	pl := testPlacement(plDigestA)
	pl.Status = sandboxv1alpha1.SwiftArtifactPlacementStatus{
		Artifacts: []sandboxv1alpha1.PlacementArtifactStatus{{Name: "a", Digest: plDigestA, Reason: sandboxv1alpha1.PlacementReasonAuthorized}},
		Nodes: []sandboxv1alpha1.PlacementNodeStatus{
			{Name: "node-a", Prepared: true, ObservedGeneration: 1},
			{Name: "node-b", Prepared: false, ObservedGeneration: 1},
			{Name: "node-c", Prepared: true, ObservedGeneration: 0}, // an older generation
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pl).Build()
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}
	sb := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "default"},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{PoolRef: &corev1.LocalObjectReference{Name: "pool"},
			Artifacts: []sandboxv1alpha1.SandboxArtifact{{Name: "x", Ref: "reg.example/art:v1", Layout: "unpacked", MountPath: "/x"}}},
		Status: sandboxv1alpha1.SwiftSandboxStatus{Artifacts: []sandboxv1alpha1.SandboxArtifactStatus{{Name: "x", Digest: plDigestA, MountPath: "/x"}}}}
	got := r.preparedNodes(context.Background(), sb)
	if len(got) != 1 || !got["node-a"] {
		t.Errorf("prepared nodes %v, want node-a only", got)
	}
	sb.Status.Artifacts[0].Digest = plDigestB
	if got := r.preparedNodes(context.Background(), sb); len(got) != 0 {
		t.Errorf("another digest: %v, want none", got)
	}
	sb.Status.Artifacts[0].Digest = plDigestA
	sb.Spec.Artifacts[0].Layout = "oci"
	if got := r.preparedNodes(context.Background(), sb); len(got) != 0 {
		t.Errorf("another layout: %v, want none", got)
	}
}

func keys[V any](m map[string]V) []string { return sortedKeys(m) }
