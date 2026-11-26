package swiftsandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func TestColdEnvViolation(t *testing.T) {
	secret := &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k"}}
	for _, tc := range []struct {
		name     string
		imageEnv []string
		spec     []corev1.EnvVar
		want     string
	}{
		{"all shell names", []string{"PATH=/bin", "_X1=a=b"}, []corev1.EnvVar{{Name: "MODE", Value: "x"}, {Name: "TOKEN", ValueFrom: secret}}, ""},
		{"spec name with a dot", []string{"PATH=/bin", "app.mode=x"}, []corev1.EnvVar{{Name: "app.mode", Value: "x"}}, `"app.mode" (spec.env)`},
		{"image name with a dash", []string{"my-var=1"}, nil, `"my-var" (the image config)`},
		{"secret-backed name", nil, []corev1.EnvVar{{Name: "db.pass", ValueFrom: secret}}, `"db.pass" (spec.env)`},
		{"leading digit", []string{"1X=1"}, nil, `"1X"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := &sandboxv1alpha1.SwiftSandbox{Spec: sandboxv1alpha1.SwiftSandboxSpec{Env: tc.spec}}
			msg, bad := coldEnvViolation(sb, tc.imageEnv)
			if bad != (tc.want != "") || !strings.Contains(msg, tc.want) {
				t.Errorf("bad=%v msg=%q, want %q", bad, msg, tc.want)
			}
		})
	}
}

// An image whose config sets a variable the guest shell cannot set fails the
// sandbox before any launcher exists, instead of a guest that panics at boot.
func TestCreateLaunch_InvalidEnvName(t *testing.T) {
	reg := newABIRegistry(t)
	base, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		imageEnv   []string
		wantFailed bool
	}{
		{"image env with a dot", []string{"PATH=/bin", "app.mode=1"}, true},
		{"shell names", []string{"PATH=/bin", "APP_MODE=1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, err := mutate.Config(base, v1.Config{Env: tc.imageEnv, Cmd: []string{"/bin/true"}})
			if err != nil {
				t.Fatal(err)
			}
			sb := plainSandbox(reg.write("test/env:"+strings.ReplaceAll(tc.name, " ", "-"), img))
			r, c := sandboxReconciler(sb, readyKernel("default", "sandbox"))
			r.Recorder = record.NewFakeRecorder(20)
			var got *sandboxv1alpha1.SwiftSandbox
			launched := false
			for i := 0; i < 50; i++ {
				reconcileSB(t, r, "sb")
				got = getSandbox(t, c, "sb")
				var pod corev1.Pod
				launched = c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod) == nil
				if launched || got.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			failed := got.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed
			if failed != tc.wantFailed || failed == launched {
				t.Fatalf("failed=%v launched=%v message=%q", failed, launched, got.Status.Message)
			}
			if failed {
				cond := apimeta.FindStatusCondition(got.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionResolved)
				if cond == nil || cond.Reason != reasonInvalidEnvName || !strings.Contains(got.Status.Message, `"app.mode" (the image config)`) {
					t.Errorf("condition %+v message %q", cond, got.Status.Message)
				}
			}
		})
	}
}
