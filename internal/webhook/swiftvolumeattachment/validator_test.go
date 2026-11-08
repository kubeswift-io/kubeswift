package swiftvolumeattachment

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	gv := schema.GroupVersion{Group: swiftv1alpha1.GroupName, Version: swiftv1alpha1.Version}
	s.AddKnownTypes(gv, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{},
		&swiftv1alpha1.SwiftVolumeAttachment{}, &swiftv1alpha1.SwiftVolumeAttachmentList{})
	metav1.AddToGroupVersion(s, gv)
	return s
}

func TestValidateCreate(t *testing.T) {
	block, fs := corev1.PersistentVolumeBlock, corev1.PersistentVolumeFilesystem
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{GuestAgent: &swiftv1alpha1.GuestAgentSpec{Enabled: true}}}
	noAgent := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "ns"}}
	pvc := func(mode corev1.PersistentVolumeMode, pv string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "ns"},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeMode: &mode, VolumeName: pv}}
	}
	nfs := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-nfs"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{}}}}
	existing := &swiftv1alpha1.SwiftVolumeAttachment{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftVolumeAttachmentSpec{GuestRef: corev1.LocalObjectReference{Name: "vm"}, PVCRef: corev1.LocalObjectReference{Name: "data"}}}

	cases := []struct {
		name     string
		disabled bool
		objs     []client.Object
		refuse   string
		warn     string
	}{
		{"ok", false, []client.Object{guest, pvc(block, "")}, "", ""},
		{"feature off", true, []client.Object{guest, pvc(block, "")}, "not enabled", ""},
		{"no agent", false, []client.Object{noAgent, pvc(block, "")}, "guestAgent", ""},
		{"filesystem", false, []client.Object{guest, pvc(fs, "")}, "volumeMode Filesystem", ""},
		{"not csi", false, []client.Object{guest, pvc(block, "pv-nfs"), nfs}, "not a CSI volume", ""},
		{"duplicate", false, []client.Object{guest, pvc(block, ""), existing}, "already attaches", ""},
		{"nothing yet", false, nil, "", "does not exist yet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &Validator{Client: fake.NewClientBuilder().WithScheme(scheme()).WithObjects(tc.objs...).Build(), Enabled: !tc.disabled}
			sva := &swiftv1alpha1.SwiftVolumeAttachment{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "ns"},
				Spec: swiftv1alpha1.SwiftVolumeAttachmentSpec{GuestRef: corev1.LocalObjectReference{Name: "vm"}, PVCRef: corev1.LocalObjectReference{Name: "data"}}}
			warns, err := v.ValidateCreate(context.Background(), sva)
			if tc.refuse == "" && err != nil || tc.refuse != "" && (err == nil || !strings.Contains(err.Error(), tc.refuse)) {
				t.Fatalf("err %v, want %q", err, tc.refuse)
			}
			if tc.warn != "" && (len(warns) == 0 || !strings.Contains(warns[0], tc.warn)) {
				t.Fatalf("warnings %v", warns)
			}
		})
	}
}
