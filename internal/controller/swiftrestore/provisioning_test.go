package swiftrestore

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

func pvcEvent(name, pvc, reason, msg string, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: pvc, Namespace: "default"},
		Type:           corev1.EventTypeWarning, Reason: reason, Message: msg,
		LastTimestamp: metav1.NewTime(at),
	}
}

// A restore PVC that cannot be provisioned used to wait as "not yet Bound"
// with nothing saying why (a Longhorn snapshot gone with its source volume
// left a restore waiting forever). The provisioner's latest error is in the
// condition now, and the restore still waits.
func TestNotBound_QuotesTheProvisionerError(t *testing.T) {
	snap := makeReadySnapshot("snap1", "default", "src", "vs1", 40<<30)
	restore := makeRestore("r1", "default", "snap1", "target", true)
	restore.Status.Phase = snapshotv1alpha1.SwiftRestorePhaseRestoring
	now := time.Now()
	objs := []client.Object{
		restore, snap,
		makeSourceGuest("default", "src", "ubuntu-noble"),
		makeBoundSourcePVC("default", "src", "sc"),
		pvcEvent("e1", "swiftguest-root-target", "ProvisioningFailed", "old error", now.Add(-time.Minute)),
		pvcEvent("e2", "swiftguest-root-target", "ProvisioningFailed", "failed to create volume: snapshot not found", now),
		pvcEvent("e3", "other", "ProvisioningFailed", "someone else's", now.Add(time.Minute)),
	}
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&snapshotv1alpha1.SwiftRestore{}, &snapshotv1alpha1.SwiftSnapshot{}).
		WithIndex(&corev1.Event{}, "involvedObject.name", func(o client.Object) []string {
			return []string{o.(*corev1.Event).InvolvedObject.Name}
		}).Build()
	r := &SwiftRestoreReconciler{Client: c, APIReader: c, Scheme: scheme}

	reconcile(t, r, "r1", "default") // creates the restore PVC, Pending in the fake
	got := get(t, c, "r1", "default")
	cond := findReady(got)
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseRestoring || reasonOrEmpty(cond) != ReasonPVCProvisioningFailed {
		t.Fatalf("phase %s reason %s (%s), want Restoring %s", got.Status.Phase, reasonOrEmpty(cond), msgOrEmpty(cond), ReasonPVCProvisioningFailed)
	}
	if msg := msgOrEmpty(cond); !strings.Contains(msg, "snapshot not found") || strings.Contains(msg, "someone else") || strings.Contains(msg, "old error") {
		t.Errorf("message = %q", msg)
	}
}
