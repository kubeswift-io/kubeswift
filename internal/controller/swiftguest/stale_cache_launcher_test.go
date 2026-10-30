package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// A reconcile reading the guest from before a live migration's cutover --
// cutover moved status.podRef to the destination, then deleted the source --
// must not boot a second launcher under the old name: that is two VMs on the
// guest's disk. Seen on a lab cluster after a controller restart mid-transfer.
func TestNoLauncherFromAGuestCachedBeforeACutover(t *testing.T) {
	for name, cachedRef := range map[string]*corev1.ObjectReference{
		// The first migration: the source launcher was the guest's own name.
		"first migration": nil,
		// A chained one: the source was an earlier migration's destination.
		"chained migration": {Name: testGuestName + "-mig-aaaaaa"},
	} {
		t.Run(name, func(t *testing.T) {
			cached := kernelGuest()
			cached.Status.PodRef = cachedRef
			live := kernelGuest()
			live.Status.PodRef = &corev1.ObjectReference{Name: testGuestName + "-mig-bbbbbb"}
			dst := runningLauncher("node-b")
			dst.Name = testGuestName + "-mig-bbbbbb"

			// The cache: the old reference, the source already gone.
			c := guestClientBuilder(cached, readyKernel(), testGuestClass()).Build()
			// The API server: cutover done, the destination running.
			api := guestClientBuilder(live, dst, readyKernel(), testGuestClass()).Build()
			r := &SwiftGuestReconciler{Client: c, Scheme: c.Scheme(), APIReader: api}

			_, res, err := reconcileGuest(t, r)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if pods := launcherPods(t, c); len(pods) != 0 {
				t.Fatalf("created a launcher from a stale guest: %s", pods[0].Name)
			}
			if res.RequeueAfter == 0 && !res.Requeue {
				t.Error("a stale guest must be looked at again")
			}
		})
	}
}

// The check stops nothing else: a launcher that is really gone (its
// reference is current) is recreated, including after an earlier migration
// left a renamed reference (the TFU #18 self-heal).
func TestLauncherStillRecreatedWhenTheReferenceIsCurrent(t *testing.T) {
	for name, ref := range map[string]*corev1.ObjectReference{
		"fresh guest":            nil,
		"lost migrated launcher": {Name: testGuestName + "-mig-cccccc"},
	} {
		t.Run(name, func(t *testing.T) {
			g := kernelGuest()
			g.Status.PodRef = ref
			c := guestClientBuilder(g, readyKernel(), testGuestClass()).Build()
			api := guestClientBuilder(g.DeepCopy(), readyKernel(), testGuestClass()).Build()
			r := &SwiftGuestReconciler{Client: c, Scheme: c.Scheme(), APIReader: api}
			if _, _, err := reconcileGuest(t, r); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			pods := launcherPods(t, c)
			if len(pods) != 1 || pods[0].Name != testGuestName {
				t.Fatalf("launcher not recreated: %v", pods)
			}
		})
	}
}
