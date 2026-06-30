package seed

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seedv1alpha1 "github.com/kubeswift-io/kubeswift/api/seed/v1alpha1"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	gvSeed := schema.GroupVersion{Group: "seed.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSeed, &seedv1alpha1.SwiftSeedProfile{}, &seedv1alpha1.SwiftSeedProfileList{})
	return s
}

func TestResolve_InlinePassThrough(t *testing.T) {
	client := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	got, err := Resolve(context.Background(), client, "ns", "hello", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

func TestResolve_ConfigMapRef(t *testing.T) {
	scheme := testScheme()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cm", Namespace: "ns"},
		Data:       map[string]string{"userdata": "#cloud-config\n"},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	ref := &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "my-cm"}, Key: "userdata"}
	got, err := Resolve(context.Background(), client, "ns", "", &seedv1alpha1.SeedDataValueFrom{ConfigMapKeyRef: ref})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "#cloud-config\n" {
		t.Errorf("got %q", got)
	}
}

func TestResolve_SecretRef(t *testing.T) {
	scheme := testScheme()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "ns"},
		Data:       map[string][]byte{"userdata": []byte("secret-content")},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	ref := &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "my-secret"}, Key: "userdata"}
	got, err := Resolve(context.Background(), client, "ns", "", &seedv1alpha1.SeedDataValueFrom{SecretKeyRef: ref})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "secret-content" {
		t.Errorf("got %q", got)
	}
}

func TestResolve_ConfigMapNotFound(t *testing.T) {
	client := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	ref := &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "missing"}, Key: "key"}
	_, err := Resolve(context.Background(), client, "ns", "", &seedv1alpha1.SeedDataValueFrom{ConfigMapKeyRef: ref})
	if err == nil {
		t.Fatal("expected error for missing ConfigMap")
	}
}
