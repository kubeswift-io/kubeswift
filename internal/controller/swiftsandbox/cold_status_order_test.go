package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// #848: a cold sandbox's status is written before its launcher pod is
// created. A lost status write creates no pod, so the next pass resolves and
// writes it again; it used to leave Resolved and status.artifacts stale for
// the sandbox's life, because a pass that finds the pod never computes them.
func TestCreateLaunch_StatusWrittenBeforeThePod(t *testing.T) {
	conflicts := 1
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(plainSandbox(testImage(t)), readyKernel("default", "sandbox")).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if sb, ok := obj.(*sandboxv1alpha1.SwiftSandbox); ok && conflicts > 0 &&
					sb.Status.Phase == sandboxv1alpha1.SwiftSandboxMaterializing {
					conflicts--
					return apierrors.NewConflict(schema.GroupResource{Resource: "swiftsandboxes"}, sb.Name, nil)
				}
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	r := &SwiftSandboxReconciler{Client: c, APIReader: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(20)}

	// Reconcile turns the conflict into a requeue (#807).
	reconcileSB(t, r, "sb")
	if conflicts != 0 {
		t.Fatal("the Materializing status write never happened")
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); !apierrors.IsNotFound(err) {
		t.Fatalf("launcher pod created although its status write was lost (err=%v)", err)
	}

	reconcileSB(t, r, "sb")
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatalf("no launcher pod after the retry: %v", err)
	}
	sb := getSandbox(t, c, "sb")
	if cond := apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Resolved = %+v, want True", cond)
	}
	if sb.Status.Rootfs == nil || sb.Status.Rootfs.Digest == "" {
		t.Errorf("status.rootfs = %+v, want the resolved digest", sb.Status.Rootfs)
	}
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxMaterializing {
		t.Errorf("phase = %q, want Materializing", sb.Status.Phase)
	}
}
