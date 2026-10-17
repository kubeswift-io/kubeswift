package swiftsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/artifactpolicy"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// Platform artifact verification (SwiftArtifactVerificationPolicy,
// docs/sandbox/artifact-verification-policy.md).
//
// Every path that mounts or prepares an artifact asks the same question,
// with the current policies, before it fetches: which policies govern this
// artifact in this namespace, and what do they require (artifactpolicy).
// A refusal fails a required artifact (ArtifactPolicyDenied) or skips an
// optional one (PolicyDenied). Otherwise the requirements go to the
// materializer in the pod spec (cold launcher, fetch pod, prepare pod),
// which verifies them and records which key met each; a warm checkout sends
// the acceptable records to the launcher, which mounts an entry only when
// every requirement has one.

// launcherFeaturesAnnotation is written by swiftletd on a launcher: what it
// enforces (rust/swiftletd/src/report.rs). A governed artifact is sent only
// to a slot that lists artifactRequirementsFeature; an older launcher would
// ignore the requirements.
const (
	launcherFeaturesAnnotation  = "kubeswift.io/launcher-features"
	artifactRequirementsFeature = "artifact-requirements"
)

// slotEnforcesRequirements reports whether a slot's launcher checks
// platform requirement records.
func slotEnforcesRequirements(p *corev1.Pod) bool {
	for _, f := range strings.Fields(p.Annotations[launcherFeaturesAnnotation]) {
		if f == artifactRequirementsFeature {
			return true
		}
	}
	return false
}

// artifactPolicies evaluates the cluster's policies for a namespace's
// artifacts, compiling each policy once per generation.
type artifactPolicies struct {
	cache artifactpolicy.Cache
}

// decide returns one decision per artifact, in order. Errors (policies or
// the namespace cannot be read) are returned, never treated as "no policy".
func (p *artifactPolicies) decide(ctx context.Context, c client.Reader, namespace string, arts []artifactpolicy.Artifact) ([]artifactpolicy.Decision, error) {
	if len(arts) == 0 {
		return nil, nil
	}
	var list sandboxv1alpha1.SwiftArtifactVerificationPolicyList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list artifact verification policies: %w", err)
	}
	// With no policy at all only the artifact's own requests apply
	// (requirePlatformVerification, claims), which need no namespace.
	var ns corev1.Namespace
	if len(list.Items) > 0 {
		if err := c.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
			return nil, fmt.Errorf("namespace %s: %w", namespace, err)
		}
	}
	compiled := p.cache.Compiled(list.Items)
	out := make([]artifactpolicy.Decision, len(arts))
	for i, a := range arts {
		out[i] = artifactpolicy.Decide(compiled, namespace, ns.Labels, a)
	}
	return out, nil
}

// sandboxArtifactInputs are sb's artifacts as the decision reads them.
func sandboxArtifactInputs(sb *sandboxv1alpha1.SwiftSandbox) []artifactpolicy.Artifact {
	out := make([]artifactpolicy.Artifact, len(sb.Spec.Artifacts))
	for i, a := range sb.Spec.Artifacts {
		out[i] = artifactpolicy.Artifact{Ref: a.Ref, Claims: a.Claims, RequirePlatformVerification: a.RequirePlatformVerification}
	}
	return out
}

// sandboxDecisions decides sb's artifacts with the current policies.
func (r *SwiftSandboxReconciler) sandboxDecisions(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) ([]artifactpolicy.Decision, error) {
	return r.policies.decide(ctx, r.Client, sb.Namespace, sandboxArtifactInputs(sb))
}

// decisionsByName pairs sb's artifacts with their decisions.
func decisionsByName(sb *sandboxv1alpha1.SwiftSandbox, decisions []artifactpolicy.Decision) map[string]*artifactpolicy.Decision {
	out := map[string]*artifactpolicy.Decision{}
	for i := range decisions {
		if i < len(sb.Spec.Artifacts) {
			out[sb.Spec.Artifacts[i].Name] = &decisions[i]
		}
	}
	return out
}

