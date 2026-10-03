package swiftstoragelocation

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
	kscheme "github.com/kubeswift-io/kubeswift/internal/scheme"
)

func cl(name string, isDefault bool) *storagev1alpha1.SwiftClusterStorageLocation {
	return &storagev1alpha1.SwiftClusterStorageLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       storagev1alpha1.StorageLocationSpec{Default: isDefault, OCI: &storagev1alpha1.OCILocation{Repository: "registry.example.com/" + name}},
	}
}

func nl(ns, name string, isDefault bool) *storagev1alpha1.SwiftStorageLocation {
	return &storagev1alpha1.SwiftStorageLocation{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       storagev1alpha1.StorageLocationSpec{Default: isDefault, OCI: &storagev1alpha1.OCILocation{Repository: "registry.example.com/" + name}},
	}
}

func reader(objs ...client.Object) client.Reader {
	return fake.NewClientBuilder().WithScheme(kscheme.Scheme).WithObjects(objs...).Build()
}

// A second cluster default is refused at create and when an update turns it
// on; an edit to a location that already is the default is not, so a conflict
// it did not make cannot block it.
func TestClusterValidator_OneDefault(t *testing.T) {
	ctx := context.Background()
	v := &ClusterValidator{Client: reader(cl("main", true), cl("other", false))}
	if _, err := v.ValidateCreate(ctx, cl("second", true)); err == nil || !strings.Contains(err.Error(), "main is already the cluster default") {
		t.Errorf("second default at create: %v", err)
	}
	if _, err := v.ValidateCreate(ctx, cl("plain", false)); err != nil {
		t.Errorf("a non-default: %v", err)
	}
	if _, err := v.ValidateUpdate(ctx, cl("other", false), cl("other", true)); err == nil {
		t.Error("turning a second location into the default must be refused")
	}
	edited := cl("main", true)
	edited.Spec.OCI.Insecure = true
	if _, err := v.ValidateUpdate(ctx, cl("main", true), edited); err != nil {
		t.Errorf("editing the existing default: %v", err)
	}
	if _, err := v.ValidateDelete(ctx, cl("main", true)); err != nil {
		t.Errorf("delete: %v", err)
	}
}

// A namespace's default competes only with defaults in the same namespace.
func TestNamespaceValidator_OneDefaultPerNamespace(t *testing.T) {
	ctx := context.Background()
	v := &NamespaceValidator{Client: reader(nl("team-a", "x", true))}
	if _, err := v.ValidateCreate(ctx, nl("team-a", "y", true)); err == nil || !strings.Contains(err.Error(), "namespace team-a") {
		t.Errorf("second default in team-a: %v", err)
	}
	if _, err := v.ValidateCreate(ctx, nl("team-b", "y", true)); err != nil {
		t.Errorf("team-b's first default: %v", err)
	}
}

func TestValidators_RefuseAMalformedSpec(t *testing.T) {
	ctx := context.Background()
	bad := cl("bad", false)
	bad.Spec.OCI.Repository = "registry.example.com/kubeswift:v1"
	if _, err := (&ClusterValidator{}).ValidateCreate(ctx, bad); err == nil || !strings.Contains(err.Error(), "tag or digest") {
		t.Errorf("cluster: %v", err)
	}
	nbad := nl("team-a", "bad", false)
	nbad.Spec.OCI.Anonymous = true
	nbad.Spec.OCI.CredentialsSecretName = "regcreds"
	if _, err := (&NamespaceValidator{}).ValidateCreate(ctx, nbad); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("namespace: %v", err)
	}
}
