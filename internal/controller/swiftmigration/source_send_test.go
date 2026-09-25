package swiftmigration

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
)

// A migration cancelled while its source launcher was still busy with an
// earlier send left its own send on the source pod, and the launcher took it
// up once free: 7.5 minutes after the cancel, against the destination pod's IP,
// which no pod held any more (lab validation of v0.15.0, R6). The cancel takes
// the send off the source.
func TestCancelLive_TakesTheSendOffTheSource(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Spec.CancelRequested = true
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusReceiveReady, recvActionID(mig), "")
	// Written, but the source is still sending an earlier migration's state.
	stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusSending, "earlier:send:1", "")
	src.Annotations[AnnotationMigrationActionArgs] = `{"target_url":"tcp:10.244.1.42:6789"}`
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	ctx := context.Background()

	if handled, _, err := r.honorCancel(ctx, mig); !handled || err != nil {
		t.Fatalf("honorCancel: handled=%v err=%v", handled, err)
	}
	var got corev1.Pod
	if err := r.Get(ctx, client.ObjectKeyFromObject(src), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{AnnotationMigrationAction, AnnotationMigrationActionID, AnnotationMigrationActionArgs} {
		if v, ok := got.Annotations[k]; ok {
			t.Errorf("%s still on the source pod (%q); the launcher would take the send up later", k, v)
		}
	}
	// The earlier send's own status is not ours to touch.
	if got.Annotations[AnnotationMigrationStatusID] != "earlier:send:1" {
		t.Errorf("status-id = %q, want the earlier send's", got.Annotations[AnnotationMigrationStatusID])
	}
}

// Another migration's action on the source pod is left alone.
func TestClearSourceSend_LeavesAnotherMigrationsAction(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	stamp(src, migrationActionVerbSend, "other:send:1", "", "", "")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	ctx := context.Background()

	if err := r.clearSourceSend(ctx, mig); err != nil {
		t.Fatal(err)
	}
	var got corev1.Pod
	_ = r.Get(ctx, client.ObjectKeyFromObject(src), &got)
	if got.Annotations[AnnotationMigrationActionID] != "other:send:1" {
		t.Errorf("another migration's action was removed: %v", got.Annotations)
	}
}

// A live migration failing before cutover takes its send off the source too.
func TestOnTerminalPhase_FailedLiveTakesTheSendOffTheSource(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.SendAttempts = 2
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseFailed
	stamp(src, migrationActionVerbSend, sendActionID(mig), "", "", "")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	ctx := context.Background()

	if err := r.onTerminalPhase(ctx, mig, &mig.Status); err != nil {
		t.Fatal(err)
	}
	var got corev1.Pod
	_ = r.Get(ctx, client.ObjectKeyFromObject(src), &got)
	if _, ok := got.Annotations[AnnotationMigrationAction]; ok {
		t.Errorf("send still on the source pod after a pre-cutover failure: %v", got.Annotations)
	}
}
