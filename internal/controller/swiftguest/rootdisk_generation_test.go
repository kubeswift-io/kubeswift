package swiftguest

import (
	"context"
	"testing"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
	"github.com/kubeswift-io/kubeswift/internal/rootdisk"
)

func generationGuest() *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "ns", UID: types.UID("vm-uid")}}
}

func genRootPVC(guest *swiftv1alpha1.SwiftGuest, name string, gen string) *corev1.PersistentVolumeClaim {
	p := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: guest.Namespace,
			Labels:          map[string]string{rootdisk.LabelGuest: guest.Name, rootdisk.LabelRole: rootdisk.RoleRootDisk},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, swiftGuestGVK)},
		},
		Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40Gi")},
		}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	if gen != "" {
		p.Annotations = map[string]string{rootdisk.AnnotationGeneration: gen}
	}
	return p
}

// After an in-place restore swapped in generation 1, the guest boots from it:
// no clone Job runs, and the generation-0 PVC is not touched.
func TestEnsureRootDiskClone_UsesTheSwappedInGeneration(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := generationGuest()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		guest,
		genRootPVC(guest, "swiftguest-root-vm", ""),
		genRootPVC(guest, "swiftguest-root-vm-g1", "1"),
	).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}
	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{PVCName: "src-image"},
		RootDisk:      resolved.RootDisk{Size: resource.MustParse("40Gi")},
	}
	res, err := r.EnsureRootDiskClone(context.Background(), guest, rg)
	if err != nil {
		t.Fatalf("EnsureRootDiskClone: %v", err)
	}
	if res.PVCName != "swiftguest-root-vm-g1" || res.NeedsGrowInit {
		t.Errorf("result = %+v, want swiftguest-root-vm-g1 without grow-init", res)
	}
	var jobs batchv1.JobList
	_ = c.List(context.Background(), &jobs)
	if len(jobs.Items) != 0 {
		t.Errorf("a clone Job ran over a restored disk: %v", jobs.Items[0].Name)
	}
}

// A generation PVC that is not Bound holds the guest; it never falls back to
// the generation-0 disk.
func TestEnsureRootDiskClone_UnboundGenerationWaits(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := generationGuest()
	g1 := genRootPVC(guest, "swiftguest-root-vm-g1", "1")
	g1.Status.Phase = corev1.ClaimPending
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, genRootPVC(guest, "swiftguest-root-vm", ""), g1).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}
	rg := &resolved.ResolvedGuest{PreparedImage: resolved.PreparedImage{PVCName: "src-image"}}
	if res, err := r.EnsureRootDiskClone(context.Background(), guest, rg); err == nil {
		t.Fatalf("got %+v, want a not-Bound wait", res)
	}
}

func TestDiskRestoreHold(t *testing.T) {
	scheme := rootdiskScheme(t)
	gvSnap := schema.GroupVersion{Group: "snapshot.kubeswift.io", Version: "v1alpha1"}
	scheme.AddKnownTypes(gvSnap, &snapshotv1alpha1.SwiftRestore{}, &snapshotv1alpha1.SwiftRestoreList{})
	metav1.AddToGroupVersion(scheme, gvSnap)

	cases := []struct {
		name  string
		phase snapshotv1alpha1.SwiftRestorePhase
		exist bool
		want  string
	}{
		{"restoring holds", snapshotv1alpha1.SwiftRestorePhaseRestoring, true, "r1"},
		{"ready releases", snapshotv1alpha1.SwiftRestorePhaseReady, true, ""},
		{"failed releases", snapshotv1alpha1.SwiftRestorePhaseFailed, true, ""},
		{"deleted restore releases", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guest := generationGuest()
			guest.Annotations = map[string]string{snapshotv1alpha1.AnnotationDiskRestore: "r1"}
			objs := []client.Object{guest}
			if tc.exist {
				objs = append(objs, &snapshotv1alpha1.SwiftRestore{
					ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns"},
					Status:     snapshotv1alpha1.SwiftRestoreStatus{Phase: tc.phase},
				})
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			r := &SwiftGuestReconciler{Client: c, APIReader: c, Scheme: scheme}
			got, err := r.diskRestoreHold(context.Background(), guest)
			if err != nil || got != tc.want {
				t.Errorf("diskRestoreHold = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// The launcher about to be created mounts a root disk resolved from the cache;
// an uncached read that disagrees stops the create.
func TestRootDiskStale(t *testing.T) {
	scheme := rootdiskScheme(t)
	guest := generationGuest()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, genRootPVC(guest, "swiftguest-root-vm-g1", "1")).Build()
	r := &SwiftGuestReconciler{Client: c, APIReader: c, Scheme: scheme}
	if stale, err := r.rootDiskStale(context.Background(), guest, "swiftguest-root-vm"); err != nil || !stale {
		t.Errorf("generation-0 claim after a swap: stale=%v err=%v, want stale", stale, err)
	}
	if stale, err := r.rootDiskStale(context.Background(), guest, "swiftguest-root-vm-g1"); err != nil || stale {
		t.Errorf("current claim: stale=%v err=%v, want not stale", stale, err)
	}
}

// A retired root disk goes once no VolumeSnapshot was taken from it; one
// that still has a snapshot stays.
func TestGCRetiredRootDisks(t *testing.T) {
	scheme := rootdiskScheme(t)
	if err := volumesnapshotv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	guest := generationGuest()
	retired := func(name string) *corev1.PersistentVolumeClaim {
		p := genRootPVC(guest, name, "")
		p.Labels[rootdisk.LabelRole] = rootdisk.RoleRetired
		return p
	}
	src := "swiftguest-root-vm-g1"
	vs := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "vs", Namespace: "ns"},
		Spec:       volumesnapshotv1.VolumeSnapshotSpec{Source: volumesnapshotv1.VolumeSnapshotSource{PersistentVolumeClaimName: &src}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, retired("swiftguest-root-vm"), retired(src), vs).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}
	if err := r.gcRetiredRootDisks(context.Background(), guest); err != nil {
		t.Fatal(err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Name: "swiftguest-root-vm", Namespace: "ns"}, &pvc); err == nil {
		t.Error("retired disk without snapshots kept")
	}
	if err := c.Get(context.Background(), client.ObjectKey{Name: src, Namespace: "ns"}, &pvc); err != nil {
		t.Errorf("retired disk with a snapshot deleted: %v", err)
	}
}
