package swiftsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/artifactpolicy"
)

// A SwiftArtifactPlacement prepares its artifacts on each node that runs a
// slot of its pool, with a prepare pod per node: exactly the containers a
// checkout's fetch pod runs (the same materializer, pull Secret, cosign key,
// cache markers), pinned to the node. Nothing new is trusted: a checkout
// still resolves its own artifacts with its own credentials, and the node's
// launcher still checks that the entry was read from the checkout's
// repository and verified with its key before it is mounted. A placement
// only moves the fetch from the checkout to before it.
const (
	// placementLabel names the placement on its prepare pods.
	placementLabel = "sandbox.kubeswift.io/artifact-placement"
	// placementNodeAnnotation names the node a prepare pod is for.
	placementNodeAnnotation = "sandbox.kubeswift.io/placement-node"
	// placementResync re-reads the pool and the registry: the lookup keeps
	// the authorization of the placement's credentials fresh (the resolver
	// renews an answer looked up after half its lifetime).
	placementResync = time.Minute
	// placementRecheck is how long a node counts as prepared before its
	// cache is checked again (an entry can be evicted or the node rebuilt).
	placementRecheck = 15 * time.Minute
	// placementPrepareTimeout bounds one artifact's preparation on a node.
	placementPrepareTimeout = 10 * time.Minute
	// placementBackoff and placementMaxBackoff space retries on a node that
	// failed.
	placementBackoff    = 30 * time.Second
	placementMaxBackoff = 10 * time.Minute
)

// ArtifactPlacementReconciler reconciles SwiftArtifactPlacement.
type ArtifactPlacementReconciler struct {
	client.Client
	// APIReader reads the placement itself, uncached (the manager's API
	// reader; the cached client when nil). See Reconcile.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	// resolver is the registry resolver (sharedResolver when nil): the same
	// cache of authorizations the checkouts use.
	resolver *registryResolver
	resolved chan event.GenericEvent
	now      func() time.Time
	// policies evaluates SwiftArtifactVerificationPolicies, exactly as a
	// checkout in the placement's namespace would (artifact_policy.go).
	policies artifactPolicies
}

func (r *ArtifactPlacementReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// placementView is the placement as a sandbox that mounts its artifacts, so
// the checkout's own request and container builders serve it unchanged. The
// artifacts are optional, so each container reports a classified result
// (prepared, or skipped and why) and never fails the pod, bounded by
// placementPrepareTimeout from now.
func placementView(p *sandboxv1alpha1.SwiftArtifactPlacement, now time.Time) *sandboxv1alpha1.SwiftSandbox {
	v := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{
		Name: p.Name, Namespace: p.Namespace, UID: p.UID, CreationTimestamp: metav1.NewTime(now),
	}}
	for _, a := range p.Spec.Artifacts {
		v.Spec.Artifacts = append(v.Spec.Artifacts, sandboxv1alpha1.SandboxArtifact{
			Name: a.Name, Ref: a.Ref, Layout: a.ArtifactLayout(), MountPath: "/" + a.Name,
			PullSecretRef: a.PullSecretRef, VerifyKeySecretRef: a.VerifyKeySecretRef,
			Claims: a.Claims, RequirePlatformVerification: a.RequirePlatformVerification,
			Optional: true, FetchTimeout: &metav1.Duration{Duration: placementPrepareTimeout},
		})
	}
	return v
}

