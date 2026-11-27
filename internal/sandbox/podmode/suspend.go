package podmode

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// AgentSandboxGroup and AgentSandboxKind are the kubernetes-sigs
	// agent-sandbox Sandbox. It suspends by deleting the Pod it owns and
	// resumes by creating a Pod of the same name; pod mode keeps the microVM
	// across the two (docs/sandbox/pod-mode.md).
	AgentSandboxGroup = "agents.x-k8s.io"
	AgentSandboxKind  = "Sandbox"
	// AgentSandboxSuspended is the Sandbox's spec.operatingMode when it
	// wants its Pod gone.
	AgentSandboxSuspended = "Suspended"
)

// AgentSandboxOwner is the Pod's controller owner when that is an
// agent-sandbox Sandbox, else nil.
func AgentSandboxOwner(pod *corev1.Pod) *metav1.OwnerReference {
	ref := metav1.GetControllerOf(pod)
	if ref == nil || ref.Kind != AgentSandboxKind || ref.Name == "" || ref.UID == "" {
		return nil
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil || gv.Group != AgentSandboxGroup {
		return nil
	}
	return ref
}

// PinToNode adds a required node affinity for the node named node to the
// Pod, on top of any it has (every required term must then also match it).
func PinToNode(pod *corev1.Pod, node string) {
	req := corev1.NodeSelectorRequirement{Key: metav1.ObjectNameField, Operator: corev1.NodeSelectorOpIn, Values: []string{node}}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil ||
		len(na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{req}}},
		}
		return
	}
	terms := na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	for i := range terms {
		terms[i].MatchFields = append(terms[i].MatchFields, req)
	}
}
