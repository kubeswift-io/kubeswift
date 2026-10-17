package swiftsandbox

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// recordArtifactStatuses sets sb's status.artifacts to statuses (one per
// artifact, spec order) and reports each artifact that became Skipped: a
// Warning Event and a count, once per transition.
func (r *SwiftSandboxReconciler) recordArtifactStatuses(sb *sandboxv1alpha1.SwiftSandbox, statuses []sandboxv1alpha1.SandboxArtifactStatus, path string) {
	before := map[string]sandboxv1alpha1.SandboxArtifactState{}
	for _, s := range sb.Status.Artifacts {
		before[s.Name] = s.State
	}
	sb.Status.Artifacts = statuses
	for i := range statuses {
		s := &statuses[i]
		if s.State == sandboxv1alpha1.SandboxArtifactSkipped && before[s.Name] != sandboxv1alpha1.SandboxArtifactSkipped {
			r.artifactSkipped(sb, s, path)
		}
	}
}

// setArtifactState moves one artifact's status to state, reporting a skip
// as recordArtifactStatuses does. Unknown names are ignored.
func (r *SwiftSandboxReconciler) setArtifactState(sb *sandboxv1alpha1.SwiftSandbox, name string, state sandboxv1alpha1.SandboxArtifactState, reason, msg, path string) {
	for i := range sb.Status.Artifacts {
		s := &sb.Status.Artifacts[i]
		if s.Name != name || s.State == state {
			continue
		}
		s.State, s.Reason, s.Message = state, reason, truncateMessage(msg)
		switch state {
		case sandboxv1alpha1.SandboxArtifactSkipped:
			r.artifactSkipped(sb, s, path)
		case sandboxv1alpha1.SandboxArtifactMounted:
			metrics.SandboxArtifactsTotal.WithLabelValues(path, boolLabel(artifactOptional(sb, name)), "mounted", reason).Inc()
			recordPolicyOutcome(s, path)
		case sandboxv1alpha1.SandboxArtifactFailed:
			metrics.SandboxArtifactsTotal.WithLabelValues(path, boolLabel(artifactOptional(sb, name)), "failed", reason).Inc()
			recordPolicyOutcome(s, path)
		}
	}
}

// failArtifacts records the required artifacts that fail sb as Failed with
// the sandbox's reason. A message "artifact <name>: ..." names one; else
// every name in names fails.
func (r *SwiftSandboxReconciler) failArtifacts(sb *sandboxv1alpha1.SwiftSandbox, names []string, reason, msg, path string) {
	if rest, ok := strings.CutPrefix(msg, "artifact "); ok {
		if name, _, ok := strings.Cut(rest, ":"); ok && artifactStatus(sb, name) != nil {
			names = []string{name}
		}
	}
	for _, name := range names {
		if !artifactOptional(sb, name) {
			r.setArtifactState(sb, name, sandboxv1alpha1.SandboxArtifactFailed, reason, msg, path)
		}
	}
}

// recordFailedResolve records a refused required artifact's statuses.
func (r *SwiftSandboxReconciler) recordFailedResolve(sb *sandboxv1alpha1.SwiftSandbox, res artifactResolution, path string) {
	if len(res.failed) == 0 {
		return
	}
	sb.Status.Artifacts = res.failed
	for i := range res.failed {
		if s := &res.failed[i]; s.State == sandboxv1alpha1.SandboxArtifactFailed {
			metrics.SandboxArtifactsTotal.WithLabelValues(path, "false", "failed", s.Reason).Inc()
			recordPolicyOutcome(s, path)
		}
	}
}

// artifactSkipped reports a skipped optional artifact.
func (r *SwiftSandboxReconciler) artifactSkipped(sb *sandboxv1alpha1.SwiftSandbox, s *sandboxv1alpha1.SandboxArtifactStatus, path string) {
	metrics.SandboxArtifactsTotal.WithLabelValues(path, "true", "skipped", s.Reason).Inc()
	recordPolicyOutcome(s, path)
	if r.Recorder == nil {
		return
	}
	event := "ArtifactSkipped"
	switch s.Reason {
	case "VerificationFailed":
		event = "ArtifactVerificationFailed"
	case "ClaimMismatch":
		event = "ArtifactClaimMismatch"
	case "PolicyDenied":
		event = "ArtifactPolicyDenied"
	}
	r.Recorder.Eventf(sb, corev1.EventTypeWarning, event,
		"optional artifact %s is not mounted (%s): %s; the workload starts without it", s.Name, s.Reason, s.Message)
}

// artifactOptional reports whether spec names the artifact optional.
func artifactOptional(sb *sandboxv1alpha1.SwiftSandbox, name string) bool {
	for i := range sb.Spec.Artifacts {
		if sb.Spec.Artifacts[i].Name == name {
			return sb.Spec.Artifacts[i].Optional
		}
	}
	return false
}

// boolLabel is a metric label for b.
func boolLabel(b bool) string { return strconv.FormatBool(b) }

// recordColdArtifacts records, once the launcher runs, how each artifact of a
// cold start was delivered: from its init container's result, mounted (from
// the node cache or fetched now) or, for an optional one, skipped and why.
func (r *SwiftSandboxReconciler) recordColdArtifacts(sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) {
	for i := range pod.Status.InitContainerStatuses {
		cs := &pod.Status.InitContainerStatuses[i]
		name, ok := strings.CutPrefix(cs.Name, artifactInitPrefix)
		if !ok {
			continue
		}
		res, ok := artifactResult(cs)
		if !ok {
			continue
		}
		recordCacheReport(pod.Spec.NodeName, res)
		if res.Skipped != nil {
			r.setArtifactState(sb, name, sandboxv1alpha1.SandboxArtifactSkipped, res.Skipped.Reason, res.Skipped.Message, "cold")
			continue
		}
		reason := "Fetched"
		if res.CacheHit {
			reason = "Cached"
		}
		r.setArtifactState(sb, name, sandboxv1alpha1.SandboxArtifactMounted, reason, "", "cold")
	}
}

// recordCacheReport exports a node artifact cache's state from a
// materializer result (only runs with limits set report one).
func recordCacheReport(node string, res materialize.Result) {
	c := res.Cache
	if c == nil || node == "" {
		return
	}
	metrics.SandboxArtifactCacheBytes.WithLabelValues(node, "used").Set(float64(c.Bytes))
	metrics.SandboxArtifactCacheBytes.WithLabelValues(node, "limit").Set(float64(c.LimitBytes))
	if c.Evicted > 0 {
		metrics.SandboxArtifactCacheEvictionsTotal.WithLabelValues(node).Add(float64(c.Evicted))
	}
	over := 0.0
	if c.OverLimit {
		over = 1
	}
	metrics.SandboxArtifactCacheOverLimit.WithLabelValues(node).Set(over)
}