func (r *ArtifactPlacementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	defer requeueOnConflict(ctx, &res, &err)
	// The placement is read past the cache. This controller reconciles on
	// every slot and prepare pod event, often before the cache holds its own
	// last status write; a pass working from that older status saw a node it
	// had just recorded prepared as unprepared, started another prepare pod,
	// and wrote Preparing over Prepared, so the placement flapped between the
	// two for as long as the loop kept winning the race.
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var p sandboxv1alpha1.SwiftArtifactPlacement
	if err := reader.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if p.DeletionTimestamp != nil {
		return ctrl.Result{}, nil // its prepare pods are garbage collected with it
	}
	orig := p.Status.DeepCopy()
	now := r.clock()

	var pool sandboxv1alpha1.SwiftSandboxPool
	err = r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.PoolRef.Name}, &pool)
	if apierrors.IsNotFound(err) {
		if err := r.deletePreparePods(ctx, &p, nil, ""); err != nil {
			return ctrl.Result{}, err
		}
		p.Status.Nodes = nil
		r.summarize(&p, sandboxv1alpha1.PlacementReasonPoolNotFound, fmt.Sprintf("SwiftSandboxPool %s not found", p.Spec.PoolRef.Name))
		return r.finish(ctx, &p, orig, placementResync)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	targets, err := r.targetNodes(ctx, &p)
	if err != nil {
		return ctrl.Result{}, err
	}
	// The platform's policies, as a checkout here would see them: a refused
	// artifact is never prepared, and the others are prepared under what the
	// policies require now.
	decisions, err := r.policies.decide(ctx, r.Client, p.Namespace, placementArtifactInputs(&p))
	if err != nil {
		return ctrl.Result{}, err
	}
	authorized, refused := r.authorize(ctx, &p, now, decisions)
	reqs := placementRequirements(&p, decisions)

	// Nodes that left the pool stop counting; their prepare pods go, and so
	// do pods preparing under requirements that no longer apply.
	if err := r.deletePreparePods(ctx, &p, targets, reqs); err != nil {
		return ctrl.Result{}, err
	}
	var nodes []sandboxv1alpha1.PlacementNodeStatus
	requeue := placementResync
	for _, name := range sortedKeys(targets) {
		ns, after, err := r.reconcileNode(ctx, &p, name, targets[name], authorized, decisions, reqs, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		nodes = append(nodes, ns)
		if after > 0 && after < requeue {
			requeue = after
		}
	}
	kept, err := r.keptNodes(ctx, &p, orig.Nodes, targets, reqs, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	nodes = append(nodes, kept...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	p.Status.Nodes = nodes

	reason, msg := sandboxv1alpha1.PlacementReasonPreparing, ""
	switch {
	case refused != "" && deniedAny(decisions):
		reason, msg = sandboxv1alpha1.PlacementReasonPolicyDenied, refused
	case refused != "":
		reason, msg = sandboxv1alpha1.PlacementReasonArtifactRefused, refused
	case len(targets) == 0:
		reason, msg = sandboxv1alpha1.PlacementReasonNoTargetNodes, fmt.Sprintf("no node runs a slot of pool %s", p.Spec.PoolRef.Name)
	}
	r.summarize(&p, reason, msg)
	return r.finish(ctx, &p, orig, requeue)
}

// targetNodes maps each node running a slot of the placement's pool to one
// of its slot pods (for the tolerations and image pull Secrets the node's
// launcher pods use).
//
// The targets are the nodes of the pool's running slot pods: warm (idle or
// still booting, once scheduled) and claimed, that is a slot checked out by a
// SwiftSandbox whose workload still runs there. A claimed slot keeps the pool
// label, so a one-slot pool keeps its node while the checkout runs, and the
// replacement slot adds its own node once scheduled. A slot leaves the set
// when its pod finishes or is deleted.
func (r *ArtifactPlacementReconciler) targetNodes(ctx context.Context, p *sandboxv1alpha1.SwiftArtifactPlacement) (map[string]*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(p.Namespace), client.MatchingLabels{PoolLabelKey: p.Spec.PoolRef.Name}); err != nil {
		return nil, err
	}
	out := map[string]*corev1.Pod{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil || podFinished(pod) || !isPoolSlot(pod, p.Spec.PoolRef.Name) {
			continue
		}
		if _, ok := out[pod.Spec.NodeName]; !ok {
			out[pod.Spec.NodeName] = pod
		}
	}
	return out, nil
}

// keptNodes returns the prepared nodes that no longer run a slot of the pool,
// as NotATarget entries, while what was found there still holds: prepared
// under this generation and these requirements, less than placementRecheck
// ago (the time a target node is trusted without a check), and the node still
// in the cluster. A one-slot pool's refill often lands on the node its
// previous slot left; forgetting that node made the placement prepare it again
// and report Preparing in the meantime, although its cache held everything.
// Kept entries are not targets: they are not counted, get no prepare pod and
// do not affect the Prepared condition, and they go once their time is up.
func (r *ArtifactPlacementReconciler) keptNodes(ctx context.Context, p *sandboxv1alpha1.SwiftArtifactPlacement, prev []sandboxv1alpha1.PlacementNodeStatus, targets map[string]*corev1.Pod, reqs string, now time.Time) ([]sandboxv1alpha1.PlacementNodeStatus, error) {
	var out []sandboxv1alpha1.PlacementNodeStatus
	for _, n := range prev {
		if _, ok := targets[n.Name]; ok {
			continue
		}
		if !n.Prepared || n.PreparedAt == nil || now.Sub(n.PreparedAt.Time) >= placementRecheck ||
			n.ObservedGeneration != p.Generation || n.Requirements != reqs {
			continue
		}
		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: n.Name}, &node); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		n.Reason = sandboxv1alpha1.PlacementReasonNotATarget
		n.Message = "no slot of the pool runs here; kept as prepared until its recheck would be due"
		out = append(out, n)
	}
	return out, nil
}

