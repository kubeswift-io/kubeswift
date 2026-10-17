package swiftsandbox

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// patchFinalizers applies edit (which changes only obj's finalizers) as a
// merge patch of exactly that change, conditional on obj's resourceVersion.
//
// Never a full-object Update: that sends the whole spec back, and a spec
// field the object was created without (a non-pointer struct such as
// spec.network, written back as {}) then gets its CRD defaults applied. The
// webhook sees a spec change and refuses it, so the sandbox never got past
// its first finalizer (#768).
func patchFinalizers(ctx context.Context, c client.Client, obj client.Object, edit func()) error {
	base := obj.DeepCopyObject().(client.Object)
	edit()
	return c.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
