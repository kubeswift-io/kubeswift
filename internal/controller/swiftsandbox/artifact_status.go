package swiftsandbox

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
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
		}
	}
}

// artifactSkipped reports a skipped optional artifact.
func (r *SwiftSandboxReconciler) artifactSkipped(sb *sandboxv1alpha1.SwiftSandbox, s *sandboxv1alpha1.SandboxArtifactStatus, path string) {
	metrics.SandboxArtifactsTotal.WithLabelValues(path, "true", "skipped", s.Reason).Inc()
	if r.Recorder == nil {
		return
	}
	event := "ArtifactSkipped"
	if s.Reason == "VerificationFailed" {
		event = "ArtifactVerificationFailed"
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