// isPoolSlot reports whether a pod labelled with the pool is one of its slots:
// controlled by the pool (a warm slot) or, once claimed, by the SwiftSandbox
// that checked it out. The label alone is not enough: anyone who can label a
// pod in the namespace could otherwise add its node to the targets.
func isPoolSlot(pod *corev1.Pod, pool string) bool {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return false
	}
	if gv, err := schema.ParseGroupVersion(ref.APIVersion); err != nil || gv.Group != sandboxv1alpha1.GroupName {
		return false
	}
	switch ref.Kind {
	case "SwiftSandboxPool":
		return ref.Name == pool
	case "SwiftSandbox":
		return pod.Labels[SlotStateLabelKey] == slotStateClaimed && ref.Name == pod.Labels[SandboxLabelKey]
	}
	return false
}

// authorize looks every artifact up with its own credentials through the
// shared resolver: only an artifact the registry serves to them is prepared,
// and the lookup keeps that authorization fresh for checkouts that use the
// same credentials. It returns the artifacts authorized at their digest, and
// a message when any was refused.
func (r *ArtifactPlacementReconciler) authorize(ctx context.Context, p *sandboxv1alpha1.SwiftArtifactPlacement, now time.Time, decisions []artifactpolicy.Decision) (map[string]bool, string) {
	view := placementView(p, now)
	prev := map[string]sandboxv1alpha1.PlacementArtifactStatus{}
	for _, a := range p.Status.Artifacts {
		prev[a.Name] = a
	}
	status := make([]sandboxv1alpha1.PlacementArtifactStatus, len(p.Spec.Artifacts))
	authorized := map[string]bool{}
	var refused []string
	reqs, err := artifactRequests(ctx, r.Client, view)
	if err == nil {
		// A pull Secret that cannot be used refuses the placement, as before
		// v0.19.2. The verify-key check artifactRequests makes for optional
		// sandbox artifacts does not apply here (the view marks every
		// artifact optional): an unusable key fails only that artifact's
		// prepare, as it always has.
		for i := range reqs {
			if reqs[i].credReason == reasonVerifyKeyInvalid {
				reqs[i].credErr, reqs[i].credReason = nil, ""
				continue
			}
			if reqs[i].credErr != nil {
				err = reqs[i].credErr
				break
			}
		}
	}
	if err != nil {
		// A missing or unreadable pull Secret: nothing can be authorized.
		for i, a := range p.Spec.Artifacts {
			status[i] = sandboxv1alpha1.PlacementArtifactStatus{Name: a.Name, Reason: sandboxv1alpha1.PlacementReasonArtifactRefused, Message: truncateMessage(err.Error())}
		}
		p.Status.Artifacts = status
		return authorized, truncateMessage(err.Error())
	}
	lookup := r.lookup(p)
	for i, req := range reqs {
		if i >= len(decisions) || decisions[i].Denied == "" {
			prime(lookup, req)
		}
	}
	for i, a := range p.Spec.Artifacts {
		st := sandboxv1alpha1.PlacementArtifactStatus{Name: a.Name, PreparedNodes: prev[a.Name].PreparedNodes}
		if i < len(decisions) {
			st.Policies = decisions[i].Policies
			if d := decisions[i]; d.Denied != "" {
				// Refused by a policy: never asked of the registry.
				st.Reason, st.Message = sandboxv1alpha1.PlacementReasonPolicyDenied, truncateMessage(d.Denied+": "+d.Message)
				if prev[a.Name].Reason != sandboxv1alpha1.PlacementReasonPolicyDenied {
					recordPlacementPolicyDenial(d.Denied)
					r.Recorder.Eventf(p, corev1.EventTypeWarning, "ArtifactPolicyDenied", "artifact %s: %s", a.Name, st.Message)
				}
				refused = append(refused, a.Name+": "+st.Message)
				status[i] = st
				continue
			}
		}
		res, done := lookup(reqs[i])
		switch {
		case !done:
			st.Reason, st.Message = sandboxv1alpha1.PlacementReasonResolving, "waiting for the registry to answer"
		case res.err != nil && registryRefused(res.err):
			st.Reason, st.Message = sandboxv1alpha1.PlacementReasonArtifactRefused, truncateMessage(res.err.Error())
			refused = append(refused, a.Name+": "+st.Message)
		case res.err != nil:
			st.Reason, st.Message = sandboxv1alpha1.PlacementReasonResolving, truncateMessage(res.err.Error())
		case res.digest != digestOf(a.Ref):
			// A digest reference answers with its own digest; anything else
			// is not the artifact the placement names.
			st.Reason, st.Message = sandboxv1alpha1.PlacementReasonArtifactRefused,
				fmt.Sprintf("the registry served %s for %s", res.digest, a.Ref)
			refused = append(refused, a.Name+": "+st.Message)
		default:
			st.Reason, st.Digest = sandboxv1alpha1.PlacementReasonAuthorized, res.digest
			authorized[a.Name] = true
		}
		status[i] = st
	}
	p.Status.Artifacts = status
	return authorized, strings.Join(refused, "; ")
}

