package swiftsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// Warm-slot artifacts: spec.artifacts on a pool checkout.
//
// Pools stay generic: a slot carries no artifact. Every warm slot boots with
// an empty read-only virtio-fs staging share. On a checkout the sandbox's
// artifacts are resolved and authorized with its own credentials (resolver.go)
// and listed, by digest, in the exec action; swiftletd binds each node-cache
// entry read-only into the staging share, and the guest agent binds it at its
// path. The VM is never rebooted.
//
// A cache entry missing on the slot's node (or not yet verified with the
// sandbox's key) makes swiftletd report ArtifactMissing; the controller then
// runs one fetch pod on that node with the sandbox's pull Secrets and keys (the
// same materializer and arguments as a cold sandbox's init containers) and
// dispatches again, once.

const (
	// warmStageTag and warmStageDir must match swiftletd's STAGE_TAG and
	// STAGE_DIR (rust/swiftletd/src/warm_artifacts.rs).
	warmStageTag    = "sbxstage"
	warmStageDir    = "/var/lib/kubeswift/sandbox-stage"
	warmStageVolume = "sandbox-stage"

	// bridgeFeaturesAnnotation is written by swiftletd on a sandbox launcher:
	// what its bridge can be asked for. warmMountsFeature is listed only when
	// the kernel's bridge supports checkout mounts and the slot has a staging
	// share.
	bridgeFeaturesAnnotation = "kubeswift.io/bridge-features"
	warmMountsFeature        = "warm-mounts"

	// artifactMissingPrefix starts the exec status detail swiftletd writes
	// when a checkout's artifacts are not on the node.
	artifactMissingPrefix = "ArtifactMissing: "
	// fetchedActionSuffix marks the second dispatch, after a fetch.
	fetchedActionSuffix = "-fetched"

	artifactFetchSuffix = "-artifacts"
	verifyKeyName       = "cosign.pub"
)

// slotSupportsWarmMounts reports whether a slot can take artifacts at checkout.
func slotSupportsWarmMounts(p *corev1.Pod) bool {
	return slices.Contains(strings.Fields(p.Annotations[bridgeFeaturesAnnotation]), warmMountsFeature)
}

// warmStageIntent is the staging share every warm slot boots with.
func warmStageIntent() runtimeintent.FilesystemIntent {
	return runtimeintent.FilesystemIntent{Name: warmStageTag, Tag: warmStageTag, SourcePath: warmStageDir, ReadOnly: true}
}

// addWarmStage gives a warm slot's launcher its staging directory (an emptyDir
// it writes bind mounts into) and the node artifact cache, read-only, as the
// source of those binds.
func addWarmStage(pod *corev1.Pod) {
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: warmStageVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	hasCache := false
	for _, v := range pod.Spec.Volumes {
		hasCache = hasCache || v.Name == artifactCacheVolume
	}
	if !hasCache {
		dirCreate := corev1.HostPathDirectoryOrCreate
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: artifactCacheVolume, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: artifactCacheDir, Type: &dirCreate}}})
	}
	c := &pod.Spec.Containers[0]
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: warmStageVolume, MountPath: warmStageDir})
	mounted := false
	for _, m := range c.VolumeMounts {
		mounted = mounted || m.Name == artifactCacheVolume
	}
	if !mounted {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: artifactCacheVolume, MountPath: artifactCacheDir, ReadOnly: true})
	}
}

// warmArtifactArg is one artifact in the exec action (swiftletd's WarmArtifact).
type warmArtifactArg struct {
	Name           string `json:"name"`
	Digest         string `json:"digest"`
	Layout         string `json:"layout"`
	MountPath      string `json:"mountPath"`
	KeyFingerprint string `json:"keyFingerprint,omitempty"`
	// RepositoryFingerprint names the repository the sandbox resolved the
	// artifact from: the node's entry must have been read from it
	// (materialize/origin.go).
	RepositoryFingerprint string `json:"repositoryFingerprint"`
}

// warmArtifactArgs lists sb's resolved artifacts for the exec action, with
// the fingerprint of each one's cosign key: the node's verification of that
// digest must have used exactly this key.
func warmArtifactArgs(ctx context.Context, c client.Reader, sb *sandboxv1alpha1.SwiftSandbox) ([]warmArtifactArg, error) {
	var out []warmArtifactArg
	for _, r := range resolvedArtifacts(sb) {
		ref, err := name.ParseReference(r.spec.Ref)
		if err != nil {
			return nil, refusedError{fmt.Errorf("artifact %s: %w", r.spec.Name, err)}
		}
		a := warmArtifactArg{Name: r.spec.Name, Digest: r.digest, Layout: r.spec.ArtifactLayout(), MountPath: r.spec.MountPath,
			RepositoryFingerprint: materialize.RepositoryFingerprint(ref.Context().Name())}
		if k := r.spec.VerifyKeySecretRef; k != nil && k.Name != "" {
			var sec corev1.Secret
			if err := c.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: k.Name}, &sec); err != nil {
				return nil, refusedError{fmt.Errorf("artifact %s: verify key %s: %w", r.spec.Name, k.Name, err)}
			}
			key := sec.Data[verifyKeyName]
			if len(key) == 0 {
				return nil, refusedError{fmt.Errorf("artifact %s: verify key Secret %s has no %s", r.spec.Name, k.Name, verifyKeyName)}
			}
			a.KeyFingerprint = materialize.KeyFingerprint(key)
		}
		out = append(out, a)
	}
	return out, nil
}

