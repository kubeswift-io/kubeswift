package swiftmigration

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/projectbeskar/kubeswift/internal/controller/migrationcert"
	"github.com/projectbeskar/kubeswift/internal/migrationsidecar"
)

// Phase 3c (Option B) live-migration mTLS stunnel sidecar wiring.
//
// The sidecar owns the cross-pod TLS hop so Cloud Hypervisor and
// swiftletd stay plaintext-on-localhost and TLS-unaware (the spike's
// load-bearing non-change — design §5). This file builds the
// DESTINATION-side sidecar (TLS server) that newDstPod injects.
//
// PR 3 scope is destination-only. The SOURCE-side sidecar (TLS client)
// is added to the pre-existing launcher pod at SwiftGuest-pod-creation
// time in PR 3b, with DST_POD_IP / CHECK_HOST delivered via a
// downward-API annotation volume + a polling entrypoint (the src pod is
// immutable once running, so env injection at migration time is
// impossible — design §4.2 corrected by the PR 3 walkthrough).
//
// Image and configs are NOT baked together: the image (alpine + stunnel
// + entrypoint, images/migration-stunnel) carries only the binary and a
// role-selecting entrypoint; the server.conf / client.conf arrive via
// the kubeswift-migration-stunnel ConfigMap (charts + kustomize overlay,
// PR 2). The entrypoint self-selects by STUNNEL_ROLE and pins the peer
// via CHECK_HOST (verifyChain + checkHost — W-3c-4).
// The sidecar contract (image, container name, mount paths, env keys,
// roles) lives in internal/migrationsidecar so the SwiftGuest controller
// (source sidecar) and this controller (destination sidecar + PR 3d
// stamping) share one source of truth. These file-local aliases keep the
// destination code + tests reading the same short names introduced in
// PR 3 while sourcing the values from the shared package.
const (
	stunnelSidecarContainerName = migrationsidecar.ContainerName
	stunnelConfigMapName        = migrationsidecar.ConfigMapName
	stunnelConfigDir            = migrationsidecar.ConfigDir
	migrationTLSDir             = migrationsidecar.TLSDir
	stunnelConfigVolumeName     = migrationsidecar.ConfigVolumeName
	stunnelCertVolumeName       = migrationsidecar.CertVolumeName

	envStunnelRole      = migrationsidecar.EnvRole
	envStunnelConfigDir = migrationsidecar.EnvConfigDir
	envStunnelCheckHost = migrationsidecar.EnvCheckHost
	envStunnelDstPodIP  = migrationsidecar.EnvDstPodIP

	stunnelRoleServer = migrationsidecar.RoleServer
	stunnelRoleClient = migrationsidecar.RoleClient
)

// MigrationStunnelImage returns the stunnel sidecar image (shared helper).
func MigrationStunnelImage() string {
	return migrationsidecar.Image()
}

// dstStunnelSidecar builds the destination-side stunnel sidecar
// container (TLS server). srcNodeSAN is the source node's SAN — the dst
// pins it via CHECK_HOST so the channel authorizes the *specific*
// source node for THIS migration, not merely any CA-signed peer
// (W-3c-4). Role/peer are env-parameterized, never image-baked, so a
// future refactor cannot collapse the two roles into separate images
// without first seeing this constraint (W-3c-2 / W26 discipline).
func dstStunnelSidecar(srcNodeSAN string) corev1.Container {
	noEsc := false
	nonRoot := true
	return corev1.Container{
		Name:            stunnelSidecarContainerName,
		Image:           MigrationStunnelImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{Name: envStunnelRole, Value: stunnelRoleServer},
			{Name: envStunnelCheckHost, Value: srcNodeSAN},
			{Name: envStunnelConfigDir, Value: stunnelConfigDir},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: stunnelConfigVolumeName, MountPath: stunnelConfigDir, ReadOnly: true},
			{Name: stunnelCertVolumeName, MountPath: migrationTLSDir, ReadOnly: true},
		},
		// Resources: mirror the source sidecar (PR 5 walkthrough finding) —
		// request a small reservation, set NO CPU limit. TLS decryption of
		// the migration stream is CPU-bound and bursts to line rate; a CPU
		// limit would throttle migration throughput. The dst sidecar
		// previously had no Resources at all; an explicit request gives the
		// scheduler a reservation without capping the burst.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(50, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(32*1024*1024, resource.BinarySI),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(128*1024*1024, resource.BinarySI),
			},
		},
		// Minimal privilege: a TLS proxy needs no Linux capabilities.
		// The image already runs as USER 65534; this makes the
		// non-root + no-cap posture explicit so a node default-policy
		// change can't silently grant more.
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &noEsc,
			RunAsNonRoot:             &nonRoot,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

// dstStunnelVolumes returns the two pod volumes the dst sidecar mounts:
// the shared stunnel-config ConfigMap and this node's per-node identity
// Secret (dstSecretName = migrationcert.MigrationNodeSecretName(dstNode)).
func dstStunnelVolumes(dstSecretName string) []corev1.Volume {
	return []corev1.Volume{
		{
			Name: stunnelConfigVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: stunnelConfigMapName},
				},
			},
		},
		{
			Name: stunnelCertVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: dstSecretName,
				},
			},
		},
	}
}

// injectDstStunnelSidecar makes the dst pod's stunnel sidecar authoritative
// as the TLS SERVER. It is W-3c-2's "flip role post-DeepCopy": newDstPod
// builds the dst by srcPod.DeepCopy(), and since PR 3b the source pod
// carries a CLIENT-role sidecar (+ a downward-API input volume + a
// per-GUEST identity Secret). The dst must instead run a SERVER (+ its
// per-NODE identity Secret, no input volume). So we STRIP any inherited
// sidecar container and the three sidecar volumes, then add the server
// sidecar + server volumes. Correct whether or not the src had a sidecar,
// and idempotent on re-entry.
//
// srcNodeName is the SOURCE node (the dst pins its SAN via checkHost);
// dstNodeName is the DESTINATION node (whose per-node identity Secret the
// server sidecar presents).
func injectDstStunnelSidecar(pod *corev1.Pod, srcNodeName, dstNodeName string) {
	pod.Spec.Containers = removeContainerByName(pod.Spec.Containers, stunnelSidecarContainerName)
	pod.Spec.Volumes = removeVolumesByName(pod.Spec.Volumes,
		migrationsidecar.ConfigVolumeName, migrationsidecar.CertVolumeName, migrationsidecar.InputVolumeName)
	pod.Spec.Containers = append(pod.Spec.Containers,
		dstStunnelSidecar(migrationcert.MigrationNodeCertSAN(srcNodeName)))
	pod.Spec.Volumes = append(pod.Spec.Volumes,
		dstStunnelVolumes(migrationcert.MigrationNodeSecretName(dstNodeName))...)
}

// removeContainerByName returns containers with any entry named `name`
// removed (preserving order).
func removeContainerByName(containers []corev1.Container, name string) []corev1.Container {
	out := containers[:0:0]
	for _, c := range containers {
		if c.Name != name {
			out = append(out, c)
		}
	}
	return out
}

// removeVolumesByName returns volumes with any entry whose name is in
// `names` removed (preserving order).
func removeVolumesByName(volumes []corev1.Volume, names ...string) []corev1.Volume {
	drop := make(map[string]struct{}, len(names))
	for _, n := range names {
		drop[n] = struct{}{}
	}
	out := volumes[:0:0]
	for _, v := range volumes {
		if _, ok := drop[v.Name]; !ok {
			out = append(out, v)
		}
	}
	return out
}
