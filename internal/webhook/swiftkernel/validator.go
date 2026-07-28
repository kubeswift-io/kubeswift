// Package swiftkernel validates SwiftKernel resources.
//
// SwiftKernel had no admission at all — the only one of the CRDs without a
// validator. It is not a low-risk kind: spec.ociRef.image is pulled by a
// per-node Job that runs as root with a hostPath mount on
// /var/lib/kubeswift/kernels, and spec.kernelCmdline is handed to the guest
// kernel at boot. Both are worth constraining at the door.
package swiftkernel

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
)

// Validator validates SwiftKernel resources.
type Validator struct{}

// shellMeta are characters with no legitimate place in an OCI reference. The
// kernel-pull Job passes the image through an env var now (it used to be
// interpolated into a shell script — see #435), so this is defence in depth
// rather than the only gate. Cheap, and it keeps the fix from silently
// regressing if that Job is ever rewritten.
const shellMeta = "$`;|&<>(){}[]!*?\\\"'\n\r\t"

func validateKernel(sk *kernelv1alpha1.SwiftKernel) error {
	img := sk.Spec.OCIRef.Image
	if strings.TrimSpace(img) == "" {
		return fmt.Errorf("spec.ociRef.image is required")
	}
	if img != strings.TrimSpace(img) {
		return fmt.Errorf("spec.ociRef.image must not have leading or trailing whitespace (got %q)", img)
	}
	if i := strings.IndexAny(img, shellMeta); i >= 0 {
		return fmt.Errorf("spec.ociRef.image contains an illegal character %q at offset %d (got %q)",
			img[i:i+1], i, img)
	}
	if strings.Contains(img, "..") {
		// The artifact lands under /var/lib/kubeswift/kernels/<ns>-<name>/ on
		// the node; a reference is never a path, and "…" in one is a smell.
		return fmt.Errorf("spec.ociRef.image must not contain '..' (got %q)", img)
	}

	// kernelCmdline is not shell-interpolated (the hypervisor spawn path uses
	// exec-form args), but it IS written into files and passed as a single
	// argument, so an embedded newline or NUL would split or truncate it.
	if i := strings.IndexAny(sk.Spec.KernelCmdline, "\n\r\x00"); i >= 0 {
		return fmt.Errorf("spec.kernelCmdline must not contain a newline or NUL (offset %d)", i)
	}
	return nil
}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sk, ok := obj.(*kernelv1alpha1.SwiftKernel)
	if !ok {
		return nil, fmt.Errorf("expected SwiftKernel, got %T", obj)
	}
	return nil, validateKernel(sk)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	sk, ok := newObj.(*kernelv1alpha1.SwiftKernel)
	if !ok {
		return nil, fmt.Errorf("expected SwiftKernel, got %T", newObj)
	}
	if err := validateKernel(sk); err != nil {
		return nil, err
	}

	old, ok := oldObj.(*kernelv1alpha1.SwiftKernel)
	if !ok {
		return nil, nil
	}
	// Editing the image on an existing SwiftKernel does NOTHING, silently: the
	// per-node pull Job is named pullJobName(name, node) with no image or digest
	// in the key, and Create swallows AlreadyExists. So the new tag is never
	// pulled and the node keeps serving the old artifact — with the CR happily
	// reporting the new image. Warn rather than reject: re-pointing a kernel is
	// legitimate, it just needs the Jobs deleted to take effect.
	if old.Spec.OCIRef.Image != sk.Spec.OCIRef.Image {
		return admission.Warnings{
			fmt.Sprintf("spec.ociRef.image changed (%s -> %s) but the per-node pull Job is keyed on "+
				"(name,node) only, so nothing will be re-pulled and nodes keep serving the OLD artifact. "+
				"Delete the pull Jobs to force it: kubectl -n %s delete job -l app=swiftkernel-pull "+
				"(or: kubectl -n %s delete job swiftkernel-pull-%s-<node>)",
				old.Spec.OCIRef.Image, sk.Spec.OCIRef.Image, sk.Namespace, sk.Namespace, sk.Name),
		}, nil
	}
	return nil, nil
}

func (v *Validator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}
