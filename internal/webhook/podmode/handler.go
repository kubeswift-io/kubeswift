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
// The handler does no network I/O: everything it needs is in the request.
package podmode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

// Path is where the handler is registered on the webhook server.
const Path = "/mutate-pods-podmode"

// Handler rewrites pod-mode Pods into stand-ins.
type Handler struct {
	// ShimImage returns the image the stand-in runs (sandbox-materialize).
	ShimImage func() string
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
	raw, err := json.Marshal(out)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	logf.FromContext(ctx).V(1).Info("rewrote pod into a pod-mode stand-in",
		"namespace", req.Namespace, "name", pod.Name, "generateName", pod.GenerateName)
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}
