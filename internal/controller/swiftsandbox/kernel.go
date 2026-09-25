package swiftsandbox

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// kernelRecheckInterval paces the wait for a missing or not-Ready SwiftKernel.
// Neither reconciler watches SwiftKernels, so this requeue is what notices one
// appear or finish pulling.
const kernelRecheckInterval = 10 * time.Second

// checkKernel reports why the launcher for sb cannot boot SwiftKernel
// kernelName yet, as a Resolved=False reason and message, or reason "" when it
// can. Only a failed read is an error. sb is the sandbox, or a pool's slot
// template.
//
// The launcher mounts the kernel directory with type DirectoryOrCreate, so
// without this a missing kernel became an empty directory, the pod started,
// and the boot failed in the hypervisor ("Cannot open initramfs file") with
// nothing naming the kernel.
//
// Readiness is SwiftKernel.PhaseOn: the phase on the node the launcher is
// pinned to (a native GPU sandbox's allocated node, or a hostname in
// spec.nodeSelector) when the kernel reports one, otherwise the overall phase,
// as for a kernel-boot SwiftGuest.
func checkKernel(ctx context.Context, c client.Reader, sb *sandboxv1alpha1.SwiftSandbox, kernelName string) (reason, msg string, err error) {
	var sk kernelv1alpha1.SwiftKernel
	if err := c.Get(ctx, client.ObjectKey{Namespace: sb.Namespace, Name: kernelName}, &sk); err != nil {
		if apierrors.IsNotFound(err) {
			return sandboxv1alpha1.SwiftSandboxReasonKernelNotFound,
				fmt.Sprintf("no SwiftKernel named %q in namespace %q", kernelName, sb.Namespace), nil
		}
		return "", "", err
	}
	node := pinnedNode(sb)
	phase := sk.PhaseOn(node)
	if phase == kernelv1alpha1.SwiftKernelPhaseReady {
		return "", "", nil
	}
	state := string(phase)
	if state == "" {
		state = "none reported yet"
	}
	msg = fmt.Sprintf("SwiftKernel %q is not Ready (phase: %s)", kernelName, state)
	if node != "" {
		msg = fmt.Sprintf("SwiftKernel %q is not Ready for node %s (phase: %s)", kernelName, node, state)
	}
	return sandboxv1alpha1.SwiftSandboxReasonKernelNotReady, msg, nil
}