// artifactMissing returns the names in an ArtifactMissing exec detail.
func artifactMissing(detail string) ([]string, bool) {
	rest, ok := strings.CutPrefix(detail, artifactMissingPrefix)
	if !ok {
		return nil, false
	}
	return strings.Split(rest, ","), true
}

// artifactFetchPod builds the pod that materializes sb's missing artifacts on
// the slot's node: one container per artifact, exactly a cold sandbox's init
// containers (pull Secret, cosign key, read-only modes, markers), at the
// digests the slot was asked for (from its action), not a fresh resolve: a
// tag may have moved since.
func artifactFetchPod(sb *sandboxv1alpha1.SwiftSandbox, slot *corev1.Pod, missing []string) *corev1.Pod {
	asked := map[string]string{}
	var args struct {
		Artifacts []warmArtifactArg `json:"artifacts"`
	}
	if err := json.Unmarshal([]byte(slot.Annotations[annSandboxExecActionArgs]), &args); err == nil {
		for _, a := range args.Artifacts {
			asked[a.Name] = a.Digest
		}
	}
	only := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: sb.ObjectMeta, Spec: sb.Spec}
	only.Spec.Artifacts = nil
	for _, a := range sb.Spec.Artifacts {
		if d := asked[a.Name]; d != "" && slices.Contains(missing, a.Name) {
			only.Spec.Artifacts = append(only.Spec.Artifacts, a)
			only.Status.Artifacts = append(only.Status.Artifacts, sandboxv1alpha1.SandboxArtifactStatus{Name: a.Name, Digest: d, MountPath: a.MountPath})
		}
	}
	containers, volumes := artifactInits(only)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      artifactFetchPodName(sb),
			Namespace: sb.Namespace,
			Labels:    map[string]string{SandboxLabelKey: sb.Name, "sandbox.kubeswift.io/artifact-fetch": "true"},
		},
		Spec: corev1.PodSpec{
			NodeName:                     slot.Spec.NodeName,
			RestartPolicy:                corev1.RestartPolicyNever,
			Containers:                   containers,
			Volumes:                      volumes,
			Tolerations:                  slot.Spec.Tolerations,
			ImagePullSecrets:             slot.Spec.ImagePullSecrets,
			AutomountServiceAccountToken: ptr.To(false),
		},
	}
}

// artifactFetchPodName is unique to the sandbox (a hash of its UID), so two
// sandboxes whose names share a long prefix never share a fetch pod.
func artifactFetchPodName(sb *sandboxv1alpha1.SwiftSandbox) string {
	sum := sha256.Sum256([]byte(sb.UID))
	suffix := artifactFetchSuffix + "-" + hex.EncodeToString(sum[:])[:10]
	base := sb.Name
	if max := 63 - len(suffix); len(base) > max {
		base = strings.TrimRight(base[:max], "-.")
	}
	return base + suffix
}

// fetchState is how a sandbox's artifact fetch stands.
type fetchState int

const (
	fetchRunning fetchState = iota
	fetchDone
	fetchFailed
)

// ensureArtifactFetch creates the fetch pod for missing on slot's node, or
// reports how the existing one stands, and why it failed.
func (r *SwiftSandboxReconciler) ensureArtifactFetch(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, slot *corev1.Pod, missing []string) (fetchState, string, error) {
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: artifactFetchPodName(sb)}, &pod)
	switch {
	case apierrors.IsNotFound(err):
		fp := artifactFetchPod(sb, slot, missing)
		if len(fp.Spec.Containers) == 0 {
			return fetchFailed, "swiftletd reported unknown artifacts missing: " + strings.Join(missing, ","), nil
		}
		if err := controllerutil.SetControllerReference(sb, fp, r.Scheme); err != nil {
			return fetchRunning, "", err
		}
		if err := r.Create(ctx, fp); err != nil && !apierrors.IsAlreadyExists(err) {
			return fetchRunning, "", err
		}
		r.Recorder.Eventf(sb, corev1.EventTypeNormal, "FetchingArtifacts",
			"artifacts %s are not on node %s; fetching them for warm slot %s", strings.Join(missing, ", "), slot.Spec.NodeName, slot.Name)
		return fetchRunning, "", nil
	case err != nil:
		return fetchRunning, "", err
	}
	// Only a pod this sandbox created decides: another one by that name (a
	// user's) is not trusted for its phase or its message.
	if !metav1.IsControlledBy(&pod, sb) {
		return fetchFailed, fmt.Sprintf("pod %s exists and was not created for this sandbox", pod.Name), nil
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return fetchDone, "", nil
	case corev1.PodFailed:
		for _, cs := range pod.Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
				return fetchFailed, fmt.Sprintf("artifact %s: %s", strings.TrimPrefix(cs.Name, artifactInitPrefix),
					firstNonEmpty(strings.TrimSpace(t.Message), "materialize failed")), nil
			}
		}
		return fetchFailed, "artifact fetch pod failed: " + pod.Status.Message, nil
	}
	return fetchRunning, "", nil
}
