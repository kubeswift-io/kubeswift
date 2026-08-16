package swiftguest

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// A CSI VolumeSnapshot clones the source VOLUME. The SwiftImage import PVC is
// Filesystem-mode and holds the disk as a file inside it, so cloning it into a
// Block root disk yields a block device whose content is a filesystem — an
// unbootable guest that still reports Running. cloneSeedModeMismatch is the
// guard that routes those clones to the copy path instead.
func TestCloneSeedModeMismatch(t *testing.T) {
	fs := corev1.PersistentVolumeFilesystem
	blk := corev1.PersistentVolumeBlock

	imagePVC := func(mode *corev1.PersistentVolumeMode) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "swiftimage-import-x", Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: mode},
		}
	}

	tests := []struct {
		name         string
		sourceMode   *corev1.PersistentVolumeMode
		targetMode   string // resolved storage: "" means Filesystem
		wantMismatch bool
	}{
		// The defect: image PVC is Filesystem, migratable guest class is Block.
		{"filesystem source into block root disk", &fs, string(blk), true},
		// The reverse is equally unsound.
		{"block source into filesystem root disk", &blk, string(fs), true},
		// Matching modes are exactly what the snapshot path is for — a CoW
		// clone here must NOT be downgraded, or Ceph/EBS lose their whole point.
		{"filesystem into filesystem keeps the snapshot path", &fs, string(fs), false},
		{"block into block keeps the snapshot path", &blk, string(blk), false},
		// A nil volumeMode means Filesystem on both sides of the comparison.
		{"nil source and empty target both mean filesystem", nil, "", false},
		{"nil source (filesystem) into block still mismatches", nil, string(blk), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(imagePVC(tc.sourceMode)).
				Build()
			r := &SwiftGuestReconciler{Client: cl, Scheme: scheme.Scheme}

			rg := &resolved.ResolvedGuest{}
			rg.Storage.VolumeMode = tc.targetMode

			got, err := r.cloneSeedModeMismatch(context.Background(), "default", "swiftimage-import-x", rg)
			if err != nil {
				t.Fatalf("cloneSeedModeMismatch: %v", err)
			}
			if tc.wantMismatch && got == "" {
				t.Fatalf("expected a mismatch (the clone would produce an unbootable disk), got none")
			}
			if !tc.wantMismatch && got != "" {
				t.Fatalf("expected the snapshot path to be kept, got downgrade reason: %s", got)
			}
			if tc.wantMismatch && !strings.Contains(got, "unbootable") {
				t.Errorf("downgrade reason should say why it matters, got: %s", got)
			}
		})
	}
}

// A missing image PVC is a different failure with a better message downstream;
// the guard must not swallow it as a "mismatch" and silently downgrade.
func TestCloneSeedModeMismatch_MissingSourcePVCIsNotAMismatch(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	r := &SwiftGuestReconciler{Client: cl, Scheme: scheme.Scheme}

	rg := &resolved.ResolvedGuest{}
	rg.Storage.VolumeMode = string(corev1.PersistentVolumeBlock)

	got, err := r.cloneSeedModeMismatch(context.Background(), "default", "does-not-exist", rg)
	if err != nil {
		t.Fatalf("a missing source PVC must not be a hard error here: %v", err)
	}
	if got != "" {
		t.Fatalf("a missing source PVC must not report a mode mismatch, got: %s", got)
	}
}
