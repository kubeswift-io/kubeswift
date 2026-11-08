package hotplug

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func TestSerialFitsVirtio(t *testing.T) {
	s := swiftv1alpha1.HotplugSerial("9f2c41d0-7b3e-8a6c-55aa-0123456789ab")
	if s != "ks9f2c41d07b3e8a6c55" || len(s) != 20 {
		t.Fatalf("serial %q (%d bytes)", s, len(s))
	}
}

func TestEncodeIsSortedAndStable(t *testing.T) {
	a, _ := EncodeVolumes([]Volume{{Name: "b"}, {Name: "a"}})
	b, _ := EncodeVolumes([]Volume{{Name: "a"}, {Name: "b"}})
	if a != b || !strings.HasPrefix(a, `[{"name":"a"`) {
		t.Fatalf("%s vs %s", a, b)
	}
	if e, _ := EncodeVolumes(nil); e != "[]" {
		t.Fatalf("empty set %q", e)
	}
}

func TestSettled(t *testing.T) {
	set := `[{"name":"a"}]`
	cases := []struct {
		ann  map[string]string
		want bool
	}{
		{map[string]string{}, true},
		{map[string]string{AnnotationVolumes: set}, false},
		{map[string]string{AnnotationVolumes: set, AnnotationStatus: "[]", AnnotationObserved: SetHash(`[]`)}, false},
		{map[string]string{AnnotationVolumes: set, AnnotationStatus: "[]", AnnotationObserved: SetHash(set)}, true},
		{map[string]string{AnnotationStatus: "[]", AnnotationObserved: SetHash("")}, true},
	}
	for i, tc := range cases {
		if got := Settled(tc.ann); got != tc.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}

func TestHasLauncherFeature(t *testing.T) {
	if !HasLauncherFeature("artifact-requirements hotplug-volumes migration-events") || HasLauncherFeature("hotplug-volumes-x") {
		t.Fatal("feature match")
	}
}

func sva(name, guest, claim string, ro bool, age int) swiftv1alpha1.SwiftVolumeAttachment {
	return swiftv1alpha1.SwiftVolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(time.Unix(int64(age), 0))},
		Spec: swiftv1alpha1.SwiftVolumeAttachmentSpec{
			GuestRef: corev1.LocalObjectReference{Name: guest}, PVCRef: corev1.LocalObjectReference{Name: claim}, ReadOnly: ro,
		},
	}
}

func rwx() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}}}
}

func TestSiblingRefusal(t *testing.T) {
	rwo := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}}}
	cases := []struct {
		name    string
		sva     swiftv1alpha1.SwiftVolumeAttachment
		earlier []swiftv1alpha1.SwiftVolumeAttachment
		pvc     *corev1.PersistentVolumeClaim
		refuse  string
	}{
		{"alone", sva("a", "g1", "c", false, 2), nil, rwo, ""},
		{"same guest same claim", sva("b", "g1", "c", false, 2), []swiftv1alpha1.SwiftVolumeAttachment{sva("a", "g1", "c", false, 1)}, rwo, "already attaches"},
		{"two guests, writable", sva("b", "g2", "c", false, 2), []swiftv1alpha1.SwiftVolumeAttachment{sva("a", "g1", "c", true, 1)}, rwx(), "readOnly"},
		{"two guests, read-only RWO", sva("b", "g2", "c", true, 2), []swiftv1alpha1.SwiftVolumeAttachment{sva("a", "g1", "c", true, 1)}, rwo, "not ReadWriteMany"},
		{"two guests, read-only RWX", sva("b", "g2", "c", true, 2), []swiftv1alpha1.SwiftVolumeAttachment{sva("a", "g1", "c", true, 1)}, rwx(), ""},
		{"other claims", sva("b", "g1", "d", false, 2), []swiftv1alpha1.SwiftVolumeAttachment{sva("a", "g1", "c", false, 1)}, rwo, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SiblingRefusal(&tc.sva, tc.earlier, tc.pvc)
			if (tc.refuse == "") != (got == "") || !strings.Contains(got, tc.refuse) {
				t.Fatalf("got %q, want %q", got, tc.refuse)
			}
		})
	}
}

func TestCap(t *testing.T) {
	var earlier []swiftv1alpha1.SwiftVolumeAttachment
	for i := 0; i < swiftv1alpha1.MaxVolumeAttachmentsPerGuest; i++ {
		earlier = append(earlier, sva(string(rune('a'+i)), "g", string(rune('a'+i)), false, i))
	}
	s := sva("z", "g", "z", false, 99)
	if got := SiblingRefusal(&s, earlier, nil); !strings.Contains(got, "16 volume attachments") {
		t.Fatalf("got %q", got)
	}
	if got := SiblingRefusal(&s, earlier[1:], nil); got != "" {
		t.Fatalf("15 earlier must pass: %q", got)
	}
}

func TestGuestRefusal(t *testing.T) {
	g := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g"}}
	if !strings.Contains(GuestRefusal(g), "guestAgent") {
		t.Fatal("agent required")
	}
	g.Spec.GuestAgent = &swiftv1alpha1.GuestAgentSpec{Enabled: true}
	if GuestRefusal(g) != "" {
		t.Fatal("plain CH guest with agent passes")
	}
	g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
	if !strings.Contains(GuestRefusal(g), "GPU") {
		t.Fatal("GPU refused")
	}
	g.Spec.GPUProfileRef = nil
	g.Annotations = map[string]string{"kubeswift.io/hypervisor-override": "qemu"}
	if !strings.Contains(GuestRefusal(g), "QEMU") {
		t.Fatal("QEMU refused")
	}
}
