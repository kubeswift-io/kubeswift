package swiftsandbox

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func placementSandbox(digest string) *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "default"},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{PoolRef: &corev1.LocalObjectReference{Name: "pool"},
			Artifacts: []sandboxv1alpha1.SandboxArtifact{{Name: "x", Ref: "reg.example/art@" + digest, Layout: "unpacked", MountPath: "/x"}}},
		Status: sandboxv1alpha1.SwiftSandboxStatus{Artifacts: []sandboxv1alpha1.SandboxArtifactStatus{{Name: "x", Digest: digest, MountPath: "/x"}}}}
}

func (e *placementEnv) prepared() map[string]bool {
	r := &SwiftSandboxReconciler{Client: e.c, Scheme: scheme.Scheme}
	return r.preparedNodes(context.Background(), placementSandbox(plDigestA))
}

// A placement verifies under the policies a checkout in its namespace
// would: its prepare pod carries the requirements, a prepared node records
// which, and checkouts are steered to it.
func TestPlacement_PolicyRequirements(t *testing.T) {
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"), trustedNamespace(), verificationPolicy(t))
	e.reconcile()
	pod := e.preparePods()["node-a"]
	if pod == nil {
		t.Fatal("no prepare pod")
	}
	if reqs := requirementsEnv(t, &pod.Spec.Containers[0]); len(reqs) != 1 || reqs[0].UID != "policy-uid" {
		t.Fatalf("prepare pod requirements = %+v", reqs)
	}
	p := e.placement()
	if len(p.Status.Artifacts) != 1 || len(p.Status.Artifacts[0].Policies) != 1 || p.Status.Artifacts[0].Policies[0].Name != "platform" {
		t.Errorf("artifact status %+v", p.Status.Artifacts)
	}
	if len(e.prepared()) != 0 {
		t.Error("a node not yet prepared is preferred")
	}
	e.finish("node-a", nil)
	e.reconcile()
	p = e.placement()
	if n := p.Status.Nodes[0]; !n.Prepared || n.Requirements == "" {
		t.Fatalf("node %+v", n)
	}
	if got := e.prepared(); !got["node-a"] {
		t.Errorf("prepared nodes %v", got)
	}
}

// A policy change that alters what the artifact must meet makes the node
// unprepared until it is verified again; checkouts stop preferring it.
func TestPlacement_PolicyChangePreparesAgain(t *testing.T) {
	pol := verificationPolicy(t)
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"), trustedNamespace(), pol)
	e.reconcile()
	first := e.preparePods()["node-a"].Name
	e.finish("node-a", nil)
	e.reconcile()
	before := e.placement().Status.Nodes[0].Requirements

	// The administrator adds a claim.
	var cur sandboxv1alpha1.SwiftArtifactVerificationPolicy
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(pol), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Claims = append(cur.Spec.Claims, sandboxv1alpha1.ArtifactClaimRequirement{Key: "example.com/kind", Value: strp("cache")})
	cur.Generation++
	if err := e.c.Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
	// Before the placement has even looked again, a checkout no longer
	// takes the node's old preparation for the new policy.
	if got := e.prepared(); len(got) != 0 {
		t.Errorf("between the policy change and the placement's next pass: %v", got)
	}
	e.reconcile()
	n := e.placement().Status.Nodes[0]
	if n.Prepared || n.Requirements == before || n.Requirements == "" {
		t.Fatalf("after the policy change: %+v", n)
	}
	if len(e.prepared()) != 0 {
		t.Error("a node prepared under the old policy is still preferred")
	}
	pod := e.preparePods()["node-a"]
	if pod == nil || pod.Name == first {
		t.Fatalf("no new prepare pod (%v)", pod)
	}
	reqs := requirementsEnv(t, &pod.Spec.Containers[0])
	if len(reqs) != 1 || len(reqs[0].Claims) != 2 {
		t.Errorf("new requirements %+v", reqs)
	}
	e.finish("node-a", nil)
	e.reconcile()
	if !e.prepared()["node-a"] {
		t.Error("prepared again but not preferred")
	}

	// Unbinding the namespace (no policy governs any more) prepares again
	// with no requirement at all.
	ns := trustedNamespace()
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(ns), ns); err != nil {
		t.Fatal(err)
	}
	ns.Labels = nil
	if err := e.c.Update(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if n := e.placement().Status.Nodes[0]; n.Prepared || n.Requirements != "" {
		t.Errorf("after unbinding: %+v", n)
	}
}

