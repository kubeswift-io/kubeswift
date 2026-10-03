package clonecommon

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// OCITag is an oci snapshot's artifact tag: spec.backend.oci.tag, or the
// per-snapshot default "<namespace>-<name>".
func OCITag(snap *snapshotv1alpha1.SwiftSnapshot) string {
	if o := snap.Spec.Backend.OCI; o != nil && o.Tag != "" {
		return o.Tag
	}
	return snap.Namespace + "-" + snap.Name
}

// OCIReference is the repository:tag an oci snapshot pushes its artifact to:
// the reference it recorded once pushed, else the one its spec names. Empty
// for a snapshot that is not oci or names no repository.
func OCIReference(snap *snapshotv1alpha1.SwiftSnapshot) string {
	if snap.Spec.Backend.Type != snapshotv1alpha1.SnapshotBackendOCI {
		return ""
	}
	if st := snap.Status.OCI; st != nil && st.Reference != "" {
		return st.Reference
	}
	if o := snap.Spec.Backend.OCI; o != nil && o.Repository != "" {
		return o.Repository + ":" + OCITag(snap)
	}
	return ""
}

// OCIReferenceHolders lists the other oci snapshots in snap's namespace that
// push, or pushed, to the same repository:tag (#705). A tag names only what was
// pushed to it last, and some registries drop the earlier manifest as soon as
// the tag is pushed again, so a second snapshot on one tag can destroy the
// first one's artifact while it still reads Ready.
//
// A snapshot being deleted is not a holder: its deletion removes only its own
// digest. Nor is a Failed one, unless it recorded a pushed artifact. Only snap's
// namespace is searched: another namespace's tags are not this one's to see,
// and its registry credentials are its own.
func OCIReferenceHolders(ctx context.Context, c client.Reader, snap *snapshotv1alpha1.SwiftSnapshot) ([]snapshotv1alpha1.SwiftSnapshot, error) {
	ref := OCIReference(snap)
	if ref == "" {
		return nil, nil
	}
	var list snapshotv1alpha1.SwiftSnapshotList
	if err := c.List(ctx, &list, client.InNamespace(snap.Namespace)); err != nil {
		return nil, err
	}
	var holders []snapshotv1alpha1.SwiftSnapshot
	for i := range list.Items {
		o := &list.Items[i]
		if o.Name == snap.Name || o.DeletionTimestamp != nil {
			continue
		}
		pushed := o.Status.OCI != nil && o.Status.OCI.Reference != ""
		if o.Status.Phase == snapshotv1alpha1.SwiftSnapshotPhaseFailed && !pushed {
			continue
		}
		if OCIReference(o) == ref {
			holders = append(holders, *o)
		}
	}
	return holders, nil
}

// OCITagInUseMessage explains a refusal of a snapshot whose repository:tag
// another snapshot already uses; the controller and the webhook both say it.
func OCITagInUseMessage(ref, holder string) string {
	return ref + " is already used by SwiftSnapshot " + holder +
		": a tag names only what was pushed to it last, and some registries drop the earlier artifact" +
		" as soon as the tag is pushed again. Leave spec.backend.oci.tag empty for a per-snapshot tag, or choose another"
}
