package swiftsandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

// artifactRequests are the registry requests for sb's artifacts, each with its
// pull Secret (the artifact's own, else spec.imagePullSecret), in spec order.
func artifactRequests(ctx context.Context, c client.Reader, sb *sandboxv1alpha1.SwiftSandbox) ([]resolveRequest, error) {
	reqs := make([]resolveRequest, 0, len(sb.Spec.Artifacts))
	for i := range sb.Spec.Artifacts {
		a := &sb.Spec.Artifacts[i]
		secret := sb.Spec.ImagePullSecret
		if a.PullSecretRef != nil {
			secret = a.PullSecretRef.Name
		}
		auth, err := pullSecretAuth(ctx, c, sb.Namespace, secret, a.Ref)
		if err != nil {
			return nil, refusedError{fmt.Errorf("artifact %s: pull secret %s: %w", a.Name, secret, err)}
		}
		kind := resolveDigestKind
		if artifactMode(a) == materialize.ModeLayout {
			kind = resolveDescriptorKind
		}
		reqs = append(reqs, newResolveRequest(kind, a.Ref, artifactMode(a), artifactCacheDir, auth))
	}
	return reqs, nil
}

// optionalWait is what a resolve does with an optional artifact whose
// registry has not answered (or answered with a transient error).
type optionalWait int

const (
	// skipUnresolvedOptional: a warm checkout claims without it; the request
	// keeps running and its answer serves the next checkout.
	skipUnresolvedOptional optionalWait = iota
	// waitUnresolvedOptional: a cold start waits for it until its deadline.
	waitUnresolvedOptional
)

// artifactDeadline is when an optional artifact stops delaying the workload:
// the sandbox's creation plus its fetchTimeout. Immutable, so a retry or a
// controller restart cannot extend it.
func artifactDeadline(sb *sandboxv1alpha1.SwiftSandbox, a *sandboxv1alpha1.SandboxArtifact) time.Time {
	return sb.CreationTimestamp.Add(a.ArtifactFetchTimeout())
}

// artifactResolution is the outcome of resolving a sandbox's artifacts.
type artifactResolution struct {
	// statuses: one per artifact, in spec order, once ready.
	statuses []sandboxv1alpha1.SandboxArtifactStatus
	// ready: nothing is still being waited for.
	ready bool
	// err: a required artifact's registry error (a refusal outranks a
	// transient error).
	err error
	// wakeAt: the earliest deadline of an optional artifact still waited for.
	wakeAt time.Time
	// failed: when a required artifact was refused, every artifact's status
	// with that one Failed, for the sandbox to record as it fails.
	failed []sandboxv1alpha1.SandboxArtifactStatus
}

// resolveArtifacts resolves each artifact to the digest the node will mount.
// Every request starts before any is waited for, and they share the pass's
// wait.
func resolveArtifacts(ctx context.Context, c client.Reader, sb *sandboxv1alpha1.SwiftSandbox, lookup lookupFunc, wait optionalWait, now time.Time) artifactResolution {
	reqs, err := artifactRequests(ctx, c, sb)
	if err != nil {
		return artifactResolution{ready: true, err: err}
	}
	return resolveArtifactRequests(sb, reqs, lookup, wait, now)
}

// resolveArtifactRequests is resolveArtifacts for requests already built (in
// spec order). A required artifact behaves as it always has: a refusal fails
// the sandbox, anything else waits. An optional one never fails or delays a
// required one: refused, it is skipped; unanswered, it is skipped or waited
// for until its deadline, as wait says.
func resolveArtifactRequests(sb *sandboxv1alpha1.SwiftSandbox, reqs []resolveRequest, lookup lookupFunc, wait optionalWait, now time.Time) artifactResolution {
	results := make([]resolveResult, len(reqs))
	done := make([]bool, len(reqs))
	for i, req := range reqs {
		req.noWait = true
		results[i], done[i] = lookup(req)
	}
	for i, req := range reqs {
		if !done[i] {
			results[i], done[i] = lookup(req)
		}
	}
	out := artifactResolution{ready: true}
	var refused, transient error
	for i := range reqs {
		a := &sb.Spec.Artifacts[i]
		st := sandboxv1alpha1.SandboxArtifactStatus{Name: a.Name, MountPath: a.MountPath}
		res := results[i]
		switch {
		case done[i] && res.err == nil:
			st.Digest, st.State = res.digest, sandboxv1alpha1.SandboxArtifactResolved
		case !a.Optional && !done[i]:
			out.ready = false
		case !a.Optional:
			err := fmt.Errorf("artifact %s: %w", a.Name, res.err)
			if registryRefused(res.err) {
				if refused == nil {
					refused = err
				}
				st.State, st.Reason, st.Message = sandboxv1alpha1.SandboxArtifactFailed, "ArtifactResolveFailed", truncateMessage(res.err.Error())
			} else if transient == nil {
				transient = err
			}
		case done[i] && registryRefused(res.err):
			skipArtifact(&st, "ResolveRefused", res.err.Error())
		default: // optional, unanswered or a transient error
			if d := artifactDeadline(sb, a); wait == waitUnresolvedOptional && now.Before(d) {
				out.ready = false
				if out.wakeAt.IsZero() || d.Before(out.wakeAt) {
					out.wakeAt = d
				}
			} else if !done[i] {
				skipArtifact(&st, "ResolvePending", "the registry had not answered when the sandbox started")
			} else {
				skipArtifact(&st, "RegistryUnavailable", res.err.Error())
			}
		}
		out.statuses = append(out.statuses, st)
	}
	if refused != nil {
		out.err = refused
		out.failed = out.statuses
	} else if transient != nil {
		out.err = transient
	}
	if out.err != nil || !out.ready {
		out.statuses = nil
	}
	return out
}