// digestOf is the digest a digest reference pins.
func digestOf(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[i+1:]
	}
	return ""
}

// lookup is the resolver lookup for p: a request that is not answered within
// the inline wait requeues p when it finishes.
func (r *ArtifactPlacementReconciler) lookup(p *sandboxv1alpha1.SwiftArtifactPlacement) lookupFunc {
	res := r.resolver
	if res == nil {
		res = sharedResolver
	}
	w := waiter{obj: &sandboxv1alpha1.SwiftArtifactPlacement{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name}}}
	if r.resolved != nil {
		w.ch = r.resolved
	} else {
		w.obj = nil
	}
	var deadline time.Time
	seen := map[resolveKey]bool{}
	return func(req resolveRequest) (resolveResult, bool) {
		req.namespace = p.Namespace
		if deadline.IsZero() {
			deadline = res.now().Add(res.inlineWait)
		}
		req.deadline = deadline
		req.followUp = req.followUp || seen[req.key]
		seen[req.key] = true
		return res.get(req, w)
	}
}

// reconcileNode moves one node toward holding every authorized artifact and
// returns its status, and when the placement should look at it again.
func (r *ArtifactPlacementReconciler) reconcileNode(ctx context.Context, p *sandboxv1alpha1.SwiftArtifactPlacement, node string, slot *corev1.Pod, authorized map[string]bool, decisions []artifactpolicy.Decision, reqs string, now time.Time) (sandboxv1alpha1.PlacementNodeStatus, time.Duration, error) {
	prev := nodeStatus(p, node)
	st := sandboxv1alpha1.PlacementNodeStatus{Name: node, ObservedGeneration: p.Generation, Requirements: reqs}
	// A node prepared under other platform requirements (a policy changed
	// its keys or claims, or now governs an artifact) is prepared again.
	if prev != nil && prev.ObservedGeneration == p.Generation && prev.Requirements == reqs {
		st.PreparedAt, st.LastFailureTime = prev.PreparedAt, prev.LastFailureTime
		st.Prepared, st.Reason, st.Message = prev.Prepared, prev.Reason, prev.Message
		st.Failures = prev.Failures
		if prev.Reason == sandboxv1alpha1.PlacementReasonNotATarget {
			// A kept node a slot returned to: prepared, as it was.
			st.Reason, st.Message = sandboxv1alpha1.PlacementReasonPrepared, ""
		}
	}
	want := wantedArtifacts(p, authorized)

	var pod corev1.Pod
	podName := preparePodName(p, node, reqs)
	err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: podName}, &pod)
	if err != nil && !apierrors.IsNotFound(err) {
		return st, 0, err
	}
	if err == nil && !metav1.IsControlledBy(&pod, p) {
		// Only a pod this placement created is read for a result.
		st.Prepared, st.Reason = false, sandboxv1alpha1.PlacementReasonPrepareFailed
		st.Message = fmt.Sprintf("pod %s exists and was not created for this placement", podName)
		return st, placementResync, nil
	}
	if err == nil {
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			r.readPrepareResult(p, &st, &pod, want)
			if err := r.Delete(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
				return st, 0, err
			}
			return st, time.Second, nil
		default:
			// A node already prepared (under these requirements) stays so
			// while its recheck runs: only a recheck that fails unprepares
			// it. Reporting it unprepared for the length of every recheck
			// told consumers to skip artifacts the node holds.
			if !st.Prepared {
				st.Reason, st.Message = sandboxv1alpha1.PlacementReasonPreparing, ""
			}
			return st, placementResync, nil
		}
	}

	// No prepare pod. A prepared node stays so until its recheck; a failed
	// one waits out its backoff; otherwise one is started.
	if len(want) == 0 {
		st.Prepared = false
		st.Reason, st.Message = sandboxv1alpha1.PlacementReasonPreparing, "waiting for the registry to authorize the artifacts"
		return st, placementResync, nil
	}
	if st.Prepared && st.PreparedAt != nil && now.Sub(st.PreparedAt.Time) < placementRecheck {
		return st, st.PreparedAt.Add(placementRecheck).Sub(now), nil
	}
	if !st.Prepared && st.LastFailureTime != nil {
		if wait := backoffFor(st.Failures) - now.Sub(st.LastFailureTime.Time); wait > 0 {
			return st, wait, nil
		}
	}
	pp := preparePod(p, node, slot, want, reqs, now)
	if err := addArtifactRequirements(pp, decisionsByName(placementView(p, now), decisions)); err != nil {
		return st, 0, err
	}
	if err := controllerutil.SetControllerReference(p, pp, r.Scheme); err != nil {
		return st, 0, err
	}
	if err := r.Create(ctx, pp); err != nil && !apierrors.IsAlreadyExists(err) {
		return st, 0, err
	}
	if !st.Prepared {
		st.Reason, st.Message = sandboxv1alpha1.PlacementReasonPreparing, ""
	}
	return st, placementResync, nil
}

