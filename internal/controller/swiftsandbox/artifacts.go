package swiftsandbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// spec.artifacts: OCI artifacts pulled on the node, cached by digest and
// mounted read-only into the guest over virtio-fs, as spec.model is. The
// controller resolves each to a digest (status.artifacts); one init container
// per artifact pulls and verifies it into the node cache; the launcher shares
// each cache entry under its own virtio-fs tag; the bridge mounts the tags
// (kubeswift.mounts, feature "mounts").

const (
	artifactCacheDir       = "/var/lib/kubeswift/sandbox-artifacts"
	artifactCacheVolume    = "artifact-cache"
	artifactInitPrefix     = "artifact-"
	artifactCosignHomeName = "artifact-cosign-home"
)

func artifactMode(a *sandboxv1alpha1.SandboxArtifact) materialize.Mode {
	if a.ArtifactLayout() == "unpacked" {
		return materialize.ModeTree
	}
	return materialize.ModeLayout
}

// artifactTag is the virtio-fs tag of the i-th artifact (short and fixed: a
// tag is at most 36 bytes, and the mount path rides beside it).
func artifactTag(i int) string { return fmt.Sprintf("sbxart%d", i) }

// resolveArtifacts resolves each artifact to the digest the node will mount,
// with its pull Secret (the artifact's own, else spec.imagePullSecret). Every
// request starts at once; ok=false while any is still running.
func resolveArtifacts(ctx context.Context, c client.Reader, sb *sandboxv1alpha1.SwiftSandbox, lookup lookupFunc) ([]sandboxv1alpha1.SandboxArtifactStatus, bool, error) {
	out := make([]sandboxv1alpha1.SandboxArtifactStatus, 0, len(sb.Spec.Artifacts))
	ready := true
	var firstErr error
	for i := range sb.Spec.Artifacts {
		a := &sb.Spec.Artifacts[i]
		secret := sb.Spec.ImagePullSecret
		if a.PullSecretRef != nil {
			secret = a.PullSecretRef.Name
		}
		auth, err := pullSecretAuth(ctx, c, sb.Namespace, secret, a.Ref)
		if err != nil {
			return nil, true, refusedError{fmt.Errorf("artifact %s: pull secret %s: %w", a.Name, secret, err)}
		}
		kind := resolveDigestKind
		if artifactMode(a) == materialize.ModeLayout {
			kind = resolveDescriptorKind
		}
		res, ok := lookup(newResolveRequest(kind, a.Ref, artifactMode(a), artifactCacheDir, auth))
		switch {
		case !ok:
			ready = false
		case res.err != nil:
			if firstErr == nil {
				firstErr = fmt.Errorf("artifact %s: %w", a.Name, res.err)
			}
		default:
			out = append(out, sandboxv1alpha1.SandboxArtifactStatus{Name: a.Name, Digest: res.digest, MountPath: a.MountPath})
		}
	}
	if firstErr != nil {
		return nil, true, firstErr
	}
	return out, ready, nil
}

// resolvedArtifact pairs an artifact with its resolved digest from status.
type resolvedArtifact struct {
	spec   *sandboxv1alpha1.SandboxArtifact
	digest string
}

// pinned is the artifact's repository at its resolved digest: what the node
// pulls and verifies, so a moved tag cannot change what is mounted.
func (r resolvedArtifact) pinned() string {
	ref, err := name.ParseReference(r.spec.Ref)
	if err != nil {
		return r.spec.Ref
	}
	return ref.Context().Name() + "@" + r.digest
}

func (r resolvedArtifact) cachePath() string {
	return materialize.CachePathFor(artifactCacheDir, r.digest, artifactMode(r.spec))
}

// resolvedArtifacts pairs spec.artifacts with status.artifacts by name; an
// artifact not yet resolved is left out.
func resolvedArtifacts(sb *sandboxv1alpha1.SwiftSandbox) []resolvedArtifact {
	digests := map[string]string{}
	for _, s := range sb.Status.Artifacts {
		digests[s.Name] = s.Digest
	}
	var out []resolvedArtifact
	for i := range sb.Spec.Artifacts {
		if d := digests[sb.Spec.Artifacts[i].Name]; d != "" {
			out = append(out, resolvedArtifact{spec: &sb.Spec.Artifacts[i], digest: d})
		}
	}
	return out
}

