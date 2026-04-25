package swiftguest

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
)

func rootdiskScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{})
	metav1.AddToGroupVersion(s, gvSwift)
	return s
}

// TestEnsureRootDiskClone_RestoreSeededSkipsCopyJob verifies that a PVC
// labelled RestoreSeededLabel=true short-circuits the Copy Job path. This
// is the contract SwiftRestore relies on — without it, the controller
// would overwrite the restored disk contents from the source SwiftImage's
// PVC.
func TestEnsureRootDiskClone_RestoreSeededSkipsCopyJob(t *testing.T) {
	scheme := rootdiskScheme(t)

	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "target-vm",
			Namespace: "default",
			UID:       "abc-123",
		},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "ubuntu"},
			GuestClassRef: corev1.LocalObjectReference{Name: "default"},
		},
	}
	swiftGuestGVKLocal := schema.GroupVersionKind{
		Group:   "swift.kubeswift.io",
		Version: "v1alpha1",
		Kind:    "SwiftGuest",
	}
	cloneName := RootDiskCloneName(guest.Name)

	// Pre-existing restore-seeded PVC (Bound, controlled by guest, no Job).
	apiGroup := "snapshot.storage.k8s.io"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cloneName,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				RestoreSeededLabel: "true",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(guest, swiftGuestGVKLocal),
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     "vs1",
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	srcPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "src-image", Namespace: guest.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40Gi")},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(guest, pvc, srcPVC).
		Build()

	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}
	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{PVCName: "src-image"},
		RootDisk:      resolved.RootDisk{Size: resource.MustParse("40Gi")},
	}

	res, err := r.EnsureRootDiskClone(context.Background(), guest, rg)
	if err != nil {
		t.Fatalf("EnsureRootDiskClone returned err = %v, want nil (restore-seeded should pass through)", err)
	}
	if res == nil || res.PVCName != cloneName {
		t.Fatalf("result = %+v, want PVCName=%s", res, cloneName)
	}
	if res.NeedsGrowInit {
		t.Errorf("NeedsGrowInit = true, want false (restore-seeded path doesn't grow)")
	}

	// Most important assertion: NO clone Job was created.
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace(guest.Namespace)); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, j := range jobs.Items {
		if j.Name == CloneJobName(guest.Name) {
			t.Errorf("Copy Job %s was created — restore-seeded path must skip it", j.Name)
		}
	}
}
