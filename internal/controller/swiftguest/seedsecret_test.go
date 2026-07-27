package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/seed"
)

func seedScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	// Only ConfigMaps and Pods are touched; the guest is just a name/namespace.
	return s
}

func legacyCM(ns, name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string]string{seed.KeyUserData: "#cloud-config\npassword: hunter2\n"},
	}
}

func podMounting(ns, guest, cmName string, viaConfigMap bool) *corev1.Pod {
	vol := corev1.Volume{Name: "seed"}
	if viaConfigMap {
		vol.ConfigMap = &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
		}
	} else {
		vol.Secret = &corev1.SecretVolumeSource{SecretName: cmName}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: guest + "-launcher",
			Labels: map[string]string{guestPodLabelKey: guest},
		},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{vol}},
	}
}

func TestRetireLegacySeedConfigMap_KeepsItWhileAPodStillMountsIt(t *testing.T) {
	// The upgrade hazard: a guest that was already running keeps its launcher
	// pod, and that pod still mounts the ConfigMap. Deleting it out from under
	// the pod breaks the guest on its next restart, when kubelet re-projects the
	// volume and cannot find it.
	s := seedScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "g1"}}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(legacyCM("ns", "g1-seed"), podMounting("ns", "g1", "g1-seed", true)).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: s}

	if err := r.retireLegacySeedConfigMap(context.Background(), guest, "g1-seed"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "g1-seed"}, &cm); err != nil {
		t.Fatalf("legacy ConfigMap was deleted while a live pod still mounted it: %v", err)
	}
}

func TestRetireLegacySeedConfigMap_DeletesOnceThePodMovedToTheSecret(t *testing.T) {
	s := seedScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "g1"}}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(legacyCM("ns", "g1-seed"), podMounting("ns", "g1", "g1-seed", false)).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: s}

	if err := r.retireLegacySeedConfigMap(context.Background(), guest, "g1-seed"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	var cm corev1.ConfigMap
	err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "g1-seed"}, &cm)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("plaintext ConfigMap survived after the pod moved to the Secret (err=%v)", err)
	}
}

func TestRetireLegacySeedConfigMap_NoPodsAtAll(t *testing.T) {
	s := seedScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "g1"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(legacyCM("ns", "g1-seed")).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: s}

	if err := r.retireLegacySeedConfigMap(context.Background(), guest, "g1-seed"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "g1-seed"},
		&cm); !apierrors.IsNotFound(err) {
		t.Fatalf("stopped guest should have had its plaintext ConfigMap retired (err=%v)", err)
	}
}

func TestRetireLegacySeedConfigMap_AbsentIsNotAnError(t *testing.T) {
	s := seedScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "g1"}}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: s}
	if err := r.retireLegacySeedConfigMap(context.Background(), guest, "g1-seed"); err != nil {
		t.Fatalf("a guest created after the change has no ConfigMap; must be a no-op, got %v", err)
	}
}

func TestRenderedSeedData_MatchesWhatTheAPIServerReturns(t *testing.T) {
	// A Secret written with StringData comes back with Data and StringData nil.
	// Comparing the built object directly against the stored one would differ on
	// every reconcile -> write -> watch event -> reconcile: a hot loop.
	built := seed.BuildSecret("g1-seed", "ns", "user", "meta", "net")
	got := renderedSeedData(built)
	want := map[string][]byte{
		seed.KeyUserData:      []byte("user"),
		seed.KeyMetaData:      []byte("meta"),
		seed.KeyNetworkConfig: []byte("net"),
	}
	if len(got) != len(want) {
		t.Fatalf("rendered %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if string(got[k]) != string(v) {
			t.Errorf("key %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestBuildSecret_CarriesNoPlaintextConfigMap(t *testing.T) {
	s := seed.BuildSecret("g1-seed", "ns", "#cloud-config\npassword: hunter2\n", "", "")
	if s.StringData[seed.KeyUserData] == "" {
		t.Fatal("user-data missing from the rendered seed Secret")
	}
	if s.TypeMeta.Kind != "" && s.TypeMeta.Kind != "Secret" {
		t.Errorf("rendered seed is a %s, want Secret", s.TypeMeta.Kind)
	}
}
