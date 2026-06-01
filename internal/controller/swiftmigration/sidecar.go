package swiftmigration

import (
	"os"

	corev1 "k8s.io/api/core/v1"

	"github.com/projectbeskar/kubeswift/internal/controller/migrationcert"
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
const (
	// MigrationStunnelImageEnv overrides the sidecar image. Mirrors
	// swiftguest.LauncherImageEnv. The release pipeline sets this to the
	// version-stamped tag; local dev and tests fall back to the default.
	MigrationStunnelImageEnv = "KUBESWIFT_MIGRATION_STUNNEL_IMAGE"
	// MigrationStunnelImageDefault is the default sidecar image.
	MigrationStunnelImageDefault = "ghcr.io/projectbeskar/kubeswift/migration-stunnel:latest"

	// stunnelSidecarContainerName is the dst pod's sidecar container.
	// Distinct from LauncherContainerName so dstPodMatches and the
	// launcher-container lookups (addReceiverEnvToLauncher,
	// launcherContainerImage) never mistake it for the swiftletd
	// container.
	stunnelSidecarContainerName = "migration-stunnel"

	// stunnelConfigMapName is the ConfigMap carrying server.conf +
	// client.conf (PR 2: charts/kubeswift/templates/migration/
	// stunnel-configmap.yaml + config/overlays/migration-mtls). Must
	// exist in the guest namespace for the sidecar to start; the Helm
	// chart/overlay provision it in .Values.namespace, and operators
	// running migrations in other namespaces must replicate it (PR 5
	// documents this — same shape as the migration identity Secret copy).
	stunnelConfigMapName = "kubeswift-migration-stunnel"

	// stunnelConfigDir is where the ConfigMap mounts (matches the PR 2
	// entrypoint's STUNNEL_CONFIG_DIR default).
	stunnelConfigDir = "/etc/stunnel-config"
	// migrationTLSDir is where the per-node identity Secret mounts
	// (matches the cert/key/CAfile paths baked into the PR 2 configs).
	migrationTLSDir = "/etc/migration-tls"

	// stunnelConfigVolumeName / stunnelCertVolumeName name the two
	// sidecar volumes on the dst pod.
	stunnelConfigVolumeName = "migration-stunnel-config"
	stunnelCertVolumeName   = "migration-tls"

	// Env keys the entrypoint reads (PR 2 entrypoint.sh contract).
	envStunnelRole      = "STUNNEL_ROLE"
	envStunnelConfigDir = "STUNNEL_CONFIG_DIR"
	envStunnelCheckHost = "CHECK_HOST"
	// envStunnelDstPodIP is the client-only connect target. Destination
	// sidecars (server role) do not set it; PR 3b stamps it on the src
	// sidecar via downward API.
	envStunnelDstPodIP = "DST_POD_IP"

	// stunnelRoleServer is the dst sidecar role (TLS server: accept
	// 0.0.0.0:6789, forward to the local CH receiver on 127.0.0.1:6790).
	stunnelRoleServer = "server"
	// stunnelRoleClient is the src sidecar role (PR 3b).
	stunnelRoleClient = "client"
)

// MigrationStunnelImage returns the stunnel sidecar image, from
// KUBESWIFT_MIGRATION_STUNNEL_IMAGE env or MigrationStunnelImageDefault.
func MigrationStunnelImage() string {
	if img := os.Getenv(MigrationStunnelImageEnv); img != "" {
		return img
	}
	return MigrationStunnelImageDefault
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

// injectDstStunnelSidecar appends the destination stunnel sidecar
// container and its two volumes to the dst pod. Idempotent: a pod that
// already carries a container named stunnelSidecarContainerName is left
// untouched (defensive against a future DeepCopy bringing a sidecar
// over — though in PR 3 the src pod has no sidecar yet).
//
// srcNodeName is the SOURCE node (the dst pins its SAN as the expected
// peer); dstNodeName is the DESTINATION node (whose identity Secret the
// sidecar presents).
func injectDstStunnelSidecar(pod *corev1.Pod, srcNodeName, dstNodeName string) {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == stunnelSidecarContainerName {
			return
		}
	}
	pod.Spec.Containers = append(pod.Spec.Containers,
		dstStunnelSidecar(migrationcert.MigrationNodeCertSAN(srcNodeName)))
	pod.Spec.Volumes = append(pod.Spec.Volumes,
		dstStunnelVolumes(migrationcert.MigrationNodeSecretName(dstNodeName))...)
}
