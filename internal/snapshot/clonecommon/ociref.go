package clonecommon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// maxTagLen is the OCI distribution spec's limit on a tag.
const maxTagLen = 128

// TruncateTag keeps a tag within the 128-character limit: a longer one is cut
// and given a hash of the whole, so two long tags never collide.
func TruncateTag(tag string) string {
	if len(tag) <= maxTagLen {
		return tag
	}
	sum := sha256.Sum256([]byte(tag))
	return tag[:maxTagLen-9] + "-" + hex.EncodeToString(sum[:])[:8]
}

// ExplicitOCITag is the tag of a snapshot that names its own registry:
// spec.backend.oci.tag, or "<namespace>-<name>".
func ExplicitOCITag(snap *snapshotv1alpha1.SwiftSnapshot) string {
	if o := snap.Spec.Backend.OCI; o != nil && o.Tag != "" {
		return o.Tag
	}
	return TruncateTag(snap.Namespace + "-" + snap.Name)
}

// LocationOCITag is the tag of a snapshot stored in a storage location:
// "<name>-<first 8 characters of its UID>". The UID part means a snapshot
// deleted and recreated under the same name never takes over the tag of an
// artifact the first one left behind (deletionPolicy Retain).
func LocationOCITag(snap *snapshotv1alpha1.SwiftSnapshot) string {
	uid := string(snap.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return TruncateTag(snap.Name + "-" + uid)
}

// OCIConnection is how to reach an oci snapshot's artifacts.
type OCIConnection struct {
	Repository            string
	Tag                   string
	Insecure              bool
	CABundle              string
	CredentialsSecretName string
	SigningKeySecretName  string
}

// SnapshotOCI is the registry an oci snapshot uses: status.location, which
// the controller records before the capture, else spec.backend.oci for a
// snapshot taken before storage locations existed. ok is false when neither
// names a repository yet (a snapshot still resolving its location).
func SnapshotOCI(snap *snapshotv1alpha1.SwiftSnapshot) (OCIConnection, bool) {
	if l := snap.Status.Location; l != nil && l.Repository != "" {
		return OCIConnection{
			Repository:            l.Repository,
			Tag:                   l.Tag,
			Insecure:              l.Insecure,
			CABundle:              l.CABundle,
			CredentialsSecretName: l.CredentialsSecretName,
			SigningKeySecretName:  l.SigningKeySecretName,
		}, true
	}
	o := snap.Spec.Backend.OCI
	if o == nil || o.Repository == "" {
		return OCIConnection{}, false
	}
	c := OCIConnection{Repository: o.Repository, Tag: ExplicitOCITag(snap), Insecure: o.Insecure}
	if o.CredentialsSecretRef != nil {
		c.CredentialsSecretName = o.CredentialsSecretRef.Name
	}
	if o.SigningKeySecretRef != nil {
		c.SigningKeySecretName = o.SigningKeySecretRef.Name
	}
	return c, true
}

// OCIReference is the repository:tag an oci snapshot pushes its artifact to:
// the reference it recorded once pushed, else the one its location or spec
// names. Empty for a snapshot that is not oci or has no repository yet.
func OCIReference(snap *snapshotv1alpha1.SwiftSnapshot) string {
	if snap.Spec.Backend.Type != snapshotv1alpha1.SnapshotBackendOCI {
		return ""
	}
	if st := snap.Status.OCI; st != nil && st.Reference != "" {
		return st.Reference
	}
	if c, ok := SnapshotOCI(snap); ok {
		return c.Repository + ":" + c.Tag
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
