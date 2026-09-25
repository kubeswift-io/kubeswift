package swiftmigration

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// migrationStatusSending is swiftletd-on-src's pre-dispatch status for a send
// it has taken up (StatusKind::Custom("sending")). With this migration's
// $SEND_ID it means the transfer is under way; with another id, the source
// launcher is still running an earlier send.
const migrationStatusSending = "sending"

// AnnotationMigrationProgressEstimateID names the send the
// migration-progress-estimate annotation belongs to. swiftletd writes it with
// every estimate; an estimate carrying another send's id is stale.
const AnnotationMigrationProgressEstimateID = "kubeswift.io/migration-progress-estimate-id"

// clearSourceSend removes this migration's send action from the source launcher
// pod, for a migration that ended before cutover (cancelled, or failed).
//
// swiftletd does not reject an action that arrives while another runs: it
// leaves it waiting and takes it up once the running one finishes. A send the
// migration left behind therefore ran later, whatever had become of the
// migration. In the lab's validation of v0.15.0 a cancelled migration's send
// ran 7.5 minutes after the cancel, against the destination pod's IP, which no
// pod held any more. A pod that had taken that IP and listened on the
// migration port would have received this guest's memory.
//
// Idempotent, and a no-op when the source pod is gone or carries another
// migration's action. If swiftletd has already taken the send up, removing
// the annotations does not stop it; the destination's cancel does.
func (r *SwiftMigrationReconciler) clearSourceSend(ctx context.Context, mig *migrationv1alpha1.SwiftMigration) error {
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		return client.IgnoreNotFound(err)
	}
	var src corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: srcPodLookupName(mig, &guest), Namespace: guest.Namespace}, &src); err != nil {
		return client.IgnoreNotFound(err)
	}
	// Any of this migration's send attempts (<name>:send:<n>).
	if !strings.HasPrefix(src.Annotations[AnnotationMigrationActionID], mig.Name+":send:") {
		return nil
	}
	patch := client.MergeFrom(src.DeepCopy())
	delete(src.Annotations, AnnotationMigrationAction)
	delete(src.Annotations, AnnotationMigrationActionID)
	delete(src.Annotations, AnnotationMigrationActionArgs)
	return client.IgnoreNotFound(r.Patch(ctx, &src, patch))
}