// artifactShares are the virtio-fs shares and the kubeswift.mounts cmdline
// value for sb's resolved artifacts.
func artifactShares(sb *sandboxv1alpha1.SwiftSandbox) ([]runtimeintent.FilesystemIntent, string) {
	var fs []runtimeintent.FilesystemIntent
	var mounts []string
	for i, r := range resolvedArtifacts(sb) {
		tag := artifactTag(i)
		fs = append(fs, runtimeintent.FilesystemIntent{Name: tag, Tag: tag, SourcePath: r.cachePath(), ReadOnly: true})
		mounts = append(mounts, tag+":"+r.spec.MountPath)
	}
	return fs, strings.Join(mounts, ",")
}

// artifactInits builds one materialize init container per resolved artifact,
// with the volumes they need (pull Secret, cosign key, the node cache).
func artifactInits(sb *sandboxv1alpha1.SwiftSandbox) ([]corev1.Container, []corev1.Volume) {
	arts := resolvedArtifacts(sb)
	if len(arts) == 0 {
		return nil, nil
	}
	dirCreate := corev1.HostPathDirectoryOrCreate
	volumes := []corev1.Volume{{Name: artifactCacheVolume, VolumeSource: corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: artifactCacheDir, Type: &dirCreate}}}}
	var inits []corev1.Container
	cosignHome := false
	for i, r := range arts {
		mode := artifactMode(r.spec)
		args := []string{"--image", r.pinned(), "--cache-dir", artifactCacheDir, "--mode", string(mode), "--read-only-artifact", "--result-file", "/dev/termination-log"}
		mounts := []corev1.VolumeMount{{Name: artifactCacheVolume, MountPath: artifactCacheDir}}
		var env []corev1.EnvVar
		secret := sb.Spec.ImagePullSecret
		if r.spec.PullSecretRef != nil {
			secret = r.spec.PullSecretRef.Name
		}
		if secret != "" {
			vol := fmt.Sprintf("artifact-pull-%d", i)
			args = append(args, "--pull-secret=/"+vol+"/config.json")
			mounts = append(mounts, corev1.VolumeMount{Name: vol, MountPath: "/" + vol, ReadOnly: true})
			volumes = append(volumes, corev1.Volume{Name: vol, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: secret, Items: []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}}}}})
		}
		if k := r.spec.VerifyKeySecretRef; k != nil && k.Name != "" {
			vol := fmt.Sprintf("artifact-verify-%d", i)
			args = append(args, "--verify-key=/"+vol+"/cosign.pub")
			env = append(env, corev1.EnvVar{Name: "HOME", Value: "/cosign-home"}, corev1.EnvVar{Name: "TMPDIR", Value: "/cosign-home"})
			mounts = append(mounts,
				corev1.VolumeMount{Name: vol, MountPath: "/" + vol, ReadOnly: true},
				corev1.VolumeMount{Name: artifactCosignHomeName, MountPath: "/cosign-home"})
			volumes = append(volumes, corev1.Volume{Name: vol, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: k.Name, Items: []corev1.KeyToPath{{Key: "cosign.pub", Path: "cosign.pub"}}}}})
			cosignHome = true
		}
		inits = append(inits, corev1.Container{
			Name:            artifactInitPrefix + r.spec.Name,
			Image:           SandboxMaterializeImage(),
			Args:            args,
			Env:             env,
			SecurityContext: privileged(),
			VolumeMounts:    mounts,
			// A failed pull or signature check says why on the sandbox.
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		})
	}
	if cosignHome {
		volumes = append(volumes, corev1.Volume{Name: artifactCosignHomeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	}
	return inits, volumes
}

// artifactInitFailure names a failed artifact init container and why.
func artifactInitFailure(pod *corev1.Pod) (string, bool) {
	for i := range pod.Status.InitContainerStatuses {
		cs := &pod.Status.InitContainerStatuses[i]
		if strings.HasPrefix(cs.Name, artifactInitPrefix) && cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			name := strings.TrimPrefix(cs.Name, artifactInitPrefix)
			return fmt.Sprintf("artifact %s: %s", name, firstNonEmpty(strings.TrimSpace(cs.State.Terminated.Message), "materialize failed")), true
		}
	}
	return "", false
}
