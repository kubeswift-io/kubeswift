// Package eviction is the Phase 4 drain-integration eviction webhook.
//
// It is a raw admission handler on pods/eviction (NOT a CRD validator) that
// makes `kubectl drain` (and any eviction-API caller — cluster-autoscaler,
// node upgrades) safely evacuate SwiftGuest VMs instead of killing them.
//
// For a SwiftGuest launcher pod the handler, per the guest's drain policy:
//   - migratable (drainPolicy Migrate/LiveMigrate, migration.enabled != false)
//     → stamps the kubeswift.io/drain-requested marker on the SwiftGuest
//     (skipping the patch on dry-run) and DENIES the eviction with 429
//     TooManyRequests so drain retries every 5s. The Phase 4 drain
//     controller reads the marker and creates the SwiftMigration; once the
//     migration's cutover Deletes the source pod, the next eviction retry
//     finds the pod gone and is allowed — drain proceeds.
//   - drainPolicy Block, migration.enabled=false, or LiveMigrate on a
//     VFIO/GPU guest → DENIES (429) with a manual-handling message and does
//     NOT stamp a marker (no auto-migration).
//   - not a SwiftGuest launcher pod, or pod already gone → ALLOWS.
//
// The marker is inert until the drain controller ships (Phase 4 PR 4): this
// PR wires the webhook only. The webhook is registered with
// failurePolicy: Ignore so a webhook outage never breaks cluster-wide
// evictions; the per-guest PodDisruptionBudget (PR 4) is the hard floor that
// protects the VM when the webhook is down.
package eviction