// anyGoverned reports whether a policy governs any of sb's artifacts that a
// checkout will mount.
func anyGoverned(sb *sandboxv1alpha1.SwiftSandbox, decisions []artifactpolicy.Decision) bool {
	by := decisionsByName(sb, decisions)
	for _, a := range artifactsToMount(sb) {
		if d := by[a.Name]; d != nil && d.Governed() {
			return true
		}
	}
	return false
}

// policyDeniedError is an artifact a policy refuses outright.
type policyDeniedError struct {
	artifact, why, msg string
}

func (e policyDeniedError) Error() string {
	return fmt.Sprintf("artifact %s: %s: %s", e.artifact, e.why, e.msg)
}

func isPolicyDenied(err error) bool {
	var pe policyDeniedError
	return errors.As(err, &pe)
}

// addArtifactRequirements gives each artifact materializer in pod the
// platform requirements of its artifact, and a writable home for cosign.
// The requirements travel in the immutable pod spec, never in a file the
// tenant could replace.
func addArtifactRequirements(pod *corev1.Pod, by map[string]*artifactpolicy.Decision) error {
	needHome := false
	for _, list := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range list {
			c := &list[i]
			name, ok := strings.CutPrefix(c.Name, artifactInitPrefix)
			if !ok {
				continue
			}
			d := by[name]
			if d == nil || len(d.Requirements) == 0 {
				continue
			}
			raw, err := json.Marshal(d.Requirements)
			if err != nil {
				return err
			}
			c.Env = append(c.Env, corev1.EnvVar{Name: materialize.RequirementsEnv, Value: string(raw)})
			if !hasEnv(c, "HOME") {
				c.Env = append(c.Env, corev1.EnvVar{Name: "HOME", Value: "/cosign-home"}, corev1.EnvVar{Name: "TMPDIR", Value: "/cosign-home"})
				c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: artifactCosignHomeName, MountPath: "/cosign-home"})
				needHome = true
			}
		}
	}
	if needHome && !podHasVolume(pod, artifactCosignHomeName) {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: artifactCosignHomeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	}
	return nil
}

func hasEnv(c *corev1.Container, name string) bool {
	for _, e := range c.Env {
		if e.Name == name {
			return true
		}
	}
	return false
}

func podHasVolume(pod *corev1.Pod, name string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// policyFailureReason maps a materializer's failure message to the
// sandbox's reason: a platform requirement it did not meet, or def.
func policyFailureReason(msg, def string) string {
	switch {
	case strings.Contains(msg, materialize.RequirementClaimMismatch+": policy "):
		return "ArtifactClaimMismatch"
	case strings.Contains(msg, materialize.RequirementVerificationFailed+": policy "):
		return "ArtifactVerificationFailed"
	}
	return def
}

// recordPolicyOutcome counts what platform verification decided for an
// artifact, once it is known: verified (mounted while governed), a
// verification failure, a claim mismatch, no policy where one was required,
// or another refusal. Bounded labels only.
func recordPolicyOutcome(s *sandboxv1alpha1.SandboxArtifactStatus, path string) {
	var result string
	switch {
	case s.Reason == "PolicyDenied" || s.Reason == "ArtifactPolicyDenied":
		result = "denied"
		if strings.HasPrefix(s.Message, artifactpolicy.DeniedNoMatchingPolicy) || strings.Contains(s.Message, ": "+artifactpolicy.DeniedNoMatchingPolicy+":") {
			result = "no_policy"
		}
	case len(s.Policies) == 0:
		return
	case s.State == sandboxv1alpha1.SandboxArtifactMounted:
		result = "verified"
	case s.Reason == "ClaimMismatch" || s.Reason == "ArtifactClaimMismatch":
		result = "claim_mismatch"
	case s.Reason == "VerificationFailed" || s.Reason == "ArtifactVerificationFailed":
		result = "verification_failed"
	default:
		return
	}
	metrics.ArtifactPolicyDecisionsTotal.WithLabelValues(path, result).Inc()
}

// artifactArgsFailure is the sandbox's reason when the exec action's
// artifacts cannot be built.
func artifactArgsFailure(err error) string {
	if isPolicyDenied(err) {
		return "ArtifactPolicyDenied"
	}
	return "ArtifactResolveFailed"
}
