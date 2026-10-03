package storagelocation

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
)

// Reasons an object waits instead of resolving a location. They are
// condition reasons on the waiting object.
const (
	ReasonNotFound           = "StorageLocationNotFound"
	ReasonInvalid            = "StorageLocationInvalid"
	ReasonAmbiguous          = "AmbiguousStorageLocation"
	ReasonNone               = "NoStorageLocation"
	ReasonCredentialsMissing = "RegistryCredentialsMissing"
	ReasonSigningKeyMissing  = "SigningKeyMissing"
)

// Need is the part of a location a consumer uses.
type Need int

const (
	// NeedOCI: the location's oci registry.
	NeedOCI Need = iota
	// NeedCSI: the location's csi VolumeSnapshotClass.
	NeedCSI
)

func (n Need) has(spec *storagev1alpha1.StorageLocationSpec) bool {
	if n == NeedCSI {
		return spec.CSI != nil
	}
	return spec.OCI != nil
}

func (n Need) String() string {
	if n == NeedCSI {
		return "csi VolumeSnapshotClass"
	}
	return "oci registry"
}

// Resolved is the location chosen for an object.
type Resolved struct {
	// Kind is KindSwiftStorageLocation or KindSwiftClusterStorageLocation.
	Kind string
	Name string
	Spec storagev1alpha1.StorageLocationSpec
}

// Source names the location as status records it: <Kind>/<name>.
func (r *Resolved) Source() string { return r.Kind + "/" + r.Name }

// Wait says why an object cannot resolve a location yet. Reason is a
// condition reason; Message names what is missing and where.
type Wait struct {
	Reason  string
	Message string
}

// Resolve chooses the location an object in namespace uses for need:
//
//  1. ref, if set: that location; missing or unusable, the object waits.
//  2. Else the namespace's default SwiftStorageLocation, if it configures
//     need. A namespace default that does not (say, one that sets only a
//     VolumeSnapshotClass) leaves need to the cluster default.
//  3. Else the cluster's default SwiftClusterStorageLocation, if it
//     configures need.
//
// It returns (nil, nil, nil) when nothing applies; the caller decides what
// that means. Two defaults at one level are never resolved by picking one:
// the object waits, naming them. A SwiftStorageLocation is only ever looked
// up in namespace.
func Resolve(ctx context.Context, c client.Reader, namespace string, ref *storagev1alpha1.StorageLocationRef, need Need) (*Resolved, *Wait, error) {
	if ref != nil {
		return resolveRef(ctx, c, namespace, ref, need)
	}

	var nsList storagev1alpha1.SwiftStorageLocationList
	if err := c.List(ctx, &nsList, client.InNamespace(namespace)); err != nil {
		return nil, nil, err
	}
	var nsDefaults []*storagev1alpha1.SwiftStorageLocation
	for i := range nsList.Items {
		if l := &nsList.Items[i]; l.Spec.Default && l.DeletionTimestamp == nil {
			nsDefaults = append(nsDefaults, l)
		}
	}
	switch {
	case len(nsDefaults) > 1:
		names := make([]string, 0, len(nsDefaults))
		for _, l := range nsDefaults {
			names = append(names, l.Name)
		}
		return nil, ambiguous(storagev1alpha1.KindSwiftStorageLocation, names, "namespace "+namespace), nil
	case len(nsDefaults) == 1 && need.has(&nsDefaults[0].Spec):
		return checked(&Resolved{Kind: storagev1alpha1.KindSwiftStorageLocation, Name: nsDefaults[0].Name, Spec: nsDefaults[0].Spec}, need)
	}

	var clList storagev1alpha1.SwiftClusterStorageLocationList
	if err := c.List(ctx, &clList); err != nil {
		return nil, nil, err
	}
	var clDefaults []*storagev1alpha1.SwiftClusterStorageLocation
	for i := range clList.Items {
		if l := &clList.Items[i]; l.Spec.Default && l.DeletionTimestamp == nil {
			clDefaults = append(clDefaults, l)
		}
	}
	switch {
	case len(clDefaults) > 1:
		names := make([]string, 0, len(clDefaults))
		for _, l := range clDefaults {
			names = append(names, l.Name)
		}
		return nil, ambiguous(storagev1alpha1.KindSwiftClusterStorageLocation, names, "the cluster"), nil
	case len(clDefaults) == 1 && need.has(&clDefaults[0].Spec):
		return checked(&Resolved{Kind: storagev1alpha1.KindSwiftClusterStorageLocation, Name: clDefaults[0].Name, Spec: clDefaults[0].Spec}, need)
	}
	return nil, nil, nil
}

