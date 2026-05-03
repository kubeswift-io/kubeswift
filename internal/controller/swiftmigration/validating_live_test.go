package swiftmigration

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

// newSourcePod creates a fake source pod for the given guest, with a
// fixed UID so tests can assert SourcePodUID stamping.
func newSourcePod(guestName, ns, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      guestName,
			Namespace: ns,
			UID:       types.UID(uid),
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guestName,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "boba",
		},
	}
}

func TestValidatingLive_HappyPath_AdvancesToPreparing(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	srcPod := newSourcePod("guest", "default", "src-pod-uid-1")
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true
	mig.Spec.Timeout = &metav1.Duration{Duration: 5 * 60 * 1e9} // 5min, satisfies MinLiveTimeout
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node, srcPod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleValidatingLive(context.Background(), mig, status)
	if result.Err != nil || result.FailureMsg != "" {
		t.Fatalf("handleValidatingLive failure: err=%v msg=%q", result.Err, result.FailureMsg)
	}
	if !result.Advanced {
		t.Fatal("expected Advanced=true on success")
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhasePreparing {
		t.Errorf("phase: want Preparing, got %q", status.Phase)
	}
	if status.Mode != migrationv1alpha1.SwiftMigrationModeLive {
		t.Errorf("status.Mode: want live, got %q", status.Mode)
	}
	if status.SourcePodUID != "src-pod-uid-1" {
		t.Errorf("SourcePodUID: want src-pod-uid-1, got %q", status.SourcePodUID)
	}
	if status.SourceNode != "boba" {
		t.Errorf("SourceNode: want boba, got %q", status.SourceNode)
	}
	if status.DestinationNode != "miles" {
		t.Errorf("DestinationNode: want miles, got %q", status.DestinationNode)
	}
}

func TestValidatingLive_NoSourcePod_FailsWithClearMessage(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	// No source pod added — simulates a guest that's not currently
	// running.
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleValidatingLive(context.Background(), mig, status)
	if !strings.Contains(result.FailureMsg, "has no pod") {
		t.Errorf("FailureMsg: want 'has no pod' message, got %q", result.FailureMsg)
	}
	if result.FailureReason != migrationv1alpha1.FailureReasonOther {
		t.Errorf("FailureReason: want Other, got %q", result.FailureReason)
	}
}

func TestValidatingLive_DefensiveGuard_NotLiveMode(t *testing.T) {
	r := &SwiftMigrationReconciler{}
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	result := r.handleValidatingLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(result.FailureMsg, "without live mode") {
		t.Errorf("guard message: got %q", result.FailureMsg)
	}
}

func TestValidatingLive_GuestNotFound_Fails(t *testing.T) {
	scheme := validatingScheme(t)
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	result := r.handleValidatingLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(result.FailureMsg, "no longer exists") {
		t.Errorf("FailureMsg: want guest-missing message, got %q", result.FailureMsg)
	}
}

func TestValidatingLive_TargetNodeCordoned_Fails(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	cordonedNode := newSpaciousNode("miles", 8, 65536)
	cordonedNode.Spec.Unschedulable = true
	srcPod := newSourcePod("guest", "default", "uid")
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, cordonedNode, srcPod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	result := r.handleValidatingLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(result.FailureMsg, "cordoned") {
		t.Errorf("FailureMsg: want cordoned message, got %q", result.FailureMsg)
	}
}