import (
	"context"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// guestLabelKey marks a SwiftGuest launcher pod. Matches the label written
// by the SwiftGuest controller's pod builder (swiftguest/pod.go).
const guestLabelKey = "swift.kubeswift.io/guest"

// Handler is the pods/eviction admission handler.
type Handler struct {
	Client client.Client
}

// Handle implements admission.Handler. For pods/eviction the AdmissionRequest
// Name/Namespace identify the pod being evicted (the policy/v1 Eviction
// object is named after the pod), so the handler fetches the pod by that
// name rather than decoding the Eviction body.
func (h *Handler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)

	var pod corev1.Pod
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			// Pod gone (a prior eviction succeeded, or a migration cutover
			// already deleted it): nothing to protect — let drain proceed.
			return admission.Allowed("pod not found")
		}
		// Transient read error: deny-with-retry (429) rather than a 500 so
		// drain keeps looping safely instead of hard-failing. (failurePolicy
		// Ignore covers webhook-CALL failures at the apiserver; this is a
		// handler-internal read error, which we choose to surface as
		// retryable so we never accidentally allow a guest eviction.)
		return denyRetry(fmt.Sprintf("transient error reading pod %s/%s; retry", req.Namespace, req.Name))
	}

	guestName := pod.Labels[guestLabelKey]
	if guestName == "" {
		return admission.Allowed("not a SwiftGuest launcher pod")
	}

	var guest swiftv1alpha1.SwiftGuest
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: guestName}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			// Labeled pod but no SwiftGuest CR (orphaned/already-deleted
			// guest): nothing to migrate and no policy to honor — allow the
			// eviction so the orphan is cleaned up and drain proceeds.
			return admission.Allowed(fmt.Sprintf("SwiftGuest %q not found; nothing to protect", guestName))
		}
		return denyRetry(fmt.Sprintf("transient error reading SwiftGuest %q; retry", guestName))
	}

	node := pod.Spec.NodeName

	// Resolve the per-guest migration enablement + drain policy (defaults:
	// enabled=true, drainPolicy=Migrate).
	enabled := true
	policy := swiftv1alpha1.DrainPolicyMigrate
	if guest.Spec.Migration != nil {
		if guest.Spec.Migration.Enabled != nil {
			enabled = *guest.Spec.Migration.Enabled
		}
		if guest.Spec.Migration.DrainPolicy != "" {
			policy = guest.Spec.Migration.DrainPolicy
		}
	}

	// migration.enabled=false is the strongest pin: no auto-migration, no
	// marker — deny with a manual-handling message.
	if !enabled {
		return denyRetry(fmt.Sprintf(
			"SwiftGuest %q has migration disabled (migration.enabled=false); it will not be auto-migrated off node %q — drain it manually or set migration.enabled=true",
			guestName, node))
	}

	switch policy {
	case swiftv1alpha1.DrainPolicyBlock:
		return denyRetry(fmt.Sprintf(
			"SwiftGuest %q has drainPolicy=Block; it will not be auto-migrated off node %q — handle this guest manually",
			guestName, node))
	case swiftv1alpha1.DrainPolicyLiveMigrate:
		// LiveMigrate forbids incurring downtime. A VFIO/GPU guest can only
		// move OFFLINE (CH cannot live-migrate a VFIO device), so block the
		// drain rather than evacuate it with downtime — the operator opted out
		// of offline by choosing LiveMigrate. Non-VFIO LiveMigrate falls
		// through to mark (live).
		if guest.HasVFIODevices() {
			return denyRetry(fmt.Sprintf(
				"SwiftGuest %q has drainPolicy=LiveMigrate but uses VFIO/GPU devices that cannot live-migrate; it will not be evacuated off node %q — set drainPolicy=Migrate to allow offline (release-and-reallocate) migration, or handle it manually",
				guestName, node))
		}
	case swiftv1alpha1.DrainPolicyMigrate:
		// Migratable: non-VFIO resolves to live where possible; VFIO/GPU
		// resolves to OFFLINE via the release-and-reallocate path. Fall
		// through to mark.
	default:
		// Unknown/unvalidated policy: treat as Migrate (the CRD enum should
		// have rejected anything else at admission of the SwiftGuest).
	}

	// SR-IOV NIC passthrough cannot be auto-migrated: the GPU release-and-
	// reallocate path handles GPUs only; reattaching an SR-IOV NIC on the
	// target is out of scope. Deny without marking (a marker would drive a
	// migration that fails for lack of a GPU profile). Reaches here only for
	// Migrate/unknown policy (LiveMigrate already denied any VFIO above).
	if guest.HasSRIOVInterface() {
		return denyRetry(fmt.Sprintf(
			"SwiftGuest %q uses SR-IOV NIC passthrough that cannot be auto-migrated off node %q — handle it manually",
			guestName, node))
	}

	// Migratable. Stamp the drain-requested marker (skip on dry-run) and
	// deny-with-retry. The Phase 4 drain controller reads the marker and
	// creates the SwiftMigration; until then the marker is inert.
	dryRun := req.DryRun != nil && *req.DryRun
	if !dryRun {
		if err := h.markDrainRequested(ctx, &guest, node); err != nil {
			log.Error(err, "failed to stamp drain-requested marker; denying (retry)",
				"guest", guestName, "node", node)
			// Still deny (retry): the marker write can succeed on the next 5s
			// eviction retry. Never allow the VM to be evicted-to-death
			// because a marker patch flaked.
			return denyRetry(fmt.Sprintf(
				"SwiftGuest %q: could not record drain request (will retry)", guestName))
		}
	}
	return denyRetry(fmt.Sprintf(
		"SwiftGuest %q on node %q is being migrated off before eviction; retry", guestName, node))
}

// markDrainRequested idempotently stamps the drain-requested marker (the
// draining node name) on the SwiftGuest via a merge patch. No-op when the
// marker already records this node.
func (h *Handler) markDrainRequested(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, node string) error {
	if guest.Annotations[swiftv1alpha1.AnnotationDrainRequested] == node {
		return nil
	}
	patch := client.MergeFrom(guest.DeepCopy())
	if guest.Annotations == nil {
		guest.Annotations = map[string]string{}
	}
	guest.Annotations[swiftv1alpha1.AnnotationDrainRequested] = node
	return h.Client.Patch(ctx, guest, patch)
}

// denyRetry denies an eviction with 429 TooManyRequests so kubectl drain
// treats it as retryable (the same status the eviction API returns for a
// PodDisruptionBudget block) and retries every 5s rather than hard-failing.
func denyRetry(msg string) admission.Response {
	return admission.Response{
		AdmissionResponse: admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Status:  metav1.StatusFailure,
				Code:    http.StatusTooManyRequests,
				Reason:  metav1.StatusReasonTooManyRequests,
				Message: msg,
			},
		},
	}
}
