// Package migrationsidecar is the single source of truth for the Phase 3c
// live-migration mTLS stunnel sidecar contract: the image, container name,
// mount paths, env keys, role values, and the controller<->sidecar
// annotation keys.
//
// It exists because the two controllers that touch the sidecar cannot
// import each other:
//   - the SwiftMigration controller injects the DESTINATION sidecar and
//     (PR 3d) stamps the source pod's dst-ip / peer-san annotations,
//   - the SwiftGuest controller injects the SOURCE sidecar and the
//     downward-API volume that surfaces those annotations to it,
//
// and swiftmigration already imports swiftguest (a cycle the other way).
// Centralising the contract here keeps the annotation keys the controller
// WRITES identical to the files the sidecar READS — a drift here would
// silently break mTLS (the sidecar would idle forever waiting on a key the
// controller never sets).
package migrationsidecar

import "os"

const (
	// ImageEnv overrides the sidecar image; ImageDefault is the fallback.
	// The release pipeline sets ImageEnv to the version-stamped tag.
	ImageEnv     = "KUBESWIFT_MIGRATION_STUNNEL_IMAGE"
	ImageDefault = "ghcr.io/projectbeskar/kubeswift/migration-stunnel:latest"

	// ContainerName is the sidecar container name on both src and dst pods.
	// Distinct from the launcher container so launcher-by-name lookups
	// never mistake it.
	ContainerName = "migration-stunnel"

	// ConfigMapName carries server.conf + client.conf (PR 2). Provisioned
	// in the system namespace by the Helm chart / kustomize overlay and
	// copied into guest namespaces by the SwiftGuest controller.
	ConfigMapName = "kubeswift-migration-stunnel"

	// Mount paths (match the PR 2 entrypoint defaults + the cert/key/CAfile
	// paths baked into the PR 2 stunnel configs).
	ConfigDir = "/etc/stunnel-config"  // ConfigMap (server.conf/client.conf)
	TLSDir    = "/etc/migration-tls"   // identity Secret (tls.crt/key/ca.crt)
	InputDir  = "/etc/migration-input" // downward-API inputs (client only)

	// Pod volume names.
	ConfigVolumeName = "migration-stunnel-config"
	CertVolumeName   = "migration-tls"
	InputVolumeName  = "migration-input"

	// Env keys the entrypoint reads.
	EnvRole      = "STUNNEL_ROLE"
	EnvConfigDir = "STUNNEL_CONFIG_DIR"
	EnvInputDir  = "STUNNEL_INPUT_DIR"
	EnvCheckHost = "CHECK_HOST" // server: peer SAN by env; client reads InputDir
	EnvDstPodIP  = "DST_POD_IP" // server never sets it; client reads InputDir

	// Role values.
	RoleServer = "server" // destination: TLS server, inputs by env at start
	RoleClient = "client" // source: TLS client, idle-polls InputDir + TLSDir

	// Controller-stamped SOURCE-pod annotations. The SwiftGuest controller's
	// downward-API volume projects these into InputFileDstIP / InputFilePeerSAN
	// under InputDir; the SwiftMigration controller (PR 3d) writes them when a
	// migration starts. These two keys are the load-bearing contract — keep
	// the writer (swiftmigration) and the reader (this volume) in lockstep.
	AnnotationDstPodIP = "kubeswift.io/migration-dst-ip"
	AnnotationPeerSAN  = "kubeswift.io/migration-peer-san"

	// Downward-API projected file names under InputDir (what the entrypoint
	// reads).
	InputFileDstIP   = "dst-ip"
	InputFilePeerSAN = "peer-san"
)

// Image returns the sidecar image, from ImageEnv or ImageDefault.
func Image() string {
	if img := os.Getenv(ImageEnv); img != "" {
		return img
	}
	return ImageDefault
}
