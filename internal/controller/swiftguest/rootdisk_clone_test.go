package swiftguest

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
)

func makeGuestObj(name, ns string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "guest-uid",
		},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "default-class"},
		},
	}
}

func makeSourceImagePVC(name, ns, storageClass, size string) *corev1.PersistentVolumeClaim {
	sc := storageClass
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	if storageClass != "" {
		pvc.Spec.StorageClassName = &sc
	}
	return pvc
}

// ---------- Copy strategy ----------

func TestCopy_PVCMissing_CreatesPVCWaitsForBound(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := makeGuestObj("g1", "default")
	src := makeSourceImagePVC("src-img", "default", "longhorn", "40Gi")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, src).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{PVCName: "src-img"},
		RootDisk:      resolved.RootDisk{Size: resource.MustParse("40Gi")},
	}
	res, err := r.EnsureRootDiskClone(context.Background(), guest, rg)
	if err == nil || !strings.Contains(err.Error(), "waiting for Bound") {
		t.Fatalf("err = %v, want 'waiting for Bound'", err)
	}
	if res != nil {
		t.Errorf("result should be nil while PVC not yet bound")
	}
	var clone corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Name: RootDiskCloneName("g1"), Namespace: "default"}, &clone); err != nil {
		t.Fatalf("clone PVC was not created: %v", err)
	}
	if clone.Spec.DataSource != nil {
		t.Errorf("copy-path PVC should not have a dataSource (got %+v)", clone.Spec.DataSource)
	}
	if !metav1.IsControlledBy(&clone, guest) {
		t.Errorf("clone PVC must be controlled by the SwiftGuest")
	}
}

func TestCopy_PVCBoundNoJob_CreatesJob(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := makeGuestObj("g1", "default")
	src := makeSourceImagePVC("src-img", "default", "longhorn", "40Gi")
	clone := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RootDiskCloneName("g1"),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, schema.GroupVersionKind{
				Group:   "swift.kubeswift.io",
				Version: "v1alpha1",
				Kind:    "SwiftGuest",
			})},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, src, clone).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{PVCName: "src-img"},
		RootDisk:      resolved.RootDisk{Size: resource.MustParse("40Gi")},
	}
	if _, err := r.EnsureRootDiskClone(context.Background(), guest, rg); err == nil ||
		!strings.Contains(err.Error(), "Job") || !strings.Contains(err.Error(), "created") {
		t.Fatalf("err = %v, want 'Job created'", err)
	}
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Name: CloneJobName("g1"), Namespace: "default"}, &job); err != nil {
		t.Fatalf("clone Job not created: %v", err)
	}
}

func TestCopy_JobComplete_PVCBound_ReturnsSuccess_NoGrowInit(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := makeGuestObj("g1", "default")
	src := makeSourceImagePVC("src-img", "default", "longhorn", "40Gi")
	clone := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RootDiskCloneName("g1"),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, schema.GroupVersionKind{
				Group:   "swift.kubeswift.io",
				Version: "v1alpha1",
				Kind:    "SwiftGuest",
			})},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: CloneJobName("g1"), Namespace: "default"},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, src, clone, job).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{PVCName: "src-img"},
		RootDisk:      resolved.RootDisk{Size: resource.MustParse("40Gi")},
	}
	res, err := r.EnsureRootDiskClone(context.Background(), guest, rg)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if res == nil || res.PVCName != RootDiskCloneName("g1") {
		t.Fatalf("result = %+v", res)
	}
	if res.NeedsGrowInit {
		t.Errorf("copy path must never need grow-init (clone Job runs qemu-img resize itself)")
	}
}

func TestCopy_OrphanedPVC_DeletedAndRecreated(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := makeGuestObj("g1", "default")
	src := makeSourceImagePVC("src-img", "default", "longhorn", "40Gi")
	// Existing PVC controlled by some OTHER object — orphan.
	other := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default", UID: "other-uid"},
	}
	clone := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RootDiskCloneName("g1"),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(other, schema.GroupVersionKind{
				Group:   "swift.kubeswift.io",
				Version: "v1alpha1",
				Kind:    "SwiftGuest",
			})},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, other, src, clone).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{PVCName: "src-img"},
		RootDisk:      resolved.RootDisk{Size: resource.MustParse("40Gi")},
	}
	_, err := r.EnsureRootDiskClone(context.Background(), guest, rg)
	if err == nil || !strings.Contains(err.Error(), "orphaned") {
		t.Fatalf("err = %v, want orphan-recreate", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: clone.Name, Namespace: "default"}, &corev1.PersistentVolumeClaim{}); err == nil {
		t.Errorf("orphan PVC should have been deleted")
	}
}

// ---------- Snapshot strategy ----------

func TestSnapshot_PVCMissing_CreatesPVCAtSourceSize_WithDataSource(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := makeGuestObj("g1", "default")
	src := makeSourceImagePVC("src-img", "default", "longhorn", "10Gi")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, src).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{
			PVCName: "src-img",
			CloneSeed: &resolved.PreparedCloneSeed{
				Kind:            "VolumeSnapshot",
				Name:            "vs1",
				Namespace:       "default",
				SourceSizeBytes: 10 * 1024 * 1024 * 1024,
			},
		},
		RootDisk: resolved.RootDisk{Size: resource.MustParse("40Gi")},
	}
	_, err := r.EnsureRootDiskClone(context.Background(), guest, rg)
	if err == nil || !strings.Contains(err.Error(), "waiting for Bound") {
		t.Fatalf("err = %v, want 'waiting for Bound'", err)
	}
	var clone corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Name: RootDiskCloneName("g1"), Namespace: "default"}, &clone); err != nil {
		t.Fatalf("clone PVC missing: %v", err)
	}
	if clone.Spec.DataSource == nil || clone.Spec.DataSource.Kind != "VolumeSnapshot" || clone.Spec.DataSource.Name != "vs1" {
		t.Errorf("clone dataSource = %+v, want VolumeSnapshot/vs1", clone.Spec.DataSource)
	}
	got := clone.Spec.Resources.Requests[corev1.ResourceStorage]
	want := resource.MustParse("10Gi")
	if got.Cmp(want) != 0 {
		t.Errorf("clone PVC requested %s, want %s (must equal source — Longhorn refuses different-size dataSource clones)", got.String(), want.String())
	}
}

