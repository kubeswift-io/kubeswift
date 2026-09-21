package sharedbase

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	kscheme "github.com/kubeswift-io/kubeswift/internal/scheme"
)

// The repo registers CRD types centrally rather than with a per-package
// AddToScheme, so tests use that scheme rather than building their own — a
// hand-rolled one drifts from what the controllers actually run with.
func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	return kscheme.Scheme
}

func guest(name, className string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			GuestClassRef: corev1.LocalObjectReference{Name: className},
		},
	}
}

// SwiftGuestClass is CLUSTER-scoped, so the fixture carries no namespace. A
// namespaced lookup against it does not error, it simply never matches — which
// is how a guard can look correct and never fire.
func class(name string, shared bool) *swiftv1alpha1.SwiftGuestClass {
	return &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{SharedBaseDisk: shared},
	}
}

func TestGuestUsesSharedBase(t *testing.T) {
	for _, tc := range []struct {
		name      string
		objs      []client.Object
		wantShare bool
		wantClass string
	}{
		{
			name:      "class opts in",
			objs:      []client.Object{guest("g", "fast"), class("fast", true)},
			wantShare: true, wantClass: "fast",
		},
		{
			name:      "class opts out",
			objs:      []client.Object{guest("g", "normal"), class("normal", false)},
			wantShare: false, wantClass: "normal",
		},
		{
			// Not this package's error to raise: whatever is validating the
			// reference already rejects it, with a message about the missing
			// object. Reporting "shared base undetermined" would replace a
			// clear error with a confusing one.
			name:      "guest absent",
			objs:      nil,
			wantShare: false, wantClass: "",
		},
		{
			name:      "class absent",
			objs:      []client.Object{guest("g", "gone")},
			wantShare: false, wantClass: "gone",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tc.objs...).Build()
			shared, className, err := GuestUsesSharedBase(context.Background(), c, "ns", "g")
			if err != nil {
				t.Fatalf("GuestUsesSharedBase: %v", err)
			}
			if shared != tc.wantShare {
				t.Errorf("shared = %v, want %v", shared, tc.wantShare)
			}
			if className != tc.wantClass {
				t.Errorf("class = %q, want %q", className, tc.wantClass)
			}
		})
	}
}

// A read failure must NOT read as "not shared". Every caller turns false into
// "allow", so swallowing an error here would let exactly the combination this
// refuses through whenever the apiserver hiccups.
func TestGuestUsesSharedBase_ErrorsAreNotSilentlyFalse(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(guest("g", "fast"), class("fast", true)).
		WithInterceptorFuncs(interceptor.Funcs{
			// Only the CLASS read fails. The guest must still resolve, or the
			// lookup returns early and the class is never read at all — which
			// is how an earlier version of this test passed while proving
			// nothing.
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*swiftv1alpha1.SwiftGuestClass); ok {
					return boom
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	_, _, err := GuestUsesSharedBase(context.Background(), c, "ns", "g")
	if err == nil {
		t.Fatal("a failed class read returned no error; callers would treat it as 'not shared' and allow")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error does not wrap the cause: %v", err)
	}
}

// No client means no opinion — used by unit tests of validators that run
// spec-shape rules only.
func TestNilClientIsNotShared(t *testing.T) {
	shared, _, err := GuestUsesSharedBase(context.Background(), nil, "ns", "g")
	if err != nil || shared {
		t.Errorf("got (%v, %v), want (false, nil)", shared, err)
	}
}

// The refusals are what an operator actually sees, so they have to name the
// class the setting lives on and a way forward. A guard that only says "no"
// gets worked around.
func TestRefusalsNameTheClassAndAWayOut(t *testing.T) {
	for _, tc := range []struct {
		name, msg string
		wants     []string
	}{
		{"csi", CSISnapshotRefusal("web-1", "fast"),
			[]string{"web-1", "fast", "sharedBaseDisk", "local", "sharedBaseDisk: false"}},
		{"live", LiveMigrationRefusal("web-1", "fast"),
			[]string{"web-1", "fast", "sharedBaseDisk", "offline", "sharedBaseDisk: false"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range tc.wants {
				if !strings.Contains(tc.msg, w) {
					t.Errorf("refusal does not mention %q: %s", w, tc.msg)
				}
			}
		})
	}
}