// skipArtifact records that an optional artifact is not mounted, and why.
func skipArtifact(st *sandboxv1alpha1.SandboxArtifactStatus, reason, msg string) {
	st.State, st.Reason, st.Message = sandboxv1alpha1.SandboxArtifactSkipped, reason, truncateMessage(msg)
}

// truncateMessage bounds a registry or materializer message kept in status.
func truncateMessage(msg string) string {
	const max = 512
	if len(msg) > max {
		return msg[:max] + "..."
	}
	return msg
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
// artifact not resolved, or skipped, is left out.
func resolvedArtifacts(sb *sandboxv1alpha1.SwiftSandbox) []resolvedArtifact {
	digests := map[string]string{}
	for i := range sb.Status.Artifacts {
		if s := &sb.Status.Artifacts[i]; s.Usable() {
			digests[s.Name] = s.Digest
		}
	}
	var out []resolvedArtifact
	for i := range sb.Spec.Artifacts {
		if d := digests[sb.Spec.Artifacts[i].Name]; d != "" {
			out = append(out, resolvedArtifact{spec: &sb.Spec.Artifacts[i], digest: d})
		}
	}
	return out
}

// backgroundFetchTimeout bounds an optional artifact's fetch that no
// workload waits for any more: a warm checkout's fetch past the artifact's
// deadline.
const backgroundFetchTimeout = 10 * time.Minute

// The node artifact cache's use-lock directory (materialize/cachegc.go): a
// hostPath beside the cache, read-write in materializer containers and
// launchers. It holds only lock files.
const (
	artifactUseVolume = "artifact-use"
	artifactUseDir    = "/var/lib/kubeswift/sandbox-artifacts-use"
)

// Environment carrying the chart's artifact cache limits to the controller,
// which passes them to every artifact materializer run.
const (
	artifactCacheMaxBytesEnv   = "KUBESWIFT_ARTIFACT_CACHE_MAX_BYTES"
	artifactCacheMaxPercentEnv = "KUBESWIFT_ARTIFACT_CACHE_MAX_PERCENT"
	artifactCacheMinAgeEnv     = "KUBESWIFT_ARTIFACT_CACHE_MIN_AGE"
)

// artifactCacheArgs are the materializer flags for the use-lock directory and
// the configured limits (unset: no limit).
func artifactCacheArgs() []string {
	args := []string{"--use-dir=" + artifactUseDir}
	if v := os.Getenv(artifactCacheMaxBytesEnv); v != "" && v != "0" {
		if q, err := resource.ParseQuantity(v); err == nil && q.Value() > 0 {
			args = append(args, "--cache-max-bytes="+strconv.FormatInt(q.Value(), 10))
		}
	}
	if v := os.Getenv(artifactCacheMaxPercentEnv); v != "" && v != "0" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			args = append(args, "--cache-max-percent="+strconv.Itoa(n))
		}
	}
	if v := os.Getenv(artifactCacheMinAgeEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			args = append(args, "--cache-min-age="+d.String())
		}
	}
	return args
}

// artifactUseVolumeSource is the use-lock directory's hostPath.
func artifactUseVolumeSource() corev1.Volume {
	dirCreate := corev1.HostPathDirectoryOrCreate
	return corev1.Volume{Name: artifactUseVolume, VolumeSource: corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: artifactUseDir, Type: &dirCreate}}}
}

