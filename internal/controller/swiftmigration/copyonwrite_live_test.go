package swiftmigration

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
)

// A clone restored copyonwrite runs from a private mapping of its snapshot's
// memory file; live migration refuses its launcher with the way out (#869).
func TestValidatingLive_CopyOnWriteCloneRefused(t *testing.T) {
	objs := explicitObjs("guest", oldLauncher)
	pod := objs[3].(*corev1.Pod)
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[swiftguest.AnnotationMemoryCopyOnWrite] = "true"

	dst, res := migrateToPreparing(t, objs, liveMigration(), validateLive)
	if dst != nil {
		t.Fatal("a destination pod was created")
	}
	if res == nil || res.FailureReason != migrationv1alpha1.FailureReasonEligibilityMismatch {
		t.Fatalf("got %+v, want Failed/EligibilityMismatch", res)
	}
	if !strings.Contains(res.FailureMsg, "copyonwrite") || !strings.Contains(res.FailureMsg, "mode: offline") {
		t.Errorf("message: %q", res.FailureMsg)
	}
}