// readPrepareResult records a finished prepare pod on st: the node is
// prepared when every wanted artifact's container prepared it.
func (r *ArtifactPlacementReconciler) readPrepareResult(p *sandboxv1alpha1.SwiftArtifactPlacement, st *sandboxv1alpha1.PlacementNodeStatus, pod *corev1.Pod, want []string) {
	failed := map[string]string{}
	reason := sandboxv1alpha1.PlacementReasonPrepareFailed
	var latest time.Time
	pending := false
	for _, name := range want {
		cs := containerStatus(pod, artifactInitPrefix+name)
		switch {
		case !podHasContainer(pod, artifactInitPrefix+name):
			// Authorized after this pod was built: the next pod prepares it.
			// Not a failure of this node.
			pending = true
		case cs == nil || cs.State.Terminated == nil:
			failed[name] = firstNonEmpty(pod.Status.Message, pod.Status.Reason, "did not run")
		case cs.State.Terminated.ExitCode != 0:
			failed[name] = firstNonEmpty(strings.TrimSpace(cs.State.Terminated.Message), "materialize failed")
		default:
			res, ok := artifactResult(cs)
			if !ok {
				failed[name] = "no result"
			} else if res.Skipped != nil {
				failed[name] = res.Skipped.Reason + ": " + res.Skipped.Message
				switch res.Skipped.Reason {
				case "VerificationFailed":
					reason = sandboxv1alpha1.PlacementReasonVerificationFailed
				case "ClaimMismatch":
					if reason != sandboxv1alpha1.PlacementReasonVerificationFailed {
						reason = sandboxv1alpha1.PlacementReasonClaimMismatch
					}
				}
			}
			if t := cs.State.Terminated.FinishedAt.Time; t.After(latest) {
				latest = t
			}
		}
	}
	if len(failed) == 0 && pending {
		st.Prepared = false
		st.Reason, st.Message = sandboxv1alpha1.PlacementReasonPreparing, ""
		return
	}
	if len(failed) == 0 {
		at := metav1.NewTime(latest)
		if latest.IsZero() {
			at = metav1.NewTime(r.clock())
		}
		if !st.Prepared {
			r.Recorder.Eventf(p, corev1.EventTypeNormal, "Prepared", "node %s holds every artifact", st.Name)
		}
		st.Prepared, st.PreparedAt, st.LastFailureTime, st.Failures = true, &at, nil, 0
		st.Reason, st.Message = sandboxv1alpha1.PlacementReasonPrepared, ""
		metrics.ArtifactPreparesTotal.WithLabelValues("prepared").Inc()
		if st.Requirements != "" {
			metrics.ArtifactPolicyDecisionsTotal.WithLabelValues("placement", "verified").Inc()
		}
		return
	}
	var parts []string
	for _, name := range sortedKeys(failed) {
		parts = append(parts, name+": "+failed[name])
	}
	at := metav1.NewTime(r.clock())
	if st.Prepared || st.LastFailureTime == nil {
		r.Recorder.Eventf(p, corev1.EventTypeWarning, reason, "node %s: %s", st.Name, truncateMessage(strings.Join(parts, "; ")))
	}
	st.Prepared, st.LastFailureTime = false, &at
	st.Failures++
	st.Reason, st.Message = reason, truncateMessage(strings.Join(parts, "; "))
	switch reason {
	case sandboxv1alpha1.PlacementReasonVerificationFailed:
		metrics.ArtifactPreparesTotal.WithLabelValues("verification_failed").Inc()
		if st.Requirements != "" {
			metrics.ArtifactPolicyDecisionsTotal.WithLabelValues("placement", "verification_failed").Inc()
		}
	case sandboxv1alpha1.PlacementReasonClaimMismatch:
		metrics.ArtifactPreparesTotal.WithLabelValues("claim_mismatch").Inc()
		metrics.ArtifactPolicyDecisionsTotal.WithLabelValues("placement", "claim_mismatch").Inc()
	default:
		metrics.ArtifactPreparesTotal.WithLabelValues("failed").Inc()
	}
}

// backoffFor doubles from placementBackoff with each consecutive failure on
// the node (recorded in its status, so it survives a controller restart), up
// to placementMaxBackoff.
func backoffFor(failures int32) time.Duration {
	d := placementBackoff
	for i := int32(1); i < failures && d < placementMaxBackoff; i++ {
		d *= 2
	}
	return min(d, placementMaxBackoff)
}

// wantedArtifacts are the authorized artifacts of p, in spec order.
func wantedArtifacts(p *sandboxv1alpha1.SwiftArtifactPlacement, authorized map[string]bool) []string {
	var out []string
	for _, a := range p.Spec.Artifacts {
		if authorized[a.Name] {
			out = append(out, a.Name)
		}
	}
	return out
}

