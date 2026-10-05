package swiftmigration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
)

// drainEvents returns every event recorded so far.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func failedEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if strings.HasPrefix(e, "Warning "+ReasonMigrationFailed+" ") {
			out = append(out, e)
		}
	}
	return out
}

// Every failure records one Warning event, with the failure reason when the
// mode has one; a handler that recorded the failure itself is not repeated.
func TestDispatchResult_FailureRecordsOneWarningEvent(t *testing.T) {
	cases := []struct {
		name   string
		result *phaseResult
		want   []string
	}{
		{"live, with a reason", phaseFailure("source launcher runs v1, the controller v2", migrationv1alpha1.FailureReasonImageTagMismatch),
			[]string{"Warning MigrationFailed ImageTagMismatch: source launcher runs v1, the controller v2"}},
		{"offline, no reason", phaseFailure("target node worker-2 is not Ready", ""),
			[]string{"Warning MigrationFailed target node worker-2 is not Ready"}},
		{"already reported by the handler", &phaseResult{FailureMsg: "never reached Ready", FailureReason: migrationv1alpha1.FailureReasonDstNeverReady, Reported: true},
			nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scheme := testScheme(t)
			mig := newMigration("m", "default")
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig).WithStatusSubresource(mig).Build()
			rec := record.NewFakeRecorder(10)
			r := &SwiftMigrationReconciler{Client: cl, Scheme: scheme, Recorder: rec}
			status := mig.Status.DeepCopy()
			if _, err := r.dispatchResult(context.Background(), mig, status, c.result); err != nil {
				t.Fatal(err)
			}
			if got := failedEvents(drainEvents(rec)); strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("events = %q, want %q", got, c.want)
			}
		})
	}
}

// The event follows the persisted transition: a failed status write records
// nothing, and the retry that does persist it records it once.
func TestDispatchResult_NoEventUntilTheFailureIsPersisted(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigration("m", "default")
	conflict := errors.New("the object has been modified")
	fail := true
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig).WithStatusSubresource(mig).
		WithInterceptorFuncs(interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if fail {
				return conflict
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		}}).Build()
	rec := record.NewFakeRecorder(10)
	r := &SwiftMigrationReconciler{Client: cl, Scheme: scheme, Recorder: rec}
	res := phaseFailure("target node worker-2 is not Ready", "")

	if _, err := r.dispatchResult(context.Background(), mig, mig.Status.DeepCopy(), res); err == nil {
		t.Fatal("want the status write's error")
	}
	if got := failedEvents(drainEvents(rec)); len(got) != 0 {
		t.Errorf("a failure that was not persisted was reported: %q", got)
	}
	fail = false
	if _, err := r.dispatchResult(context.Background(), mig, mig.Status.DeepCopy(), res); err != nil {
		t.Fatal(err)
	}
	if got := failedEvents(drainEvents(rec)); len(got) != 1 {
		t.Errorf("events = %q, want exactly one", got)
	}
}

// The lab case (#694): a live migration refused at validation because the
// source launcher came from another build recorded no event at all.
func TestReconcile_RefusedAtValidationRecordsAWarningEvent(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("worker-2", 8, 65536)
	t.Setenv(swiftguest.LauncherImageEnv, "ghcr.io/test/swiftletd:v2.0.0")
	srcPod := newSourcePodWithLauncherImage("guest", "default", "uid", "ghcr.io/test/swiftletd:v1.0.0")
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node, srcPod).WithStatusSubresource(mig).Build()
	rec := record.NewFakeRecorder(20)
	r := &SwiftMigrationReconciler{Client: cl, APIReader: cl, Scheme: scheme, Recorder: rec}

	ctx := context.Background()
	var got migrationv1alpha1.SwiftMigration
	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
			t.Fatal(err)
		}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(mig), &got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseFailed {
			break
		}
	}
	if got.Status.FailureReason != migrationv1alpha1.FailureReasonImageTagMismatch {
		t.Fatalf("phase=%s reason=%s, want Failed/ImageTagMismatch", got.Status.Phase, got.Status.FailureReason)
	}
	events := failedEvents(drainEvents(rec))
	if len(events) != 1 || !strings.Contains(events[0], "ImageTagMismatch: ") || !strings.Contains(events[0], "v1.0.0") {
		t.Errorf("events = %q, want one MigrationFailed warning naming the mismatch", events)
	}
	// A terminal migration reconciles again without reporting again.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
		t.Fatal(err)
	}
	if again := failedEvents(drainEvents(rec)); len(again) != 0 {
		t.Errorf("reported again on a terminal reconcile: %q", again)
	}
}
