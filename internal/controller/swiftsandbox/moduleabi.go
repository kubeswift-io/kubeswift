package swiftsandbox

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// Reasons for the module ABI check (WS1.5).
const (
	// reasonKernelModuleABIMismatch fails a GPU sandbox (or degrades a GPU
	// pool) whose image declares, in its ModuleABILabel, a gpu-sandbox
	// module ABI other than the one its SwiftKernel's artifact carries. Its
	// NVIDIA modules would be refused at insmod, or, on a kernel without the
	// guard, load and crash the guest.
	reasonKernelModuleABIMismatch = "KernelModuleABIMismatch"
	// reasonModuleABIUndeclared is the Warning Event for a GPU image that
	// declares no module ABI on a kernel that has one: the launch goes ahead,
	// and the kernel refuses a mismatched module at insmod.
	reasonModuleABIUndeclared = "ModuleABIUndeclared"
	// reasonKernelResolveFailed fails a GPU sandbox whose kernel artifact the
	// registry refuses to describe (missing, or not readable with
	// spec.ociRef.pullSecret).
	reasonKernelResolveFailed = "KernelResolveFailed"
)

// moduleABIRequest is the registry request for the module-abi file of
// SwiftKernel kernelName's artifact, read with the kernel's own pull Secret.
// The caller has already checked that the SwiftKernel exists (checkKernel).
// ok is false when the kernel names no artifact, so there is nothing to check.
func moduleABIRequest(ctx context.Context, c, secrets client.Reader, namespace, kernelName string) (req resolveRequest, ok bool, err error) {
	var sk kernelv1alpha1.SwiftKernel
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: kernelName}, &sk); err != nil {
		return resolveRequest{}, false, err
	}
	ref := sk.Spec.OCIRef.Image
	if ref == "" {
		return resolveRequest{}, false, nil
	}
	auth, err := pullSecretAuth(ctx, secrets, namespace, sk.Spec.OCIRef.PullSecret, ref)
	if err != nil {
		req := newResolveRequest(resolveModuleABIKind, ref, "", "", nil)
		req.credErr = fmt.Errorf("SwiftKernel %q: %w", kernelName, err)
		return req, true, nil
	}
	return newResolveRequest(resolveModuleABIKind, ref, "", "", auth), true, nil
}

// resolveModuleABI looks up req. ready is false while the request is running.
// A zero req (no artifact to check) is ready and finds nothing.
func resolveModuleABI(req resolveRequest, lookup lookupFunc) (abi string, found, ready bool, err error) {
	if req.key.ref == "" {
		return "", false, true, nil
	}
	if req.credErr != nil {
		return "", false, true, req.credErr
	}
	res, ok := lookup(req)
	if !ok || res.err != nil {
		return "", false, ok, res.err
	}
	return res.moduleABI, res.moduleABIFound, true, nil
}

// moduleABIVerdict compares a kernel's module ABI with what an image declares.
// It returns reasonKernelModuleABIMismatch with a message when they differ,
// reasonModuleABIUndeclared when the kernel has one and the image declares
// none, and "" otherwise (including a kernel artifact published before the
// guard, which carries no module-abi file).
func moduleABIVerdict(kernelName, kernelABI string, kernelFound bool, image string, labels map[string]string) (reason, msg string) {
	if !kernelFound || kernelABI == "" {
		return "", ""
	}
	declared, ok := labels[materialize.ModuleABILabel]
	if !ok || declared == "" {
		return reasonModuleABIUndeclared, fmt.Sprintf(
			"image %s declares no %s label; SwiftKernel %q loads modules built for %s only, so rebuild the image's kernel modules for it and set the label",
			image, materialize.ModuleABILabel, kernelName, kernelABI)
	}
	if declared != kernelABI {
		return reasonKernelModuleABIMismatch, fmt.Sprintf(
			"image %s carries kernel modules built for %s (label %s), but SwiftKernel %q is %s; rebuild the modules against that kernel, or point the sandbox at a SwiftKernel for %s",
			image, declared, materialize.ModuleABILabel, kernelName, kernelABI, declared)
	}
	return "", ""
}
