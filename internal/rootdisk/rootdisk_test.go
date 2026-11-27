package rootdisk

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func guest() *metav1.ObjectMeta {
	return &metav1.ObjectMeta{Name: "vm", Namespace: "ns", UID: types.UID("guest-uid")}
}

func genPVC(name string, gen string, ownerUID types.UID) corev1.PersistentVolumeClaim {
	isController := true
	p := corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "ns",
		Labels:      map[string]string{LabelGuest: "vm", LabelRole: RoleRootDisk},
		Annotations: map[string]string{AnnotationGeneration: gen},
	}}
	if ownerUID != "" {
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "swift.kubeswift.io/v1alpha1", Kind: "SwiftGuest", Name: "vm", UID: ownerUID, Controller: &isController}}
	}
	return p
}

func TestNames(t *testing.T) {
	if got := DefaultName("vm"); got != "swiftguest-root-vm" {
		t.Errorf("DefaultName = %q", got)
	}
	if got := GenerationName("vm", 0); got != "swiftguest-root-vm" {
		t.Errorf("GenerationName(0) = %q", got)
	}
	if got := GenerationName("vm", 3); got != "swiftguest-root-vm-g3" {
		t.Errorf("GenerationName(3) = %q", got)
	}
}

func TestSelect(t *testing.T) {
	now := metav1.Now()
	deleting := genPVC("swiftguest-root-vm-g5", "5", "guest-uid")
	deleting.DeletionTimestamp = &now
	wrongRole := genPVC("swiftguest-root-vm-g6", "6", "guest-uid")
	wrongRole.Labels[LabelRole] = "root-disk-pending"
	otherGuest := genPVC("swiftguest-root-vm-g7", "7", "guest-uid")
	otherGuest.Labels[LabelGuest] = "other"

	cases := []struct {
		name     string
		pvcs     []corev1.PersistentVolumeClaim
		wantName string
		wantGen  int
	}{
		{"none", nil, "swiftguest-root-vm", 0},
		{"generation 0 only", []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "swiftguest-root-vm"}}}, "swiftguest-root-vm", 0},
		{"highest wins", []corev1.PersistentVolumeClaim{
			genPVC("swiftguest-root-vm-g1", "1", "guest-uid"),
			genPVC("swiftguest-root-vm-g2", "2", "guest-uid"),
		}, "swiftguest-root-vm-g2", 2},
		{"previous guest's PVC ignored", []corev1.PersistentVolumeClaim{genPVC("swiftguest-root-vm-g4", "4", "old-uid")}, "swiftguest-root-vm", 0},
		{"no controller ignored", []corev1.PersistentVolumeClaim{genPVC("swiftguest-root-vm-g4", "4", "")}, "swiftguest-root-vm", 0},
		{"deleting ignored", []corev1.PersistentVolumeClaim{genPVC("swiftguest-root-vm-g1", "1", "guest-uid"), deleting}, "swiftguest-root-vm-g1", 1},
		{"pending role ignored", []corev1.PersistentVolumeClaim{wrongRole}, "swiftguest-root-vm", 0},
		{"other guest ignored", []corev1.PersistentVolumeClaim{otherGuest}, "swiftguest-root-vm", 0},
		{"bad annotation ignored", []corev1.PersistentVolumeClaim{genPVC("swiftguest-root-vm-gx", "x", "guest-uid")}, "swiftguest-root-vm", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, gen := Select(guest(), tc.pvcs)
			if name != tc.wantName || gen != tc.wantGen {
				t.Errorf("Select = %q, %d; want %q, %d", name, gen, tc.wantName, tc.wantGen)
			}
		})
	}
}

func TestClaimLists(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	g1 := genPVC("swiftguest-root-vm-g1", "1", "guest-uid")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(&g1).Build()
	name, gen, err := Claim(context.Background(), c, guest())
	if err != nil || name != "swiftguest-root-vm-g1" || gen != 1 {
		t.Fatalf("Claim = %q, %d, %v", name, gen, err)
	}
}
