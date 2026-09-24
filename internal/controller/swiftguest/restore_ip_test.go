package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
)

// A guest restored in place resumes its memory, network configuration
// included, so it never asks DHCP for an address and the launcher's lease
// poller finds none. A new launcher clears the address the last one reported,
// so the restore launcher must be created knowing it: the one the snapshot
// recorded at capture. (The nightly local-roundtrip e2e found the restored
// guest with no status.network.primaryIP.)
func TestBuildPod_InPlaceRestoreCarriesTheCapturedAddress(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		snapGuest string
		snapIP    string
		noRestore bool
		want      string
	}{
		{name: "in-place resumes with the captured address", mode: RestoreModeInPlace, snapGuest: "g1", snapIP: "192.168.99.14", want: "192.168.99.14"},
		{name: "a clone takes a lease of its own", mode: RestoreModeClone, snapGuest: "g1", snapIP: "192.168.99.14"},
		{name: "a snapshot of another guest", mode: RestoreModeInPlace, snapGuest: "other", snapIP: "192.168.99.14"},
		{name: "no address recorded", mode: RestoreModeInPlace, snapGuest: "g1"},
		{name: "not an address", mode: RestoreModeInPlace, snapGuest: "g1", snapIP: "192.168.99.14 x"},
		{name: "the restore is gone", mode: RestoreModeInPlace, snapGuest: "g1", snapIP: "192.168.99.14", noRestore: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guest := minimalGuest()
			guest.Annotations[AnnotationRestoreMode] = tc.mode
			guest.Annotations[AnnotationRestoreSnapshotPath] = clonecommon.SnapshotDir("default", "snap1")
			objs := []client.Object{&snapshotv1alpha1.SwiftSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: "snap1", Namespace: "default"},
				Spec:       snapshotv1alpha1.SwiftSnapshotSpec{GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: tc.snapGuest}},
				Status: snapshotv1alpha1.SwiftSnapshotStatus{
					GuestSpec: &snapshotv1alpha1.CapturedGuestSpec{PrimaryIP: tc.snapIP},
				},
			}}
			if !tc.noRestore {
				objs = append(objs, &snapshotv1alpha1.SwiftRestore{
					ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "default"},
					Spec: snapshotv1alpha1.SwiftRestoreSpec{
						SnapshotRef: snapshotv1alpha1.SwiftRestoreSnapshotRef{Name: "snap1"},
					},
				})
			}
			r := &SwiftGuestReconciler{Client: guestClientBuilder(objs...).Build(), Scheme: scheme.Scheme}

			pod, err := r.buildPod(context.Background(), guest, minimalResolved(), "", "g1-runtime-intent", nil)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			if got := pod.Annotations[PodAnnotationGuestIP]; got != tc.want {
				t.Errorf("launcher guest-ip = %q, want %q", got, tc.want)
			}
		})
	}
}

// The address the restore launcher is created with is what the guest's
// status reports once that launcher is its pod — the new launcher's clear of
// the last run's state does not lose it.
func TestMapPodToStatus_RestoreLauncherReportsTheCarriedAddress(t *testing.T) {
	guest := minimalGuest()
	guest.Annotations[AnnotationRestoreMode] = RestoreModeInPlace
	guest.Annotations[AnnotationRestoreSnapshotPath] = clonecommon.SnapshotDir("default", "snap1")
	r := &SwiftGuestReconciler{Client: guestClientBuilder(
		&snapshotv1alpha1.SwiftRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "default"},
			Spec:       snapshotv1alpha1.SwiftRestoreSpec{SnapshotRef: snapshotv1alpha1.SwiftRestoreSnapshotRef{Name: "snap1"}},
		},
		&snapshotv1alpha1.SwiftSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "snap1", Namespace: "default"},
			Spec:       snapshotv1alpha1.SwiftSnapshotSpec{GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "g1"}},
			Status:     snapshotv1alpha1.SwiftSnapshotStatus{GuestSpec: &snapshotv1alpha1.CapturedGuestSpec{PrimaryIP: "192.168.99.14"}},
		},
	).Build(), Scheme: scheme.Scheme}
	pod, err := r.buildPod(context.Background(), guest, minimalResolved(), "", "g1-runtime-intent", nil)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	pod.UID = "restore-launcher"

	status := guest.Status.DeepCopy()
	status.PodRef = &corev1.ObjectReference{Name: "g1", UID: "captured-launcher"}
	status.Network = &swiftv1alpha1.GuestNetworkStatus{PrimaryIP: "192.168.99.14", Ready: true}
	MapPodToStatus(pod, status)
	if status.Network == nil || status.Network.PrimaryIP != "192.168.99.14" {
		t.Errorf("status.network after the restore launcher = %+v, want primaryIP 192.168.99.14", status.Network)
	}
}
