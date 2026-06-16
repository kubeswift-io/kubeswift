package swiftguest

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// Identity-action annotation keys — the Go side of swiftletd's IDENTITY_KEYS
// (rust/swiftletd/src/action.rs). The controller writes the action keys; it
// reads the status keys back as swiftletd's mirror.
const (
	identityActionKey     = "kubeswift.io/identity-action"
	identityActionIDKey   = "kubeswift.io/identity-action-id"
	identityActionArgsKey = "kubeswift.io/identity-action-args"
	identityStatusKey     = "kubeswift.io/identity-status"
	identityStatusIDKey   = "kubeswift.io/identity-status-id"
	identityStatusDetail  = "kubeswift.io/identity-status-detail"

	identityVerbRegenerate = "regenerate"
)

// identityRegenArgs is the JSON the controller writes to identityActionArgsKey;
// it matches swiftletd's IdentityRegenArgs (camelCase). Items map verbatim from
// SwiftGuest.spec.cloneFromSnapshot.regenerate.
type identityRegenArgs struct {
	Items      []string `json:"items,omitempty"`
	MAC        string   `json:"mac,omitempty"`
	Hostname   string   `json:"hostname,omitempty"`
	RenewLease bool     `json:"renewLease,omitempty"`
}

// cloneIdentityActionID returns a stable per-guest action-id (immutable across
// reconciles, mirrors resumeActionID): the regen fires exactly once per clone.
func cloneIdentityActionID(guest *swiftv1alpha1.SwiftGuest) string {
	uid := string(guest.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return guest.Name + "-" + uid + "-identity"
}

// ensureCloneIdentityRegen drives the one-shot in-guest identity regeneration
// for a cloneFromSnapshot guest whose SOURCE opted into the agent. It stamps the
// identity-action on the launcher pod once the clone is Running, reads
// swiftletd's status mirror, and maps it to the CloneIdentityRegenerated
// condition. Returns a requeue delay while the action is in flight.
//
// LOAD-BEARING: the agent must already be RUNNING in the clone's resumed RAM
// (it was captured from the source). If it is not (source had no agent / device
// absent / not yet up), swiftletd reports GuestAgentUnreachable and the clone
// keeps the source's identity — a loud, status-visible fallback, never silent.
func (r *SwiftGuestReconciler) ensureCloneIdentityRegen(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	pod *corev1.Pod,
	status *swiftv1alpha1.SwiftGuestStatus,
) (time.Duration, error) {
	// Only for an agent-enabled clone with a running launcher.
	if !guest.UsesCloneFromSnapshot() || guest.Annotations[AnnotationCloneAgentEnabled] != "true" {
		return 0, nil
	}
	if pod == nil || status.Phase != swiftv1alpha1.SwiftGuestPhaseRunning || !isLauncherReady(pod) {
		return 0, nil
	}

	actionID := cloneIdentityActionID(guest)
	annos := pod.GetAnnotations()

	// First pass: stamp the action (idempotent — swiftletd dedupes by action-id).
	if annos[identityActionIDKey] != actionID {
		args := identityRegenArgs{
			Items:      cloneRegenItems(guest.Spec.CloneFromSnapshot),
			MAC:        primaryCloneMAC(guest),
			Hostname:   guest.Name,
			RenewLease: true,
		}
		argsJSON, err := json.Marshal(args)
		if err != nil {
			return 0, err
		}
		if err := r.patchPodIdentityAction(ctx, pod, actionID, string(argsJSON)); err != nil {
			return 0, err
		}
		SetCloneIdentityRegeneratedCondition(status, false, "Regenerating",
			"identity-regen action sent to launcher pod "+pod.Name)
		return 3 * time.Second, nil
	}

	// Action stamped — read swiftletd's status mirror.
	if annos[identityStatusIDKey] != actionID {
		// mirror not yet visible
		SetCloneIdentityRegeneratedCondition(status, false, "Regenerating",
			"waiting for in-guest agent to acknowledge")
		return 3 * time.Second, nil
	}
	detail := annos[identityStatusDetail]
	switch annos[identityStatusKey] {
	case "running", "":
		SetCloneIdentityRegeneratedCondition(status, false, "Regenerating",
			"in-guest agent is regenerating identity")
		return 3 * time.Second, nil
	case "ready":
		SetCloneIdentityRegeneratedCondition(status, true, "", detail)
		return 0, nil
	case "failed":
		// swiftletd reports "GuestAgentUnreachable: ..." when the agent does not
		// answer (absent / device-less / timeout); surface that reason verbatim.
		reason := "RegenFailed"
		if strings.Contains(detail, "GuestAgentUnreachable") {
			reason = "GuestAgentUnreachable"
		}
		SetCloneIdentityRegeneratedCondition(status, false, reason, detail)
		return 0, nil
	case "rejected":
		SetCloneIdentityRegeneratedCondition(status, false, "RegenRejected", detail)
		return 0, nil
	default:
		SetCloneIdentityRegeneratedCondition(status, false, "RegenFailed",
			"unexpected identity-status: "+annos[identityStatusKey])
		return 0, nil
	}
}

// patchPodIdentityAction merge-patches the identity-action annotations onto the
// launcher pod. Mirrors swiftrestore's patchPodActionAnnotations.
func (r *SwiftGuestReconciler) patchPodIdentityAction(
	ctx context.Context, pod *corev1.Pod, actionID, argsJSON string,
) error {
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				identityActionKey:     identityVerbRegenerate,
				identityActionIDKey:   actionID,
				identityActionArgsKey: argsJSON,
			},
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return r.Patch(ctx, pod, client.RawPatch(types.MergePatchType, data))
}

// cloneRegenItems maps spec.cloneFromSnapshot.regenerate to the agent's item
// strings. An empty list is passed through empty — the agent defaults to all
// four (matching the CRD's "empty defaults to all").
func cloneRegenItems(src *swiftv1alpha1.CloneFromSnapshotSource) []string {
	if src == nil || len(src.Regenerate) == 0 {
		return nil
	}
	items := make([]string, 0, len(src.Regenerate))
	for _, it := range src.Regenerate {
		items = append(items, string(it))
	}
	return items
}

// primaryCloneMAC returns the per-clone MAC for the guest's primary interface —
// the first entry of the AnnotationRestoreMACRewrites CSV the clone path already
// computed (so the guest-visible MAC the agent sets matches the host-side
// config.net[0].mac that was rewritten). Empty if unavailable (the agent then
// skips macAddresses but still re-DHCPs).
func primaryCloneMAC(guest *swiftv1alpha1.SwiftGuest) string {
	csv := guest.Annotations[AnnotationRestoreMACRewrites]
	if csv == "" {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(csv, ",", 2)[0])
}
