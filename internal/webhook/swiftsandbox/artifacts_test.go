package swiftsandbox

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func arts(a ...sandboxv1alpha1.SandboxArtifact) *sandboxv1alpha1.SwiftSandboxSpec {
	return &sandboxv1alpha1.SwiftSandboxSpec{Image: "x", Artifacts: a}
}

func art(name, mountPath string) sandboxv1alpha1.SandboxArtifact {
	return sandboxv1alpha1.SandboxArtifact{Name: name, Ref: "registry.example.com/team/app@sha256:" + strings.Repeat("a", 64), MountPath: mountPath}
}

func TestValidateArtifacts(t *testing.T) {
	if err := ValidateArtifacts(arts(art("app", "/run/artifacts/app"), art("policy", "/etc/policy"))); err != nil {
		t.Errorf("unexpected error %v", err)
	}
	withModel := arts(art("app", "/model/app"))
	withModel.Model = &sandboxv1alpha1.SandboxModel{ImageRef: "m"}
	badLayout := art("app", "/a")
	badLayout.Layout = "tar"
	badRef := art("app", "/a")
	badRef.Ref = "not a ref!"
	for _, tc := range []struct {
		name, want string
		s          *sandboxv1alpha1.SwiftSandboxSpec
	}{
		{"name twice", "used twice", arts(art("app", "/a"), art("app", "/b"))},
		{"bad name", "DNS label", arts(art("App_1", "/a"))},
		{"bad ref", "ref", arts(badRef)},
		{"bad layout", "oci or unpacked", arts(badLayout)},
		{"relative path", "clean absolute", arts(art("app", "run/x"))},
		{"comma in path", "clean absolute", arts(art("app", "/run/a,b"))},
		{"under /dev", "guest's own", arts(art("app", "/dev/x"))},
		{"nested mounts", "overlaps", arts(art("a", "/run/artifacts"), art("b", "/run/artifacts/b"))},
		{"inside the model mount", "overlaps", withModel},
	} {
		err := ValidateArtifacts(tc.s)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
	// A secret file inside an artifact mount (read-only) is refused.
	s := arts(art("app", "/run/artifacts/app"))
	s.SecretFiles = []sandboxv1alpha1.SandboxSecretFile{{SecretName: "x", Items: []sandboxv1alpha1.SandboxSecretFileItem{{Key: "k", Path: "/run/artifacts/app/token"}}}}
	if err := ValidateSecretFiles(s); err == nil {
		t.Error("a secret file inside a read-only artifact mount must be refused")
	}
}

// fetchTimeout belongs to optional artifacts only, between 0s and 5m.
func TestValidateArtifacts_FetchTimeout(t *testing.T) {
	ft := func(d time.Duration, optional bool) sandboxv1alpha1.SandboxArtifact {
		a := art("app", "/a")
		a.Optional, a.FetchTimeout = optional, &metav1.Duration{Duration: d}
		return a
	}
	for _, ok := range []sandboxv1alpha1.SandboxArtifact{ft(0, true), ft(10*time.Second, true), ft(5*time.Minute, true)} {
		if err := ValidateArtifacts(arts(ok)); err != nil {
			t.Errorf("%v refused: %v", ok.FetchTimeout.Duration, err)
		}
	}
	for _, tc := range []struct {
		a    sandboxv1alpha1.SandboxArtifact
		want string
	}{
		{ft(time.Second, false), "only for an optional"},
		{ft(-time.Second, true), "between"},
		{ft(6*time.Minute, true), "between"},
	} {
		if err := ValidateArtifacts(arts(tc.a)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v", tc.a.FetchTimeout, err)
		}
	}
	// optional alone is fine; a required artifact keeps no fetchTimeout.
	o := art("app", "/a")
	o.Optional = true
	if err := ValidateArtifacts(arts(o)); err != nil {
		t.Error(err)
	}
}

// Claims are plain strings in a restricted alphabet: no separators a claim
// could be split on, at most eight per artifact.
func TestValidateArtifacts_Claims(t *testing.T) {
	with := func(c map[string]string) *sandboxv1alpha1.SwiftSandboxSpec {
		a := art("app", "/a")
		a.Claims = c
		return arts(a)
	}
	if err := ValidateArtifacts(with(map[string]string{"example.com/source": "sha256:" + strings.Repeat("b", 64), "scope": "team-a"})); err != nil {
		t.Errorf("unexpected error %v", err)
	}
	many := map[string]string{}
	for i := 0; i < 9; i++ {
		many[string(rune('a'+i))] = "v"
	}
	for _, tc := range []struct {
		name, want string
		c          map[string]string
	}{
		{"too many", "at most 8", many},
		{"comma in value", "letters, digits", map[string]string{"k": "a,b"}},
		{"equals in value", "letters, digits", map[string]string{"k": "a=b"}},
		{"empty value", "letters, digits", map[string]string{"k": ""}},
		{"space in key", "letters, digits", map[string]string{"a b": "v"}},
		{"key starts with a dot", "letters, digits", map[string]string{".k": "v"}},
		{"long value", "letters, digits", map[string]string{"k": strings.Repeat("v", 257)}},
	} {
		err := ValidateArtifacts(with(tc.c))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}