// preparePod builds the pod that prepares want on node: the checkout fetch
// pod's containers, one per artifact, pinned to the node.
func preparePod(p *sandboxv1alpha1.SwiftArtifactPlacement, node string, slot *corev1.Pod, want []string, reqs string, now time.Time) *corev1.Pod {
	view := placementView(p, now)
	only := view.DeepCopy()
	only.Spec.Artifacts = nil
	for _, a := range view.Spec.Artifacts {
		if slices.Contains(want, a.Name) {
			only.Spec.Artifacts = append(only.Spec.Artifacts, a)
			only.Status.Artifacts = append(only.Status.Artifacts, sandboxv1alpha1.SandboxArtifactStatus{
				Name: a.Name, Digest: digestOf(a.Ref), MountPath: a.MountPath})
		}
	}
	containers, volumes := artifactInits(only, false)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        preparePodName(p, node, reqs),
			Namespace:   p.Namespace,
			Labels:      map[string]string{placementLabel: p.Name},
			Annotations: map[string]string{placementNodeAnnotation: node},
		},
		Spec: corev1.PodSpec{
			NodeName:      node,
			RestartPolicy: corev1.RestartPolicyNever,
			// Pinned by node name, so no scheduler gives up on it: the deadline
			// ends a pod whose image pull stalls.
			ActiveDeadlineSeconds:        ptr.To(int64((placementPrepareTimeout + 2*time.Minute) / time.Second)),
			Containers:                   containers,
			Volumes:                      volumes,
			Tolerations:                  slot.Spec.Tolerations,
			ImagePullSecrets:             slot.Spec.ImagePullSecrets,
			AutomountServiceAccountToken: ptr.To(false),
		},
	}
}

// preparePodName is unique to the placement (its UID), the node, the spec
// generation and the platform requirements (reqs): a changed spec or policy
// gets a new pod, an unchanged one finds its own.
func preparePodName(p *sandboxv1alpha1.SwiftArtifactPlacement, node, reqs string) string {
	key := fmt.Sprintf("%s/%s/%d", p.UID, node, p.Generation)
	if reqs != "" {
		key += "/" + reqs
	}
	sum := sha256.Sum256([]byte(key))
	suffix := "-prep-" + hex.EncodeToString(sum[:])[:10]
	base := p.Name
	if max := 63 - len(suffix); len(base) > max {
		base = strings.TrimRight(base[:max], "-.")
	}
	return base + suffix
}

// deletePreparePods deletes p's prepare pods for nodes not in keep, and
// those of an older spec generation. keep nil deletes every one.
func (r *ArtifactPlacementReconciler) deletePreparePods(ctx context.Context, p *sandboxv1alpha1.SwiftArtifactPlacement, keep map[string]*corev1.Pod, reqs string) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(p.Namespace), client.MatchingLabels{placementLabel: p.Name}); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, p) {
			continue
		}
		node := pod.Annotations[placementNodeAnnotation]
		if _, ok := keep[node]; ok && pod.Name == preparePodName(p, node, reqs) {
			continue
		}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// summarize fills the counts and the Prepared condition. reason and msg are
// used when not every target node is prepared (or there is none).
func (r *ArtifactPlacementReconciler) summarize(p *sandboxv1alpha1.SwiftArtifactPlacement, reason, msg string) {
	p.Status.ObservedGeneration = p.Generation
	p.Status.TargetNodes = 0
	p.Status.PreparedNodes = 0
	count := map[string]int32{}
	var failing []string
	for _, n := range p.Status.Nodes {
		if n.Reason == sandboxv1alpha1.PlacementReasonNotATarget {
			continue // kept, not a target
		}
		p.Status.TargetNodes++
		if n.Prepared {
			p.Status.PreparedNodes++
			for _, a := range p.Spec.Artifacts {
				count[a.Name]++
			}
		} else if n.Reason == sandboxv1alpha1.PlacementReasonPrepareFailed || n.Reason == sandboxv1alpha1.PlacementReasonVerificationFailed ||
			n.Reason == sandboxv1alpha1.PlacementReasonClaimMismatch {
			failing = append(failing, n.Name+": "+n.Message)
			if reason == sandboxv1alpha1.PlacementReasonPreparing {
				reason = n.Reason
			}
		}
	}
	for i := range p.Status.Artifacts {
		p.Status.Artifacts[i].PreparedNodes = count[p.Status.Artifacts[i].Name]
	}
	cond := metav1.Condition{Type: sandboxv1alpha1.PlacementConditionPrepared, ObservedGeneration: p.Generation}
	switch {
	case p.Status.TargetNodes > 0 && p.Status.PreparedNodes == p.Status.TargetNodes:
		cond.Status, cond.Reason = metav1.ConditionTrue, sandboxv1alpha1.PlacementReasonPrepared
		cond.Message = fmt.Sprintf("%d/%d pool nodes hold every artifact", p.Status.PreparedNodes, p.Status.TargetNodes)
	default:
		cond.Status, cond.Reason = metav1.ConditionFalse, reason
		cond.Message = msg
		if cond.Message == "" {
			cond.Message = fmt.Sprintf("%d/%d pool nodes hold every artifact", p.Status.PreparedNodes, p.Status.TargetNodes)
			if len(failing) > 0 {
				cond.Message += "; " + strings.Join(failing, "; ")
			}
		}
		cond.Message = truncateMessage(cond.Message)
	}
	apimeta.SetStatusCondition(&p.Status.Conditions, cond)
}

