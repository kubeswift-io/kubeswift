package swiftguest

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// Lab validation of v0.15.0, round 5: local-roundtrip-test.sh applies its
// guest before its SwiftImage. The first reconcile found no image and marked
// the guest Failed, and it stayed Failed through the whole import, although
// every later pass was only waiting for the image.
func TestReconcile_GuestAppliedBeforeItsImageWaits(t *testing.T) {
	c := guestClientBuilder(asDiskBoot(kernelGuest()), testGuestClass()).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	got, res, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhasePending {
		t.Errorf("phase = %q, want Pending while the image does not exist yet", got.Status.Phase)
	}
	cond := guestCondition(t, got, ConditionResolved)
	if cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "SwiftImage not found") {
		t.Errorf("Resolved = %s %q; want False saying the image is not found", cond.Status, cond.Message)
	}
	if res.RequeueAfter <= 0 {
		t.Error("no requeue: the guest would not notice the image being created")
	}
	if n := len(launcherPods(t, c)); n != 0 {
		t.Errorf("%d launcher pods before the image exists", n)
	}

	importing := readyImage()
	importing.Status.Phase = imagev1alpha1.SwiftImagePhaseImporting
	if err := c.Create(context.Background(), importing); err != nil {
		t.Fatal(err)
	}
	got, _, err = reconcileGuest(t, r)
	if err != nil {
		t.Fatalf("reconcile while importing: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhasePending {
		t.Errorf("while importing: phase = %q, want Pending", got.Status.Phase)
	}
}

// A guest that went Failed on resolution before this fix (or whose failed
// image was since recreated) goes back to Pending once it is only waiting.
func TestReconcile_ResolutionFailedGuestGoesBackToPendingWhileWaiting(t *testing.T) {
	g := asDiskBoot(kernelGuest())
	g.Status.Phase = swiftv1alpha1.SwiftGuestPhaseFailed
	SetResolvedCondition(&g.Status, false, "SwiftImage not found: swiftimages \"img\" not found")
	importing := readyImage()
	importing.Status.Phase = imagev1alpha1.SwiftImagePhaseImporting
	c := guestClientBuilder(g, testGuestClass(), importing).Build()
	got, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhasePending {
		t.Errorf("phase = %q, want Pending: the guest is waiting for its image, not failed", got.Status.Phase)
	}
}

// A guest that failed at run time (it resolved, then its launcher failed)
// keeps Failed: going back to Pending is only for a guest that never got past
// resolution.
func TestReconcile_RuntimeFailedGuestStaysFailedWhileWaiting(t *testing.T) {
	g := asDiskBoot(kernelGuest())
	g.Status.Phase = swiftv1alpha1.SwiftGuestPhaseFailed
	SetResolvedCondition(&g.Status, true, "")
	importing := readyImage()
	importing.Status.Phase = imagev1alpha1.SwiftImagePhaseImporting
	c := guestClientBuilder(g, testGuestClass(), importing).Build()
	got, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseFailed {
		t.Errorf("phase = %q, want Failed kept", got.Status.Phase)
	}
}

// Lab validation of v0.15.0, round 5: a guest whose image had failed showed
// only Failed and "SwiftImage not Ready", which reads the same as an image
// still importing, and recorded no event.
func TestReconcile_FailedImageSaysSoOnTheGuest(t *testing.T) {
	failed := readyImage()
	failed.Status.Phase = imagev1alpha1.SwiftImagePhaseFailed
	failed.Status.Conditions = []metav1.Condition{{
		Type: "Failed", Status: metav1.ConditionTrue, Reason: "ImportFailed",
		Message: "Job has reached the specified backoff limit",
	}}
	c := guestClientBuilder(asDiskBoot(kernelGuest()), testGuestClass(), failed).Build()
	rec := record.NewFakeRecorder(10)
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme, Recorder: rec}
	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	const want = "SwiftImage failed: Job has reached the specified backoff limit"
	if cond := guestCondition(t, got, ConditionResolved); cond.Message != want {
		t.Errorf("Resolved message = %q, want %q", cond.Message, want)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "ResolutionFailed") || !strings.Contains(ev, want) {
			t.Errorf("event = %q, want a ResolutionFailed warning carrying %q", ev, want)
		}
	default:
		t.Fatal("no event recorded for the failed resolution")
	}

	// Later passes while the image stays Failed record nothing new.
	if _, _, err := reconcileGuest(t, r); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	select {
	case ev := <-rec.Events:
		t.Errorf("second pass recorded %q; the failure was already reported", ev)
	default:
	}
}
