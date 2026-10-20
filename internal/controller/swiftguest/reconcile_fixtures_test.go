package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// Fixtures for tests that drive Reconcile against a fake client, where what
// matters is what the operator sees on the guest, not what a helper returns.

const testGuestName = "g"

var testGuestKey = types.NamespacedName{Namespace: "ns", Name: testGuestName}

// testGuestUID is the test guest's UID. Its launcher pods are controlled by
// it: a pod by the launcher's name that the guest does not control is not its
// launcher (#819).
const testGuestUID types.UID = "guest-uid"

// guestControllerRef is the controller reference the controller sets on every
// launcher pod of the test guest.
func guestControllerRef() []metav1.OwnerReference {
	return []metav1.OwnerReference{*metav1.NewControllerRef(
		&swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: testGuestName, UID: testGuestUID}},
		schema.GroupVersionKind{Group: swiftv1alpha1.GroupName, Version: swiftv1alpha1.Version, Kind: "SwiftGuest"})}
}

// kernelGuest is the smallest guest that reconciles all the way to a launcher
// pod: kernel boot needs only testGuestClass and readyKernel.
func kernelGuest() *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: testGuestName, Namespace: "ns", UID: testGuestUID},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			KernelRef:     &corev1.LocalObjectReference{Name: "k"},
			GuestClassRef: corev1.LocalObjectReference{Name: "cls"},
			RunPolicy:     swiftv1alpha1.RunPolicyRunning,
		},
	}
}

func readyKernel() *kernelv1alpha1.SwiftKernel {
	return &kernelv1alpha1.SwiftKernel{
		ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: "ns"},
		Status:     kernelv1alpha1.SwiftKernelStatus{Phase: kernelv1alpha1.SwiftKernelPhaseReady},
	}
}

// asDiskBoot switches g to boot from readyImage. With preparedPVC present it
// reaches the root-disk clone.
func asDiskBoot(g *swiftv1alpha1.SwiftGuest) *swiftv1alpha1.SwiftGuest {
	g.Spec.KernelRef = nil
	g.Spec.ImageRef = &corev1.LocalObjectReference{Name: "img"}
	return g
}

func readyImage() *imagev1alpha1.SwiftImage {
	return &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status: imagev1alpha1.SwiftImageStatus{
			Phase: imagev1alpha1.SwiftImagePhaseReady,
			PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{
				PVCRef: &imagev1alpha1.PVCObjectReference{Name: "img-prepared"},
			},
		},
	}
}

func preparedPVC() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "img-prepared", Namespace: "ns"}}
}

// runningLauncher is the test guest's launcher pod, already up on nodeName.
func runningLauncher(nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testGuestName, Namespace: "ns", OwnerReferences: guestControllerRef()},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func guestClientBuilder(objs ...client.Object) *fake.ClientBuilder {
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
		WithStatusSubresource(&swiftv1alpha1.SwiftGuest{})
}

// reconcileGuest runs one Reconcile of the test guest and returns it as stored.
func reconcileGuest(t *testing.T, r *SwiftGuestReconciler) (*swiftv1alpha1.SwiftGuest, ctrl.Result, error) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: testGuestKey})
	var got swiftv1alpha1.SwiftGuest
	if getErr := r.Get(context.Background(), testGuestKey, &got); getErr != nil {
		t.Fatalf("get guest: %v", getErr)
	}
	return &got, res, err
}

func guestCondition(t *testing.T, g *swiftv1alpha1.SwiftGuest, condType string) metav1.Condition {
	t.Helper()
	c := findCondition(&g.Status, condType)
	if c == nil {
		t.Fatalf("guest has no %s condition; status = %+v", condType, g.Status)
	}
	return *c
}

func launcherPods(t *testing.T, c client.Client) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	return pods.Items
}
