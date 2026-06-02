package swiftmigration

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftguest"
)

// W-3c-1 / TFU #24 — freeze the destination runtime intent to lifecycle:run.
//
// The destination launcher pod is built by srcPod.DeepCopy() (newDstPod),
// so it inherits the source pod's runtime-intent volume pointing at the
// LIVE, controller-managed `<guest>-runtime-intent` ConfigMap. If the
// SwiftGuest is stopped (runPolicy: Stopped) while a migration is in flight
// — the canonical stop-during-migration race that Phase 4 drain makes
// reachable — the SwiftGuest controller rewrites that CM to lifecycle:stop,
// and swiftletd's launch gate (main.rs:201) honors `lifecycle == "stop"`
// for ALL launch paths, INCLUDING migration_receiver_mode (the receiver
// role is an env var, not an exemption). The dst receiver would then skip
// its launch and the migration's receive would fail.
//
// The fix: the dst pod mounts a FROZEN per-migration intent ConfigMap with
// lifecycle forced to "start", which the SwiftGuest controller never
// writes (it manages only `<guest>-runtime-intent`). A later flip of the
// live CM to lifecycle:stop cannot reach the dst receiver.
//
// Post-cutover correctness is unaffected: swiftletd reads the intent once
// at boot, so a post-migration stop takes effect via pod deletion (graceful
// stop), not via re-reading the intent — the frozen CM is a boot-time-only
// artifact. Pod recreations after the migration use the live CM
// (guest.Name pods mount `<guest>-runtime-intent`).

// runtimeIntentVolumeName is the launcher pod's runtime-intent ConfigMap
// volume name (matches swiftguest/pod.go's literal). The dst pod inherits
// it via DeepCopy; the freeze repoints it.
const runtimeIntentVolumeName = "runtime-intent"

// repointRuntimeIntentVolume sets the pod's runtime-intent ConfigMap volume
// to mount frozenCMName instead of the inherited live
// `<guest>-runtime-intent`. No-op if the pod has no such volume (defensive;
// every launcher pod has one).
func repointRuntimeIntentVolume(pod *corev1.Pod, frozenCMName string) {
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		if v.Name == runtimeIntentVolumeName && v.ConfigMap != nil {
			v.ConfigMap.Name = frozenCMName
			return
		}
	}
}

// frozenDstIntentCMName returns the deterministic name of the frozen
// per-migration intent ConfigMap for a given destination pod. Keyed off the
// dst pod name (itself deterministic from the SwiftMigration UID), so
// re-entry on leader handover ensures/repoints the same CM.
func frozenDstIntentCMName(dstPodName string) string {
	return dstPodName + swiftguest.IntentConfigMapSuffix
}

// ensureFrozenDstIntent mints (idempotently) the frozen destination intent
// ConfigMap: a copy of the guest's live `<guest>-runtime-intent` with the
// runtime-intent.json `lifecycle` field forced to "start". Owned by the
// SwiftGuest (GC'd on guest delete). Returns the frozen CM name for
// newDstPod to repoint the dst pod's runtime-intent volume at.
//
// Must be called BEFORE the dst pod is created (the pod mounts this CM).
func (r *SwiftMigrationReconciler) ensureFrozenDstIntent(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	dstPodName string,
) (string, error) {
	liveName := guest.Name + swiftguest.IntentConfigMapSuffix
	var live corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: guest.Namespace, Name: liveName}, &live); err != nil {
		if apierrors.IsNotFound(err) {
			// No live intent CM to copy (shouldn't happen for a running
			// guest). Skip the freeze gracefully: return "" so newDstPod
			// leaves the inherited volume in place (pre-freeze behaviour).
			// The freeze is defense-in-depth; not blocking the migration on
			// a missing CM is preferable, and the inherited volume points
			// at the same name anyway.
			logf.FromContext(ctx).Info("frozen dst intent skipped: live runtime-intent CM not found",
				"configMap", liveName, "guest", guest.Name)
			return "", nil
		}
		return "", fmt.Errorf("get live runtime-intent ConfigMap %q: %w", liveName, err)
	}

	frozenData, err := forceLifecycleStart(live.Data)
	if err != nil {
		return "", fmt.Errorf("freeze runtime intent lifecycle: %w", err)
	}

	frozenName := frozenDstIntentCMName(dstPodName)
	key := types.NamespacedName{Namespace: guest.Namespace, Name: frozenName}
	var existing corev1.ConfigMap
	gerr := r.Get(ctx, key, &existing)
	if gerr == nil {
		if equality.Semantic.DeepEqual(existing.Data, frozenData) {
			return frozenName, nil
		}
		existing.Data = frozenData
		if uerr := r.Update(ctx, &existing); uerr != nil {
			return "", fmt.Errorf("update frozen intent ConfigMap %q: %w", frozenName, uerr)
		}
		return frozenName, nil
	}
	if !apierrors.IsNotFound(gerr) {
		return "", fmt.Errorf("get frozen intent ConfigMap %q: %w", frozenName, gerr)
	}

	frozen := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      frozenName,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kubeswift",
				"app.kubernetes.io/component":  "migration",
				"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
			},
		},
		Data: frozenData,
	}
	if serr := controllerutil.SetControllerReference(guest, frozen, r.Scheme); serr != nil {
		return "", fmt.Errorf("set ownerRef on frozen intent ConfigMap %q: %w", frozenName, serr)
	}
	if cerr := r.Create(ctx, frozen); cerr != nil {
		if apierrors.IsAlreadyExists(cerr) {
			return frozenName, nil
		}
		return "", fmt.Errorf("create frozen intent ConfigMap %q: %w", frozenName, cerr)
	}
	return frozenName, nil
}

// forceLifecycleStart returns a copy of the intent ConfigMap data with the
// runtime-intent.json `lifecycle` field set to "start" (the canonical run
// value; swiftletd's gate only skips on "stop"). Other keys/fields are
// preserved verbatim. The intent is parsed as a generic object so this does
// not depend on the full RuntimeIntent struct shape.
func forceLifecycleStart(data map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(data))
	for k, v := range data {
		out[k] = v
	}
	raw, ok := out[swiftguest.IntentFile]
	if !ok {
		return nil, fmt.Errorf("live intent ConfigMap missing key %q", swiftguest.IntentFile)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil, fmt.Errorf("unmarshal %q: %w", swiftguest.IntentFile, err)
	}
	obj["lifecycle"] = json.RawMessage(`"start"`)
	frozen, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal frozen intent: %w", err)
	}
	out[swiftguest.IntentFile] = string(frozen)
	return out, nil
}
