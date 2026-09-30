// Package namespaces answers what the controllers need to know about the
// namespace an object lives in.
package namespaces

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Terminating reports whether the namespace is being deleted.
//
// A namespace being deleted admits no new objects ("unable to create new
// content in namespace … because it is being terminated"), so a controller
// has nothing to build in it: the namespace controller deletes the object
// being reconciled next, and its deletion path runs then. Building toward
// running meanwhile only fails, and each refused create was logged as a
// reconciler error (lab: 10 to 30 for each test namespace deleted).
//
// Only the path that builds may skip on this. A deletion path must still run,
// and retry, in a namespace being deleted.
//
// A namespace the client cannot find, or no namespace (a cluster-scoped
// object), is reported as not terminating, so a caller goes on as it did
// before this check existed.
func Terminating(ctx context.Context, c client.Reader, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return !ns.DeletionTimestamp.IsZero() || ns.Status.Phase == corev1.NamespaceTerminating, nil
}