func resolveRef(ctx context.Context, c client.Reader, namespace string, ref *storagev1alpha1.StorageLocationRef, need Need) (*Resolved, *Wait, error) {
	kind := ref.Kind
	if kind == "" {
		kind = storagev1alpha1.KindSwiftStorageLocation
	}
	var (
		spec    storagev1alpha1.StorageLocationSpec
		err     error
		where   = "in namespace " + namespace
		deleted bool
	)
	switch kind {
	case storagev1alpha1.KindSwiftStorageLocation:
		var l storagev1alpha1.SwiftStorageLocation
		err = c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &l)
		spec, deleted = l.Spec, l.DeletionTimestamp != nil
	case storagev1alpha1.KindSwiftClusterStorageLocation:
		var l storagev1alpha1.SwiftClusterStorageLocation
		err = c.Get(ctx, client.ObjectKey{Name: ref.Name}, &l)
		spec, deleted, where = l.Spec, l.DeletionTimestamp != nil, "in the cluster"
	default:
		return nil, &Wait{Reason: ReasonInvalid, Message: fmt.Sprintf("spec locationRef.kind %q is not SwiftStorageLocation or SwiftClusterStorageLocation", ref.Kind)}, nil
	}
	if apierrors.IsNotFound(err) || (err == nil && deleted) {
		return nil, &Wait{Reason: ReasonNotFound, Message: fmt.Sprintf("%s %s does not exist %s", kind, ref.Name, where)}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	res := &Resolved{Kind: kind, Name: ref.Name, Spec: spec}
	if !need.has(&spec) {
		return nil, &Wait{Reason: ReasonInvalid, Message: fmt.Sprintf("%s configures no %s", res.Source(), need)}, nil
	}
	return checked(res, need)
}

// checked refuses a location whose spec is malformed, which a disabled
// webhook may have let in.
func checked(res *Resolved, need Need) (*Resolved, *Wait, error) {
	if err := Validate(&res.Spec); err != nil {
		return nil, &Wait{Reason: ReasonInvalid, Message: fmt.Sprintf("%s is not valid: %v", res.Source(), err)}, nil
	}
	return res, nil, nil
}

func ambiguous(kind string, names []string, where string) *Wait {
	return &Wait{Reason: ReasonAmbiguous, Message: fmt.Sprintf(
		"%ss %s are all defaults in %s; unset spec.default on all but one, or name one in locationRef",
		kind, strings.Join(names, ", "), where)}
}

// CredentialsSecret is the dockerconfigjson Secret an oci location's objects
// use: its credentialsSecretName, else DefaultCredentialsSecretName, or none
// when it is anonymous.
func CredentialsSecret(o *storagev1alpha1.OCILocation) string {
	switch {
	case o.Anonymous:
		return ""
	case o.CredentialsSecretName != "":
		return o.CredentialsSecretName
	}
	return storagev1alpha1.DefaultCredentialsSecretName
}

// CheckSecrets makes sure the Secrets a resolved oci location names exist in
// namespace, with the keys the transfer Jobs mount, so a missing one makes the
// object wait with a reason instead of leaving a Job pod stuck. Only namespace
// is read, whatever the location: a location holds Secret names, never a way
// to reach another namespace's Secrets.
func CheckSecrets(ctx context.Context, c client.Reader, namespace, credentials, signingKey string) (*Wait, error) {
	if credentials != "" {
		w, err := checkSecretKey(ctx, c, namespace, credentials, corev1.DockerConfigJsonKey, ReasonCredentialsMissing,
			"the registry credentials Secret", "create a kubernetes.io/dockerconfigjson Secret of that name, or set anonymous: true on the location if the registry needs none")
		if w != nil || err != nil {
			return w, err
		}
	}
	if signingKey != "" {
		return checkSecretKey(ctx, c, namespace, signingKey, "cosign.key", ReasonSigningKeyMissing,
			"the signing key Secret", "create it with keys cosign.key and cosign.password (cosign generate-key-pair)")
	}
	return nil, nil
}

func checkSecretKey(ctx context.Context, c client.Reader, namespace, name, key, reason, what, fix string) (*Wait, error) {
	var s corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &s)
	if apierrors.IsNotFound(err) {
		return &Wait{Reason: reason, Message: fmt.Sprintf("%s %s does not exist in namespace %s: %s", what, name, namespace, fix)}, nil
	}
	if err != nil {
		return nil, err
	}
	if _, ok := s.Data[key]; !ok {
		return &Wait{Reason: reason, Message: fmt.Sprintf("%s %s in namespace %s has no %s key: %s", what, name, namespace, key, fix)}, nil
	}
	return nil, nil
}

// SnapshotRepository is the repository a location's snapshots go to: a
// cluster location's repository plus <namespace>/snapshots, so each namespace
// gets its own path, or a namespace location's plus snapshots.
func SnapshotRepository(res *Resolved, namespace string) string {
	repo := strings.TrimSuffix(res.Spec.OCI.Repository, "/")
	if res.Kind == storagev1alpha1.KindSwiftClusterStorageLocation {
		return repo + "/" + namespace + "/snapshots"
	}
	return repo + "/snapshots"
}
