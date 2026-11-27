// Package podmode is the pod-mode mutating webhook: a raw admission handler
// on Pod CREATE that rewrites a Pod naming the kubeswift-microvm RuntimeClass
// into an unprivileged stand-in (internal/sandbox/podmode). The controller
// then runs the workload in a SwiftSandbox microVM.
//
// The MutatingWebhookConfiguration is failurePolicy Fail, scoped to namespaces
// labelled kubeswift.io/pod-mode=enabled and, by a match condition, to Pods
// naming the RuntimeClass. When the webhook cannot run, such a Pod is refused;
// when it was skipped, the pod-mode admission policy refuses a Pod still
// naming the RuntimeClass; and when both are bypassed, the RuntimeClass's
// handler exists on no node, so the Pod never starts.
//
// The handler reads only from the controller's cache: a Pod resuming a
// suspended agent-sandbox Sandbox is pinned to the node holding the
// microVM's saved state. A failed read leaves the Pod unpinned (it may then
// cold-boot elsewhere); it never refuses the Pod.
package podmode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

// Path is where the handler is registered on the webhook server.
const Path = "/mutate-pods-podmode"

// Handler rewrites pod-mode Pods into stand-ins.
type Handler struct {
	// ShimImage returns the image the stand-in runs (sandbox-materialize).
	ShimImage func() string
	// Reader finds suspended microVMs (cached); nil pins nothing.
	Reader client.Reader
	// Namespace holds the pod-mode SwiftSandboxes.
	Namespace string
}

// Handle implements admission.Handler.
func (h *Handler) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create || req.SubResource != "" {
		return admission.Allowed("")
	}
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if !podmode.Wants(&pod) {
		return admission.Allowed("")
	}
	out, err := podmode.Rewrite(&pod, h.ShimImage())
	if err != nil {
		var refused *podmode.RefusedError
		if errors.As(err, &refused) {
			return admission.Denied("kubeswift pod mode: " + refused.Error())
		}
		return admission.Errored(http.StatusInternalServerError, err)
	}
	ns := pod.Namespace
	if ns == "" {
		ns = req.Namespace
	}
	if node := h.resumeNode(ctx, out, ns); node != "" {
		podmode.PinToNode(out, node)
		logf.FromContext(ctx).Info("pinned a pod-mode stand-in to its suspended microVM's node",
			"namespace", ns, "name", pod.Name, "node", node)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	logf.FromContext(ctx).V(1).Info("rewrote pod into a pod-mode stand-in",
		"namespace", req.Namespace, "name", pod.Name, "generateName", pod.GenerateName)
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}

// resumeNode is the node holding the saved state of the suspended microVM the
// Pod will resume: a Suspended pod-mode SwiftSandbox of a stand-in with the
// Pod's namespace and name, suspended for the agent-sandbox Sandbox that owns
// the Pod. "" when there is none or the node is gone.
func (h *Handler) resumeNode(ctx context.Context, pod *corev1.Pod, namespace string) string {
	ref := podmode.AgentSandboxOwner(pod)
	if h.Reader == nil || ref == nil || pod.Name == "" || pod.Spec.NodeName != "" {
		return ""
	}
	log := logf.FromContext(ctx)
	var list sandboxv1alpha1.SwiftSandboxList
	if err := h.Reader.List(ctx, &list, client.InNamespace(h.Namespace), client.MatchingLabels{
		podmode.LabelStandInNamespace: namespace, podmode.LabelOwnerUID: string(ref.UID),
	}); err != nil {
		log.Error(err, "listing suspended pod-mode microVMs")
		return ""
	}
	for i := range list.Items {
		sb := &list.Items[i]
		st := sb.Status.SuspendedState
		if sb.Annotations[podmode.AnnotationStandIn] != namespace+"/"+pod.Name || sb.DeletionTimestamp != nil ||
			!sb.Spec.Suspended() || sb.Status.Phase != sandboxv1alpha1.SwiftSandboxSuspended || st == nil || st.NodeName == "" {
			continue
		}
		var node corev1.Node
		if err := h.Reader.Get(ctx, types.NamespacedName{Name: st.NodeName}, &node); err != nil {
			log.Info("not pinning a resuming stand-in: its microVM's node is unavailable", "node", st.NodeName, "error", err.Error())
			return ""
		}
		return st.NodeName
	}
	return ""
}