// artifactResultsVolume is a cold launcher pod's emptyDir where each optional
// artifact's init container writes its ok token (artifactOKFile), and
// artifactResultsDir where both the init containers and the launcher (read
// only) mount it.
const (
	artifactResultsVolume = "artifact-results"
	artifactResultsDir    = "/var/lib/kubeswift/artifact-results"
)

// artifactOKFile is where the init container of optional artifact name
// records that THIS pod resolved, verified and materialized it. swiftletd
// shares an optional artifact only when it exists: never decided from what
// the shared node cache happens to hold.
func artifactOKFile(name string) string { return artifactResultsDir + "/" + name }

// hasOptionalArtifact reports whether any resolved artifact is optional.
func hasOptionalArtifact(sb *sandboxv1alpha1.SwiftSandbox) bool {
	for _, r := range resolvedArtifacts(sb) {
		if r.spec.Optional {
			return true
		}
	}
	return false
}

// artifactShares are the virtio-fs shares and the kubeswift.mounts cmdline
// value for sb's resolved artifacts.
func artifactShares(sb *sandboxv1alpha1.SwiftSandbox) ([]runtimeintent.FilesystemIntent, string) {
	var fs []runtimeintent.FilesystemIntent
	var mounts []string
	for i, r := range resolvedArtifacts(sb) {
		tag := artifactTag(i)
		f := runtimeintent.FilesystemIntent{Name: tag, Tag: tag, SourcePath: r.cachePath(), ReadOnly: true}
		if r.spec.Optional {
			f.OptionalToken = artifactOKFile(r.spec.Name)
		}
		fs = append(fs, f)
		mounts = append(mounts, tag+":"+r.spec.MountPath)
	}
	return fs, strings.Join(mounts, ",")
}

// artifactInits builds one materialize container per resolved artifact, with
// the volumes they need (pull Secret, cosign key, the node cache). An optional
// artifact's container never fails (--optional): a failure is a skip in its
// result, bounded by the artifact's deadline. With tokens (a cold launcher's
// init containers) each optional one also writes its ok token.
func artifactInits(sb *sandboxv1alpha1.SwiftSandbox, tokens bool) ([]corev1.Container, []corev1.Volume) {
	arts := resolvedArtifacts(sb)
	if len(arts) == 0 {
		return nil, nil
	}
	dirCreate := corev1.HostPathDirectoryOrCreate
	volumes := []corev1.Volume{{Name: artifactCacheVolume, VolumeSource: corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: artifactCacheDir, Type: &dirCreate}}}, artifactUseVolumeSource()}
	var inits []corev1.Container
	cosignHome, resultsVolume := false, false
	for i, r := range arts {
		mode := artifactMode(r.spec)
		args := []string{"--image", r.pinned(), "--cache-dir", artifactCacheDir, "--mode", string(mode), "--read-only-artifact", "--result-file", "/dev/termination-log"}
		args = append(args, artifactCacheArgs()...)
		mounts := []corev1.VolumeMount{{Name: artifactCacheVolume, MountPath: artifactCacheDir}, {Name: artifactUseVolume, MountPath: artifactUseDir}}
		if r.spec.Optional {
			// A fetch pod runs on past the artifact's deadline, up to
			// backgroundFetchTimeout: the controller stops waiting for it at
			// the deadline and dispatches without the artifact, and the
			// fetch fills the node cache for the next checkout. Bounding it
			// by the deadline instead meant an artifact slower to fetch than
			// its fetchTimeout was never cached at all. A cold launcher's
			// init container counts its fetchTimeout from now, when the pod
			// is built: the boot waits for it, the time the launch waited
			// for registries already had its own bound, and pod start-up is
			// not the artifact's doing.
			deadline := time.Now().Add(backgroundFetchTimeout)
			if tokens {
				deadline = time.Now().Add(r.spec.ArtifactFetchTimeout())
			}
			args = append(args, "--optional", "--deadline="+deadline.UTC().Format(time.RFC3339))
			if tokens {
				args = append(args, "--ok-file="+artifactOKFile(r.spec.Name))
				mounts = append(mounts, corev1.VolumeMount{Name: artifactResultsVolume, MountPath: artifactResultsDir})
				resultsVolume = true
			}
		}
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
	if resultsVolume {
		volumes = append(volumes, corev1.Volume{Name: artifactResultsVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
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

// artifactResult reads the materialize result an artifact container left in
// its termination message: for an optional artifact, whether it was skipped.
func artifactResult(cs *corev1.ContainerStatus) (materialize.Result, bool) {
	var res materialize.Result
	t := cs.State.Terminated
	if t == nil || t.ExitCode != 0 {
		return res, false
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(t.Message)), &res); err != nil {
		return res, false
	}
	return res, true
}