// finish writes the status when it changed and requeues after: a merge patch
// of what changed, made against the resourceVersion Reconcile read (#758
// moved off a full update; the placement is now read uncached, so a conflict
// means someone else wrote it in between).
func (r *ArtifactPlacementReconciler) finish(ctx context.Context, p *sandboxv1alpha1.SwiftArtifactPlacement, orig *sandboxv1alpha1.SwiftArtifactPlacementStatus, after time.Duration) (ctrl.Result, error) {
	if !apiequality.Semantic.DeepEqual(orig, &p.Status) {
		base := p.DeepCopy()
		base.Status = *orig
		// Locked: status.nodes is a list a merge patch replaces whole, so a
		// write from an older read must fail (and be retried) rather than
		// put back what it read.
		if err := r.Status().Patch(ctx, p, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

func podHasContainer(pod *corev1.Pod, name string) bool {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return true
		}
	}
	return false
}

func nodeStatus(p *sandboxv1alpha1.SwiftArtifactPlacement, node string) *sandboxv1alpha1.PlacementNodeStatus {
	for i := range p.Status.Nodes {
		if p.Status.Nodes[i].Name == node {
			return &p.Status.Nodes[i]
		}
	}
	return nil
}

func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == name {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// placementsForPod maps a pool slot pod to the placements of its pool, and a
// prepare pod to its placement.
func (r *ArtifactPlacementReconciler) placementsForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	if name := obj.GetLabels()[placementLabel]; name != "" {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
	}
	pool := obj.GetLabels()[PoolLabelKey]
	if pool == "" {
		return nil
	}
	return r.placementsForPool(ctx, obj.GetNamespace(), pool)
}

func (r *ArtifactPlacementReconciler) placementsForPool(ctx context.Context, namespace, pool string) []reconcile.Request {
	var list sandboxv1alpha1.SwiftArtifactPlacementList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		log.FromContext(ctx).Error(err, "list placements")
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.PoolRef.Name == pool {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return out
}

func (r *ArtifactPlacementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.resolved = make(chan event.GenericEvent, resolvedEventBuffer)
	return ctrl.NewControllerManagedBy(mgr).
		Named("sandbox-artifact-placement").
		For(&sandboxv1alpha1.SwiftArtifactPlacement{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.placementsForPod)).
		Watches(&sandboxv1alpha1.SwiftSandboxPool{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			return r.placementsForPool(ctx, obj.GetNamespace(), obj.GetName())
		})).
		WatchesRawSource(source.Channel(r.resolved, &handler.EnqueueRequestForObject{})).
		// A policy or a namespace's labels change what placements must verify.
		Watches(&sandboxv1alpha1.SwiftArtifactVerificationPolicy{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
			return r.placementsIn(ctx, "")
		})).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			return r.placementsIn(ctx, obj.GetName())
		})).
		Complete(r)
}

// placementsIn lists the placements in namespace ("" for every namespace).
func (r *ArtifactPlacementReconciler) placementsIn(ctx context.Context, namespace string) []reconcile.Request {
	var list sandboxv1alpha1.SwiftArtifactPlacementList
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := r.List(ctx, &list, opts...); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return out
}

// placementArtifactInputs are p's artifacts as the policy decision reads them.
func placementArtifactInputs(p *sandboxv1alpha1.SwiftArtifactPlacement) []artifactpolicy.Artifact {
	out := make([]artifactpolicy.Artifact, len(p.Spec.Artifacts))
	for i, a := range p.Spec.Artifacts {
		out[i] = artifactpolicy.Artifact{Ref: a.Ref, Claims: a.Claims, RequirePlatformVerification: a.RequirePlatformVerification}
	}
	return out
}

func deniedAny(decisions []artifactpolicy.Decision) bool {
	for i := range decisions {
		if decisions[i].Denied != "" {
			return true
		}
	}
	return false
}