func TestSnapshot_TargetEqualsSource_NoExpand_ReturnsSuccessWithGrowInit(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := makeGuestObj("g1", "default")
	src := makeSourceImagePVC("src-img", "default", "longhorn", "10Gi")
	clone := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RootDiskCloneName("g1"),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, schema.GroupVersionKind{
				Group: "swift.kubeswift.io", Version: "v1alpha1", Kind: "SwiftGuest",
			})},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:    corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, src, clone).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{
			PVCName: "src-img",
			CloneSeed: &resolved.PreparedCloneSeed{
				Kind:            "VolumeSnapshot",
				Name:            "vs1",
				Namespace:       "default",
				SourceSizeBytes: 10 * 1024 * 1024 * 1024,
			},
		},
		RootDisk: resolved.RootDisk{Size: resource.MustParse("10Gi")},
	}
	res, err := r.EnsureRootDiskClone(context.Background(), guest, rg)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !res.NeedsGrowInit {
		t.Errorf("snapshot path always needs grow-init for sgdisk -e even when target==source")
	}
}

// Expand-and-wait gate: the canonical Phase 0 §5 finding. If
// status.capacity has not yet reached target, the controller must NOT
// return success — the launcher's qemu-img resize would write past the
// underlying block device end.
func TestSnapshot_TargetGreater_CapacityNotYetReached_RequeueNotSuccess(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := makeGuestObj("g1", "default")
	src := makeSourceImagePVC("src-img", "default", "longhorn", "10Gi")
	clone := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RootDiskCloneName("g1"),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, schema.GroupVersionKind{
				Group: "swift.kubeswift.io", Version: "v1alpha1", Kind: "SwiftGuest",
			})},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:    corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}, // not yet expanded
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, src, clone).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{
			PVCName: "src-img",
			CloneSeed: &resolved.PreparedCloneSeed{
				Kind:            "VolumeSnapshot",
				Name:            "vs1",
				Namespace:       "default",
				SourceSizeBytes: 10 * 1024 * 1024 * 1024,
			},
		},
		RootDisk: resolved.RootDisk{Size: resource.MustParse("40Gi")},
	}
	res, err := r.EnsureRootDiskClone(context.Background(), guest, rg)
	if err == nil {
		t.Fatalf("expected requeue error while capacity < target, got success res=%+v", res)
	}
	if !strings.Contains(err.Error(), "expanding") {
		t.Errorf("err = %v, want 'expanding' (capacity gate)", err)
	}
	if res != nil {
		t.Errorf("result must be nil while capacity not yet at target")
	}
}

func TestSnapshot_TargetGreater_CapacityAtTarget_ReturnsSuccess(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := makeGuestObj("g1", "default")
	src := makeSourceImagePVC("src-img", "default", "longhorn", "10Gi")
	clone := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RootDiskCloneName("g1"),
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, schema.GroupVersionKind{
				Group: "swift.kubeswift.io", Version: "v1alpha1", Kind: "SwiftGuest",
			})},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:    corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40Gi")},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, src, clone).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{
			PVCName: "src-img",
			CloneSeed: &resolved.PreparedCloneSeed{
				Kind:            "VolumeSnapshot",
				Name:            "vs1",
				Namespace:       "default",
				SourceSizeBytes: 10 * 1024 * 1024 * 1024,
			},
		},
		RootDisk: resolved.RootDisk{Size: resource.MustParse("40Gi")},
	}
	res, err := r.EnsureRootDiskClone(context.Background(), guest, rg)
	if err != nil {
		t.Fatalf("err = %v, want nil once capacity is at target", err)
	}
	if !res.NeedsGrowInit {
		t.Errorf("NeedsGrowInit = false, want true when target > source")
	}
	if res.SourceSizeBytes != 10*1024*1024*1024 || res.TargetSizeBytes != 40*1024*1024*1024 {
		t.Errorf("sizes = src=%d tgt=%d", res.SourceSizeBytes, res.TargetSizeBytes)
	}
}

func TestSnapshot_CrossNamespaceSeed_Rejected(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := makeGuestObj("g1", "default")
	src := makeSourceImagePVC("src-img", "default", "longhorn", "10Gi")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, src).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}

	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{
			PVCName: "src-img",
			CloneSeed: &resolved.PreparedCloneSeed{
				Kind:            "VolumeSnapshot",
				Name:            "vs1",
				Namespace:       "other-ns", // cross-namespace — silent fail on k0s 1.34
				SourceSizeBytes: 10 * 1024 * 1024 * 1024,
			},
		},
		RootDisk: resolved.RootDisk{Size: resource.MustParse("40Gi")},
	}
	_, err := r.EnsureRootDiskClone(context.Background(), guest, rg)
	if err == nil || !strings.Contains(err.Error(), "same namespace") {
		t.Errorf("expected cross-namespace rejection, got: %v", err)
	}
}

// Avoid unused-package warning in some build modes.
var _ = ptr.To[int32](0)
