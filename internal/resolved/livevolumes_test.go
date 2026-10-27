package resolved

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func livePVC(name string, mode corev1.PersistentVolumeAccessMode, block bool) *corev1.PersistentVolumeClaim {
	vm := corev1.PersistentVolumeFilesystem
	if block {
		vm = corev1.PersistentVolumeBlock
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{mode}, VolumeMode: &vm},
	}
}

// #877: every volume the launcher holds besides the root disk must attach on
// the destination while the source still holds it.
func TestLiveMigrationVolumesError(t *testing.T) {
	blank := func(name string) swiftv1alpha1.DataDiskRef {
		return swiftv1alpha1.DataDiskRef{Name: name, Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("1Gi")}}
	}
	pvcDisk := func(name, pvc string, asDisk bool) swiftv1alpha1.DataDiskRef {
		return swiftv1alpha1.DataDiskRef{Name: name, PVCRef: &corev1.LocalObjectReference{Name: pvc}, AttachAsDisk: asDisk}
	}
	fsPVC := func(name, pvc string) swiftv1alpha1.Filesystem {
		return swiftv1alpha1.Filesystem{Name: name, Source: swiftv1alpha1.FilesystemSource{PVCRef: &corev1.LocalObjectReference{Name: pvc}}}
	}
	hostPath := "/srv/share"
	cases := []struct {
		name    string
		spec    func(*swiftv1alpha1.SwiftGuestSpec)
		objs    []client.Object
		blocked string // substring of the gate; "" = live-capable
	}{
		{"no extra volumes", func(*swiftv1alpha1.SwiftGuestSpec) {}, nil, ""},
		{"blank RWO (the controller default)", func(s *swiftv1alpha1.SwiftGuestSpec) { s.DataDiskRefs = []swiftv1alpha1.DataDiskRef{blank("scratch")} },
			[]client.Object{livePVC(BlankDataDiskPVCName("g", "scratch"), corev1.ReadWriteOnce, true)}, `data disk "scratch"`},
		{"blank RWX Block", func(s *swiftv1alpha1.SwiftGuestSpec) { s.DataDiskRefs = []swiftv1alpha1.DataDiskRef{blank("scratch")} },
			[]client.Object{livePVC(BlankDataDiskPVCName("g", "scratch"), corev1.ReadWriteMany, true)}, ""},
		{"blank RWX Filesystem", func(s *swiftv1alpha1.SwiftGuestSpec) { s.DataDiskRefs = []swiftv1alpha1.DataDiskRef{blank("scratch")} },
			[]client.Object{livePVC(BlankDataDiskPVCName("g", "scratch"), corev1.ReadWriteMany, false)}, "volumeMode Filesystem"},
		{"blank PVC missing", func(s *swiftv1alpha1.SwiftGuestSpec) { s.DataDiskRefs = []swiftv1alpha1.DataDiskRef{blank("scratch")} },
			nil, "does not exist"},
		{"image-backed dataDiskRefs (guest copy RWO)", func(s *swiftv1alpha1.SwiftGuestSpec) {
			s.DataDiskRefs = []swiftv1alpha1.DataDiskRef{{Name: "seeded", ImageRef: &corev1.LocalObjectReference{Name: "img"}}}
		}, []client.Object{livePVC(BlankDataDiskPVCName("g", "seeded"), corev1.ReadWriteOnce, true)}, `data disk "seeded"`},
		{"image-backed dataDiskRef (no copy yet)", func(s *swiftv1alpha1.SwiftGuestSpec) { s.DataDiskRef = &corev1.LocalObjectReference{Name: "img"} },
			nil, `data disk "data" PVC "g-data-data" does not exist`},
		{"pvcRef attachAsDisk RWX Block", func(s *swiftv1alpha1.SwiftGuestSpec) {
			s.DataDiskRefs = []swiftv1alpha1.DataDiskRef{pvcDisk("d", "user-pvc", true)}
		},
			[]client.Object{livePVC("user-pvc", corev1.ReadWriteMany, true)}, ""},
		{"pvcRef attachAsDisk RWX Filesystem", func(s *swiftv1alpha1.SwiftGuestSpec) {
			s.DataDiskRefs = []swiftv1alpha1.DataDiskRef{pvcDisk("d", "user-pvc", true)}
		},
			[]client.Object{livePVC("user-pvc", corev1.ReadWriteMany, false)}, "volumeMode Filesystem"},
		{"pvcRef mounted RWX Filesystem", func(s *swiftv1alpha1.SwiftGuestSpec) {
			s.DataDiskRefs = []swiftv1alpha1.DataDiskRef{pvcDisk("d", "user-pvc", false)}
		},
			[]client.Object{livePVC("user-pvc", corev1.ReadWriteMany, false)}, ""},
		{"pvcRef mounted RWO", func(s *swiftv1alpha1.SwiftGuestSpec) {
			s.DataDiskRefs = []swiftv1alpha1.DataDiskRef{pvcDisk("d", "user-pvc", false)}
		},
			[]client.Object{livePVC("user-pvc", corev1.ReadWriteOnce, false)}, "not ReadWriteMany"},
		{"filesystem PVC RWO", func(s *swiftv1alpha1.SwiftGuestSpec) {
			s.Filesystems = []swiftv1alpha1.Filesystem{fsPVC("share", "fs-pvc")}
		},
			[]client.Object{livePVC("fs-pvc", corev1.ReadWriteOnce, false)}, `filesystem "share"`},
		{"filesystem PVC RWX", func(s *swiftv1alpha1.SwiftGuestSpec) {
			s.Filesystems = []swiftv1alpha1.Filesystem{fsPVC("share", "fs-pvc")}
		},
			[]client.Object{livePVC("fs-pvc", corev1.ReadWriteMany, false)}, ""},
		{"filesystem hostPath (no PVC)", func(s *swiftv1alpha1.SwiftGuestSpec) {
			s.Filesystems = []swiftv1alpha1.Filesystem{{Name: "share", Source: swiftv1alpha1.FilesystemSource{HostPath: &hostPath}}}
		}, nil, ""},
		{"kernel boot with an RWO data disk", func(s *swiftv1alpha1.SwiftGuestSpec) {
			s.KernelRef = &corev1.LocalObjectReference{Name: "k"}
			s.DataDiskRefs = []swiftv1alpha1.DataDiskRef{blank("scratch")}
		}, []client.Object{livePVC(BlankDataDiskPVCName("g", "scratch"), corev1.ReadWriteOnce, true)}, `data disk "scratch"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}}
			tc.spec(&guest.Spec)
			c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(tc.objs...).Build()
			gate, err := LiveMigrationVolumesError(context.Background(), c, guest)
			if err != nil {
				t.Fatalf("unexpected read error: %v", err)
			}
			switch {
			case tc.blocked == "" && gate != nil:
				t.Fatalf("want live-capable, got %v", gate)
			case tc.blocked != "" && gate == nil:
				t.Fatalf("want refused (%s), got live-capable", tc.blocked)
			case tc.blocked != "" && (!strings.Contains(gate.Error(), tc.blocked) || !strings.Contains(gate.Error(), "spec.mode=offline")):
				t.Fatalf("gate %q should name %q and offer spec.mode=offline", gate, tc.blocked)
			}
		})
	}
}

// The PVC's bound access modes win over the requested ones.
func TestLiveMigrationVolumesError_BoundAccessModes(t *testing.T) {
	pvc := livePVC("user-pvc", corev1.ReadWriteMany, true)
	pvc.Status.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{DataDiskRefs: []swiftv1alpha1.DataDiskRef{{Name: "d", PVCRef: &corev1.LocalObjectReference{Name: "user-pvc"}, AttachAsDisk: true}}}}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(pvc).WithStatusSubresource(pvc).Build()
	if err := c.Status().Update(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	gate, err := LiveMigrationVolumesError(context.Background(), c, guest)
	if err != nil || gate == nil {
		t.Fatalf("bound RWO must refuse live migration; gate=%v err=%v", gate, err)
	}
}
