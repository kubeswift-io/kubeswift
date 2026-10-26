package swiftmigration

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// #877: a guest whose root disk is live-capable but whose data disk is not
// used to pass Validating, then fail ~60 s later in Preparing with
// DstNeverReady (Multi-Attach on the destination). The data volumes are now
// part of the live-migration storage gate.

func dataDiskPVC(guest, disk string, mode corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
	block := corev1.PersistentVolumeBlock
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: resolved.BlankDataDiskPVCName(guest, disk), Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{mode}, VolumeMode: &block},
	}
}

func withBlankDataDisk(g *swiftv1alpha1.SwiftGuest) {
	g.Spec.DataDiskRefs = []swiftv1alpha1.DataDiskRef{{Name: "scratch", Blank: &swiftv1alpha1.BlankDiskSpec{Size: resource.MustParse("2Gi")}}}
}

// Explicit mode=live with an RWO data disk: refused at Validating, before any
// destination pod exists, with EligibilityMismatch naming the disk.
func TestValidatingLive_RWODataDisk_RefusedAtValidating(t *testing.T) {
	objs := explicitObjs("guest", oldLauncher)
	withBlankDataDisk(objs[0].(*swiftv1alpha1.SwiftGuest))
	objs = append(objs, dataDiskPVC("guest", "scratch", corev1.ReadWriteOnce))

	dst, res := migrateToPreparing(t, objs, liveMigration(), validateLive)
	if dst != nil {
		t.Fatal("a destination pod was created")
	}
	if res == nil || res.FailureReason != migrationv1alpha1.FailureReasonEligibilityMismatch {
		t.Fatalf("got %+v, want Failed/EligibilityMismatch at Validating", res)
	}
	if !strings.Contains(res.FailureMsg, `data disk "scratch"`) || !strings.Contains(res.FailureMsg, "spec.mode=offline") {
		t.Errorf("message should name the data disk and offer offline: %q", res.FailureMsg)
	}
}

// An RWX+Block data disk is live-capable: the migration proceeds.
func TestValidatingLive_RWXBlockDataDisk_Passes(t *testing.T) {
	objs := explicitObjs("guest", oldLauncher)
	withBlankDataDisk(objs[0].(*swiftv1alpha1.SwiftGuest))
	objs = append(objs, dataDiskPVC("guest", "scratch", corev1.ReadWriteMany))

	if _, res := migrateToPreparing(t, objs, liveMigration(), validateLive); res != nil {
		t.Fatalf("RWX+Block data disk refused: %+v", res)
	}
}

// mode=auto (every node drain with drainPolicy Migrate) with an RWO data disk
// resolves to offline instead of a live attempt that cannot succeed; with an
// RWX+Block data disk it stays live.
func TestAutoMode_DataDiskDecidesLiveOrOffline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     corev1.PersistentVolumeAccessMode
		wantLive bool
	}{
		{"RWO data disk -> offline", corev1.ReadWriteOnce, false},
		{"RWX+Block data disk -> live", corev1.ReadWriteMany, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := validatingScheme(t)
			enabled := true
			guest := newGuestForValidating("drained", "default", "live-class")
			guest.Spec.Migration = &swiftv1alpha1.MigrationSpec{Enabled: &enabled, PreferredMode: "auto"}
			guest.Spec.Interfaces = nil
			guest.Status.NodeName = "worker-2"
			withBlankDataDisk(guest)
			mig := newMigration("drain-m", "default")
			mig.Spec.GuestRef.Name = "drained"
			mig.Spec.Target.NodeName = "worker-1"
			mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeAuto
			mig.Spec.AllowIPChange = true
			mig.Spec.Timeout = &metav1.Duration{Duration: 5 * 60 * 1e9}
			mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating
			objs := []client.Object{mig, guest, newGuestClass("live-class", 2, 4096), newSpaciousNode("worker-1", 8, 65536),
				launcherSourcePod("drained", oldLauncher), dataDiskPVC("drained", "scratch", tc.mode)}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(mig).Build()
			r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

			status := mig.Status.DeepCopy()
			res := r.handleValidating(context.Background(), mig, status)
			if res.FailureMsg != "" || res.Err != nil {
				t.Fatalf("auto resolution failed: %+v", res)
			}
			if gotLive := status.Mode == migrationv1alpha1.SwiftMigrationModeLive; gotLive != tc.wantLive {
				t.Fatalf("status.mode=%q, want live=%v", status.Mode, tc.wantLive)
			}
		})
	}
}

// Explicit mode=live for a guest whose devices, backends or network stay with
// the source node: refused at Validating with EligibilityMismatch, as auto
// mode and the (off-by-default) webhook already do, instead of failing later.
func TestValidatingLive_NodeBoundGuest_RefusedAtValidating(t *testing.T) {
	share := "/srv/share"
	for _, tc := range []struct {
		name  string
		setup func(g *swiftv1alpha1.SwiftGuest) []client.Object
		want  string
	}{
		{"GPU (VFIO)", func(g *swiftv1alpha1.SwiftGuest) []client.Object {
			g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu"}
			return nil
		}, "VFIO devices"},
		{"virtio-fs filesystem", func(g *swiftv1alpha1.SwiftGuest) []client.Object {
			g.Spec.Filesystems = []swiftv1alpha1.Filesystem{{Name: "share", Source: swiftv1alpha1.FilesystemSource{HostPath: &share}}}
			return nil
		}, "node-local virtio backends"},
		{"primary UDN namespace", func(g *swiftv1alpha1.SwiftGuest) []client.Object {
			return []client.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default",
				Labels: map[string]string{resolved.OVNPrimaryUDNNamespaceLabel: ""}}}}
		}, "primary user-defined network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := explicitObjs("guest", oldLauncher)
			objs = append(objs, tc.setup(objs[0].(*swiftv1alpha1.SwiftGuest))...)
			dst, res := migrateToPreparing(t, objs, liveMigration(), validateLive)
			if dst != nil {
				t.Fatal("a destination pod was created")
			}
			if res == nil || res.FailureReason != migrationv1alpha1.FailureReasonEligibilityMismatch || !strings.Contains(res.FailureMsg, tc.want) {
				t.Fatalf("got %+v, want Failed/EligibilityMismatch naming %q", res, tc.want)
			}
		})
	}
}
