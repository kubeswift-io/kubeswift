package swiftsandbox

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

// Secret env (spec.env[].valueFrom.secretKeyRef). The controller never
// carries a value: it checks the Secrets exist, grants the launcher's own
// account `get` on exactly them, and hands swiftletd the references.
// swiftletd reads the values and gives them to the guest, on a cold boot via
// the config disk (kept in memory, secretRunDir) and on a checkout over vsock.

// secretRunDir is a memory-backed volume a cold launcher with secret env
// writes its config disk to, so the values never reach the node's disk.
const (
	secretRunVolume = "secret-run"
	secretRunDir    = "/var/lib/kubeswift/secret-run"
	// annSecretError is swiftletd's report that it could not read a secret
	// variable (the message names the Secret and key, never a value).
	annSecretError = "kubeswift.io/sandbox-secret-error"
	// annKernelError is swiftletd's report that the kernel's bridge lacks a
	// feature the sandbox needs (bridge-features next to the kernel).
	annKernelError = "kubeswift.io/sandbox-kernel-error"
)

// secretFileRefs lists spec.secretFiles item by item, with each file's mode
// resolved (item, else entry, else 0400).
func secretFileRefs(sb *sandboxv1alpha1.SwiftSandbox) []runtimeintent.SecretFileRef {
	var out []runtimeintent.SecretFileRef
	for _, f := range sb.Spec.SecretFiles {
		for _, it := range f.Items {
			mode := int32(0o400)
			if f.Mode != nil {
				mode = *f.Mode
			}
			if it.Mode != nil {
				mode = *it.Mode
			}
			out = append(out, runtimeintent.SecretFileRef{
				Secret: f.SecretName, Key: it.Key, Path: it.Path, Mode: mode, Optional: f.Optional,
			})
		}
	}
	return out
}

// usesSecrets reports whether sb reads any Secret (env or files).
func usesSecrets(sb *sandboxv1alpha1.SwiftSandbox) bool {
	return len(secretEnvRefs(sb)) > 0 || len(sb.Spec.SecretFiles) > 0
}

// secretEnvRefs lists the variables whose values come from Secrets.
func secretEnvRefs(sb *sandboxv1alpha1.SwiftSandbox) []runtimeintent.SecretEnvRef {
	var out []runtimeintent.SecretEnvRef
	for _, e := range sb.Spec.Env {
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			continue
		}
		r := e.ValueFrom.SecretKeyRef
		out = append(out, runtimeintent.SecretEnvRef{
			Name: e.Name, Secret: r.Name, Key: r.Key, Optional: r.Optional != nil && *r.Optional,
		})
	}
	return out
}

// secretNames are the Secrets sb's launcher may read (env and files).
func secretNames(sb *sandboxv1alpha1.SwiftSandbox) []string {
	var out []string
	for _, r := range secretEnvRefs(sb) {
		out = append(out, r.Secret)
	}
	for _, f := range sb.Spec.SecretFiles {
		out = append(out, f.SecretName)
	}
	return out
}

// secretNamesFor is secretNames for sb's launcher pod podName running as
// serviceAccount: none for the shared account, which must never be granted a
// Secret (a launcher created before per-pod accounts, which never had secret
// env either).
func secretNamesFor(serviceAccount string, sb *sandboxv1alpha1.SwiftSandbox, podName string) []string {
	if serviceAccount != swiftguest.SandboxLauncherServiceAccountFor(podName) {
		return nil
	}
	return secretNames(sb)
}

// checkSecretEnv reports a referenced Secret or key that does not exist, as a
// reason and message for a sandbox that waits for it. Optional references
// are skipped: a missing one leaves the variable unset, as on a pod. The
// value read here stays in this function.
func checkSecretEnv(ctx context.Context, c client.Reader, sb *sandboxv1alpha1.SwiftSandbox) (string, string, error) {
	for i, r := range secretEnvRefs(sb) {
		if r.Optional {
			continue
		}
		var s corev1.Secret
		err := c.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: r.Secret}, &s)
		if apierrors.IsNotFound(err) {
			return "SecretNotFound", fmt.Sprintf("secret env %s (#%d): Secret %s not found", r.Name, i, r.Secret), nil
		}
		if err != nil {
			return "", "", err
		}
		if _, ok := s.Data[r.Key]; !ok {
			if _, ok := s.StringData[r.Key]; !ok {
				return "SecretKeyNotFound", fmt.Sprintf("secret env %s (#%d): Secret %s has no key %s", r.Name, i, r.Secret, r.Key), nil
			}
		}
	}
	for _, r := range secretFileRefs(sb) {
		if r.Optional {
			continue
		}
		var s corev1.Secret
		err := c.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: r.Secret}, &s)
		if apierrors.IsNotFound(err) {
			return "SecretNotFound", fmt.Sprintf("secret file %s: Secret %s not found", r.Path, r.Secret), nil
		}
		if err != nil {
			return "", "", err
		}
		if _, ok := s.Data[r.Key]; !ok {
			if _, ok := s.StringData[r.Key]; !ok {
				return "SecretKeyNotFound", fmt.Sprintf("secret file %s: Secret %s has no key %s", r.Path, r.Secret, r.Key), nil
			}
		}
	}
	return "", "", nil
}

// kernelError is swiftletd's report that the kernel's bridge lacks a feature.
func kernelError(pod *corev1.Pod) (string, bool) {
	msg, ok := pod.Annotations[annKernelError]
	return msg, ok
}

// secretError is swiftletd's report that a secret variable could not be read.
func secretError(pod *corev1.Pod) (string, bool) {
	msg, ok := pod.Annotations[annSecretError]
	return msg, ok
}
