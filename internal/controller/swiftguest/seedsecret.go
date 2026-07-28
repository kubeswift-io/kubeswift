package swiftguest

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// renderedSeedData converts a freshly built seed Secret's StringData into the
// Data form the apiserver stores and returns.
//
// This exists so the reconcile comparison is apples-to-apples. A Secret written
// with StringData comes back with Data and StringData nil, so comparing the two
// directly would differ on every pass and write on every reconcile — which the
// watch then re-enqueues, i.e. a hot loop.
func renderedSeedData(s *corev1.Secret) map[string][]byte {
	out := make(map[string][]byte, len(s.StringData)+len(s.Data))
	for k, v := range s.Data {
		out[k] = v
	}
	for k, v := range s.StringData {
		out[k] = []byte(v)
	}
	return out
}

// retireLegacySeedConfigMap deletes the pre-v0.13.4 plaintext seed ConfigMap
// once nothing is mounting it any more.
//
// Upgrade shape: the seed is now a Secret sharing the ConfigMap's name (they are
// different resource types, so both can exist). A guest that was already running
// keeps its launcher pod, and that pod still mounts the ConfigMap — deleting it
// out from under a live pod would break the guest on its next restart, when
// kubelet re-projects the volume and cannot find it.
//
// So: delete only when no pod for this guest still references it. A running
// guest therefore keeps the plaintext copy until it is next recreated, which is
// the honest trade — the alternative is breaking running VMs on upgrade. Guests
// created after this change never get a ConfigMap at all.
func (r *SwiftGuestReconciler) retireLegacySeedConfigMap(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, name string) error {
	logger := crlog.FromContext(ctx)

	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: name}, &cm); err != nil {
		return client.IgnoreNotFound(err) // already gone: nothing to do
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(guest.Namespace),
		client.MatchingLabels{guestPodLabelKey: guest.Name}); err != nil {
		return err
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp != nil {
			continue
		}
		for _, v := range pods.Items[i].Spec.Volumes {
			if v.ConfigMap != nil && v.ConfigMap.Name == name {
				// Still in use — leave it and try again on a later reconcile.
				return nil
			}
		}
	}

	if err := r.Delete(ctx, &cm); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	logger.Info("retired the legacy plaintext seed ConfigMap; the seed is a Secret now",
		"configMap", name, "namespace", guest.Namespace)
	return nil
}
