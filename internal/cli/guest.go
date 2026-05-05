package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

const (
	GuestLabelKey     = "swift.kubeswift.io/guest"
	LauncherContainer = "launcher"
)

// GuestResolver resolves SwiftGuest and its pod.
type GuestResolver struct {
	client.Client
}

// GuestInfo holds resolved guest and pod info.
type GuestInfo struct {
	Guest *swiftv1alpha1.SwiftGuest
	Pod   *corev1.Pod
}

// GuestID returns the runtime directory guest-id (namespace-name).
// Matches swift-runtime sanitization: slashes replaced with hyphens.
func GuestID(namespace, name string) string {
	return strings.ReplaceAll(namespace+"/"+name, "/", "-")
}

// ResolveGuest fetches the SwiftGuest by name in the given namespace.
func (r *GuestResolver) ResolveGuest(ctx context.Context, namespace, name string) (*swiftv1alpha1.SwiftGuest, error) {
	var guest swiftv1alpha1.SwiftGuest
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &guest)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("swiftguest %q not found in namespace %q", name, namespace)
		}
		return nil, err
	}
	return &guest, nil
}

// ResolvePod fetches the pod for the given SwiftGuest.
//
// Resolution order:
//  1. status.podRef (authoritative when present and reachable). The
//     SwiftGuest controller patches this at cutover step 1 of a live
//     migration, so post-migration it points at the destination pod
//     name (typically <guest>-mig-<uid>). Pre-migration it equals
//     <guest>.
//  2. Label selector swift.kubeswift.io/guest=<guest.Name> as a
//     fallback. Triggered when (a) podRef is nil (pre-controller-
//     stamp transient, e.g., very early after SwiftGuest create), or
//     (b) podRef is set but the named pod returns NotFound (cutover
//     race — podRef has been patched to dst but dst pod is not yet
//     created, OR src pod was already deleted but this controller's
//     status patch is still in flight).
//
// When the label selector returns multiple pods (chain-migration
// transient: M1 src still Terminating while M2 dst is Running, both
// labeled with the same guest name), pods are stable-sorted with the
// best match first by:
//
//  1. Non-Terminating before Terminating (DeletionTimestamp == nil wins)
//  2. Running before non-Running (Status.Phase == Running wins)
//  3. Newest CreationTimestamp first (latest wins on tie)
//
// The first item after the sort is returned. If every candidate is
// Terminating, the newest one is returned with a stderr warning so
// the operator at least gets something to inspect; this matches the
// "best-effort" posture of swiftctl during transient cluster states.
//
// Other Get errors (timeout, auth, conflict) propagate immediately.
// Only NotFound triggers the label-selector fallback.
//
// TODO: route the all-Terminating warning through structured logging
// when internal/cli grows a logger; today it prints to stderr to
// avoid scope creep.
func (r *GuestResolver) ResolvePod(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*corev1.Pod, error) {
	namespace := guest.Namespace
	name := guest.Name

	if guest.Status.PodRef != nil {
		var pod corev1.Pod
		err := r.Get(ctx, client.ObjectKey{
			Namespace: guest.Status.PodRef.Namespace,
			Name:      guest.Status.PodRef.Name,
		}, &pod)
		if err == nil {
			return &pod, nil
		}
		if !errors.IsNotFound(err) {
			return nil, err
		}
		// NotFound: fall through to the label-selector path. The
		// named pod is gone (cutover race) but a labeled candidate
		// may still resolve.
	}

	sel := labels.SelectorFromSet(map[string]string{GuestLabelKey: name})
	var list corev1.PodList
	if err := r.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("pod for guest %q not found", name)
	}

	pods := list.Items
	sort.SliceStable(pods, func(i, j int) bool {
		ti := pods[i].DeletionTimestamp != nil
		tj := pods[j].DeletionTimestamp != nil
		if ti != tj {
			// Non-Terminating wins.
			return !ti
		}
		ri := pods[i].Status.Phase == corev1.PodRunning
		rj := pods[j].Status.Phase == corev1.PodRunning
		if ri != rj {
			// Running wins.
			return ri
		}
		// Tie-break by newest CreationTimestamp.
		return pods[i].CreationTimestamp.After(pods[j].CreationTimestamp.Time)
	})

	// All-Terminating warn path: surface a hint that what we return
	// is actively being deleted; operator may need to wait for the
	// next reconcile or pick a different action.
	allTerminating := true
	for i := range pods {
		if pods[i].DeletionTimestamp == nil {
			allTerminating = false
			break
		}
	}
	if allTerminating {
		fmt.Fprintf(os.Stderr,
			"warning: all %d candidate pod(s) for guest %q are Terminating; returning newest (%q). The operation may fail; retry once the next launcher pod is Running.\n",
			len(pods), name, pods[0].Name)
	}

	return &pods[0], nil
}

// Resolve fetches both guest and pod.
func (r *GuestResolver) Resolve(ctx context.Context, namespace, name string) (*GuestInfo, error) {
	guest, err := r.ResolveGuest(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	pod, err := r.ResolvePod(ctx, guest)
	if err != nil {
		return nil, err
	}
	return &GuestInfo{Guest: guest, Pod: pod}, nil
}

// SwiftGuestResource returns the schema for SwiftGuest (for dynamic client if needed).
func SwiftGuestResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "swift.kubeswift.io",
		Version:  "v1alpha1",
		Resource: "swiftguests",
	}
}

// DeletePod deletes the given pod.
func (r *GuestResolver) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	return r.Delete(ctx, pod)
}

// PatchRunPolicy patches the SwiftGuest spec.runPolicy.
func (r *GuestResolver) PatchRunPolicy(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, runPolicy swiftv1alpha1.RunPolicy) error {
	guest.Spec.RunPolicy = runPolicy
	return r.Update(ctx, guest)
}
