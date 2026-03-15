package cli

import (
	"context"
	"fmt"
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
// Uses status.podRef if set; otherwise finds by label swift.kubeswift.io/guest=<name>.
func (r *GuestResolver) ResolvePod(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*corev1.Pod, error) {
	namespace := guest.Namespace
	name := guest.Name

	if guest.Status.PodRef != nil {
		var pod corev1.Pod
		err := r.Get(ctx, client.ObjectKey{
			Namespace: guest.Status.PodRef.Namespace,
			Name:      guest.Status.PodRef.Name,
		}, &pod)
		if err != nil {
			if errors.IsNotFound(err) {
				return nil, fmt.Errorf("pod for guest %q not found", name)
			}
			return nil, err
		}
		return &pod, nil
	}

	// Fallback: list pods by label
	sel := labels.SelectorFromSet(map[string]string{GuestLabelKey: name})
	var list corev1.PodList
	err := r.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel})
	if err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("pod for guest %q not found", name)
	}
	return &list.Items[0], nil
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