// placementRequirements identifies the platform verification p's artifacts
// are prepared under: a hash of each governed artifact's acceptable records,
// or "" when no policy governs any of them. It depends on the policies only,
// never on how far the registry has answered, so a prepare pod is not
// replaced while authorizations arrive.
func placementRequirements(p *sandboxv1alpha1.SwiftArtifactPlacement, decisions []artifactpolicy.Decision) string {
	h := sha256.New()
	governed := false
	for i, a := range p.Spec.Artifacts {
		if i >= len(decisions) || !decisions[i].Governed() || decisions[i].Denied != "" {
			continue
		}
		recs, err := decisions[i].AcceptableRecords(digestOf(a.Ref))
		if err != nil {
			continue
		}
		governed = true
		fmt.Fprintf(h, "%s\x00", a.Name)
		for _, g := range recs {
			fmt.Fprintf(h, "%s\x00", strings.Join(g, ","))
		}
		fmt.Fprint(h, "\x01")
	}
	if !governed {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// recordPlacementPolicyDenial counts a placement artifact a policy refused.
func recordPlacementPolicyDenial(why string) {
	result := "denied"
	if why == artifactpolicy.DeniedNoMatchingPolicy {
		result = "no_policy"
	}
	metrics.ArtifactPolicyDecisionsTotal.WithLabelValues("placement", result).Inc()
}

// placedKey identifies a prepared cache entry the way a checkout needs it:
// read from this repository at this digest, in this layout, verified with
// the key in this Secret (none when empty), under the platform requirements
// these claims resolve to.
type placedKey struct {
	repository, digest, layout, key, claims string
}

// claimsKey is an artifact's claims and requirePlatformVerification, which
// together with the namespace decide its platform requirements.
func claimsKey(claims map[string]string, requirePlatform bool) string {
	var b strings.Builder
	for _, k := range sortedKeys(claims) {
		b.WriteString(k + "=" + claims[k] + "\x00")
	}
	if requirePlatform {
		b.WriteString("!")
	}
	return b.String()
}

// preparedNodes are the nodes where the placements of sb's pool have
// prepared every artifact sb mounts. Empty when sb mounts none or no
// placement covers them. A hint for picking a slot: the launcher still checks
// the node's entry before it mounts it.
func (r *SwiftSandboxReconciler) preparedNodes(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) map[string]bool {
	arts := resolvedArtifacts(sb)
	if len(arts) == 0 || sb.Spec.PoolRef == nil {
		return nil
	}
	var list sandboxv1alpha1.SwiftArtifactPlacementList
	if err := r.List(ctx, &list, client.InNamespace(sb.Namespace)); err != nil || len(list.Items) == 0 {
		return nil
	}
	byNode := map[string]map[placedKey]bool{}
	for i := range list.Items {
		p := &list.Items[i]
		if p.Spec.PoolRef.Name != sb.Spec.PoolRef.Name || p.DeletionTimestamp != nil {
			continue
		}
		authorized := map[string]bool{}
		for _, a := range p.Status.Artifacts {
			if a.Reason == sandboxv1alpha1.PlacementReasonAuthorized {
				authorized[a.Name] = true
			}
		}
		// Only a node prepared under the requirements that apply now counts.
		decisions, err := r.policies.decide(ctx, r.Client, p.Namespace, placementArtifactInputs(p))
		if err != nil {
			continue
		}
		reqs := placementRequirements(p, decisions)
		for _, n := range p.Status.Nodes {
			if !n.Prepared || n.ObservedGeneration != p.Generation || n.Requirements != reqs {
				continue
			}
			for _, a := range p.Spec.Artifacts {
				if !authorized[a.Name] {
					continue
				}
				if byNode[n.Name] == nil {
					byNode[n.Name] = map[placedKey]bool{}
				}
				byNode[n.Name][placedKey{repoOf(a.Ref), digestOf(a.Ref), a.ArtifactLayout(), secretRefName(a.VerifyKeySecretRef),
					claimsKey(a.Claims, a.RequirePlatformVerification)}] = true
			}
		}
	}
	out := map[string]bool{}
	for node, have := range byNode {
		all := true
		for _, ra := range arts {
			if !have[placedKey{repoOf(ra.spec.Ref), ra.digest, ra.spec.ArtifactLayout(), secretRefName(ra.spec.VerifyKeySecretRef),
				claimsKey(ra.spec.Claims, ra.spec.RequirePlatformVerification)}] {
				all = false
				break
			}
		}
		if all {
			out[node] = true
		}
	}
	return out
}

// repoOf is ref's repository, as the cache records where an entry was read
// from.
func repoOf(ref string) string {
	r, err := name.ParseReference(ref)
	if err != nil {
		return ""
	}
	return r.Context().Name()
}

func secretRefName(s *sandboxv1alpha1.SecretObjectReference) string {
	if s == nil {
		return ""
	}
	return s.Name
}