// A policy refusal is never prepared, never asked of the registry, and
// says so; a claim mismatch on the node fails it as such.
func TestPlacement_PolicyDeniedAndClaimMismatch(t *testing.T) {
	pl := testPlacement(plDigestA)
	pl.Spec.Artifacts[0].RequirePlatformVerification = true
	e := newPlacementEnv(t, pl, poolSlot("s1", "node-a"))
	e.reconcile()
	p := e.placement()
	cond := preparedCondition(p)
	if p.Status.Artifacts[0].Reason != sandboxv1alpha1.PlacementReasonPolicyDenied || cond == nil || cond.Reason != sandboxv1alpha1.PlacementReasonPolicyDenied ||
		!strings.Contains(p.Status.Artifacts[0].Message, "NoMatchingPolicy") {
		t.Fatalf("denied: %+v / %+v", p.Status.Artifacts, cond)
	}
	if len(e.preparePods()) != 0 {
		t.Error("a refused artifact was prepared")
	}

	e2 := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"), trustedNamespace(), verificationPolicy(t))
	e2.reconcile()
	e2.finish("node-a", &materialize.Skip{Reason: "ClaimMismatch", Message: "policy platform: signed by authority release, but claim example.com/consumer is missing"})
	e2.reconcile()
	n := e2.placement().Status.Nodes[0]
	if n.Prepared || n.Reason != sandboxv1alpha1.PlacementReasonClaimMismatch || n.Failures != 1 {
		t.Errorf("claim mismatch: %+v", n)
	}
	if c := preparedCondition(e2.placement()); c == nil || c.Reason != sandboxv1alpha1.PlacementReasonClaimMismatch {
		t.Errorf("condition %+v", c)
	}
}

// A checkout whose artifact asks for other claims than the placement's
// does not take the placement's preparation as its own.
func TestPreparedNodes_ClaimsMustMatch(t *testing.T) {
	pl := testPlacement(plDigestA)
	pl.Spec.Artifacts[0].Claims = map[string]string{"source": "s1"}
	pl.Status = sandboxv1alpha1.SwiftArtifactPlacementStatus{
		Artifacts: []sandboxv1alpha1.PlacementArtifactStatus{{Name: "a", Digest: plDigestA, Reason: sandboxv1alpha1.PlacementReasonAuthorized}},
		Nodes:     []sandboxv1alpha1.PlacementNodeStatus{{Name: "node-a", Prepared: true, ObservedGeneration: 1}},
	}
	e := newPlacementEnv(t, pl)
	r := &SwiftSandboxReconciler{Client: e.c, Scheme: scheme.Scheme}
	sb := placementSandbox(plDigestA)
	if got := r.preparedNodes(context.Background(), sb); len(got) != 0 {
		t.Errorf("no claims vs the placement's: %v", got)
	}
	sb.Spec.Artifacts[0].Claims = map[string]string{"source": "s1"}
	if got := r.preparedNodes(context.Background(), sb); !got["node-a"] {
		t.Errorf("same claims: %v", got)
	}
}

func strp(s string) *string { return &s }

// A prepare pod built before every artifact was authorized is not a failure
// for the artifacts it never had (it used to report them "did not run",
// with a backoff): the node is simply not prepared yet.
func TestPlacement_PodWithoutALaterArtifactIsNotAFailure(t *testing.T) {
	r := &ArtifactPlacementReconciler{Recorder: record.NewFakeRecorder(5)}
	p := testPlacement(plDigestA)
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: artifactInitPrefix + "a"}}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: artifactInitPrefix + "a",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: `{"digest":"` + plDigestA + `"}`}}}}}}
	st := sandboxv1alpha1.PlacementNodeStatus{Name: "node-a"}
	r.readPrepareResult(p, &st, pod, []string{"a", "b"})
	if st.Prepared || st.Reason != sandboxv1alpha1.PlacementReasonPreparing || st.Failures != 0 || st.LastFailureTime != nil {
		t.Errorf("node %+v", st)
	}
	r.readPrepareResult(p, &st, pod, []string{"a"})
	if !st.Prepared {
		t.Errorf("every wanted artifact prepared: %+v", st)
	}
}

// The requirements a node is prepared under depend on the policies, not on
// which artifacts the registry has answered for yet.
func TestPlacementRequirements_IndependentOfAuthorization(t *testing.T) {
	pl := testPlacement(plDigestA)
	pl.Spec.Artifacts = append(pl.Spec.Artifacts, sandboxv1alpha1.PlacementArtifact{Name: "b", Ref: "reg.example/art@" + plDigestB, Layout: "unpacked"})
	e := newPlacementEnv(t, pl, poolSlot("s1", "node-a"), trustedNamespace(), verificationPolicy(t))
	r := &ArtifactPlacementReconciler{Client: e.c}
	d, err := r.policies.decide(context.Background(), e.c, "default", placementArtifactInputs(pl))
	if err != nil {
		t.Fatal(err)
	}
	if got := placementRequirements(pl, d); got == "" || got != placementRequirements(pl, d) {
		t.Errorf("requirements %q", got)
	}
}
