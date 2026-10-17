package swiftsandbox

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// A sandbox created without spec.network gets its finalizers by a patch of
// metadata only. A full-object update wrote spec.network back as {}, the API
// server defaulted it, and the webhook refused the "spec change": the
// sandbox never started (#768).
func TestFinalizers_PatchMetadataOnly(t *testing.T) {
	sb := plainSandbox(testImage(t))
	sb.Spec.Network = sandboxv1alpha1.SandboxNetwork{}
	var patches []string
	updates := 0
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sb, readyKernel("default", defaultKernelProfile)).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*sandboxv1alpha1.SwiftSandbox); ok {
					updates++
				}
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*sandboxv1alpha1.SwiftSandbox); ok {
					data, _ := p.Data(obj)
					patches = append(patches, string(data))
				}
				return cl.Patch(ctx, obj, p, opts...)
			},
		}).Build()
	r := &SwiftSandboxReconciler{Client: c, APIReader: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(20)}
	reconcileSB(t, r, "sb")
	if updates != 0 {
		t.Errorf("%d full-object updates of the sandbox", updates)
	}
	if len(patches) == 0 {
		t.Fatal("no finalizer patch")
	}
	for _, p := range patches {
		if strings.Contains(p, `"spec"`) || !strings.Contains(p, `"finalizers"`) || !strings.Contains(p, `"resourceVersion"`) {
			t.Errorf("patch %s: want finalizers and resourceVersion only", p)
		}
	}
	if got := getSandbox(t, c, "sb"); !strings.Contains(strings.Join(got.Finalizers, ","), sandboxTerminationFinalizer) {
		t.Errorf("finalizers %v", got.Finalizers)
	}
}
