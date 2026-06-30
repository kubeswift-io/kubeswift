package swiftguest

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/migrationsidecar"
)

// Phase 3c (Option B) SOURCE-side live-migration mTLS stunnel sidecar.
//
// Unlike the destination sidecar (injected by the SwiftMigration controller
// at migration time, when the target node is known), the source sidecar
// must live inside the pre-existing, immutable launcher pod — you cannot
// add a container or env to a running pod. So when mTLS is enabled, EVERY
// migration-eligible launcher pod is born with an IDLE stunnel client
// sidecar. It idle-polls a downward-API volume + the per-guest identity
// Secret for the inputs a migration stamps later (design §4.2, corrected by
// the PR 3 walkthrough: the source pod's node may not even be known at
// creation time, which is why the identity arrives via a per-guest Secret
// populated at migration time rather than a per-node Secret mounted here).
//
// This file (PR 3b) only wires the IDLE sidecar. The SwiftMigration
// controller activates it (PR 3d) by populating the per-guest identity
// Secret and stamping the dst-ip / peer-san annotations.

// migrationIdentitySecretSuffix forms the per-guest identity Secret name.
const migrationIdentitySecretSuffix = "-migration-identity"

// PerGuestMigrationIdentitySecretName returns the name of the per-guest
// Secret that holds the SOURCE guest's migration identity (tls.crt /
// tls.key / ca.crt). The SwiftGuest controller ensures it exists (empty)
// at pod creation; the SwiftMigration controller (PR 3d) populates it with
// the source node's issued identity at migration time.
func PerGuestMigrationIdentitySecretName(guestName string) string {
	return guestName + migrationIdentitySecretSuffix
}

// migrationEligible reports whether SwiftMigrations may target this guest.
// Mirrors the SwiftMigration webhook's eligibility rule: migration is
// enabled unless explicitly disabled (spec.migration.enabled=false). Only
// eligible guests get the source sidecar — pinned-in-place guests
// (enabled=false) never migrate, so the idle sidecar would be dead weight.
func migrationEligible(guest *swiftv1alpha1.SwiftGuest) bool {
	if guest.Spec.Migration == nil || guest.Spec.Migration.Enabled == nil {
		return true
	}
	return *guest.Spec.Migration.Enabled
}

// sourceStunnelSidecar builds the source-side stunnel client sidecar.
// It carries NO peer SAN / dst IP in env: those are per-migration and
// unknown at pod creation, so the entrypoint idle-polls the downward-API
// input volume (and the per-guest identity Secret) and only starts stunnel
// once a migration stamps them. Role/peer are env/file-parameterized,
// never image-baked (W-3c-2).
func sourceStunnelSidecar() corev1.Container {
	noEsc := false
	nonRoot := true
	return corev1.Container{
		Name:            migrationsidecar.ContainerName,
		Image:           migrationsidecar.Image(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{Name: migrationsidecar.EnvRole, Value: migrationsidecar.RoleClient},
			{Name: migrationsidecar.EnvConfigDir, Value: migrationsidecar.ConfigDir},
			{Name: migrationsidecar.EnvInputDir, Value: migrationsidecar.InputDir},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: migrationsidecar.ConfigVolumeName, MountPath: migrationsidecar.ConfigDir, ReadOnly: true},
			{Name: migrationsidecar.CertVolumeName, MountPath: migrationsidecar.TLSDir, ReadOnly: true},
			{Name: migrationsidecar.InputVolumeName, MountPath: migrationsidecar.InputDir, ReadOnly: true},
		},
		// No readinessProbe: the sidecar is "ready" (running) at rest, and a
		// data-port probe would mark the launcher pod NotReady whenever no
		// migration is in flight.
		//
		// Resources: TLS encryption of the migration stream is CPU-bound and
		// bursts to line rate (~112 MB/s) during the brief transfer window.
		// A CPU *limit* throttles throughput and cripples migration time —
		// PR 5 cluster walkthrough: a 100m limit dropped a 4 GiB migration to
		// ~7 MB/s (~16x slower) and it failed when CH errored on the
		// abnormally long send. So request a small reservation for
		// scheduling and set NO CPU limit (the proxy is idle at rest and
		// only bursts during a migration). Keep a modest memory limit —
		// stunnel's buffers are small.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(50, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(32*1024*1024, resource.BinarySI),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(128*1024*1024, resource.BinarySI),
			},
		},
		// Minimal privilege: a TLS proxy needs no Linux capabilities. The
		// image already runs as USER 65534; making it explicit prevents a
		// node default-policy change from silently granting more.
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &noEsc,
			RunAsNonRoot:             &nonRoot,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

// sourceStunnelVolumes returns the three volumes the source sidecar mounts:
//   - the shared stunnel-config ConfigMap (server.conf/client.conf),
//   - the per-guest identity Secret (empty until a migration populates it),
//   - a downward-API volume projecting the controller-stamped dst-ip /
//     peer-san annotations into files the entrypoint polls.
func sourceStunnelVolumes(guestName string) []corev1.Volume {
	return []corev1.Volume{
		{
			Name: migrationsidecar.ConfigVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: migrationsidecar.ConfigMapName},
				},
			},
		},
		{
			Name: migrationsidecar.CertVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: PerGuestMigrationIdentitySecretName(guestName),
				},
			},
		},
		{
			Name: migrationsidecar.InputVolumeName,
			VolumeSource: corev1.VolumeSource{
				DownwardAPI: &corev1.DownwardAPIVolumeSource{
					Items: []corev1.DownwardAPIVolumeFile{
						{
							Path:     migrationsidecar.InputFileDstIP,
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: fmt.Sprintf("metadata.annotations['%s']", migrationsidecar.AnnotationDstPodIP)},
						},
						{
							Path:     migrationsidecar.InputFilePeerSAN,
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: fmt.Sprintf("metadata.annotations['%s']", migrationsidecar.AnnotationPeerSAN)},
						},
					},
				},
			},
		},
	}
}

// applyMigrationSourceSidecar appends the idle source stunnel client
// sidecar and its three volumes to the launcher pod, and sets the
// secured-mode env on the launcher container. Idempotent: a pod that
// already carries the sidecar container is left untouched.
func applyMigrationSourceSidecar(pod *corev1.Pod, guest *swiftv1alpha1.SwiftGuest) {
	// Secured-mode signal for swiftletd (PR 4): set on the launcher
	// container, NOT the sidecar. The dst launcher inherits it via
	// newDstPod's DeepCopy. Set first so it lands even on the idempotent
	// re-entry guard below (a pod created before this code shipped that
	// already has the sidecar still gets the env on the next reconcile-
	// driven rebuild).
	setLauncherMTLSEnv(pod)
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == migrationsidecar.ContainerName {
			return
		}
	}
	pod.Spec.Containers = append(pod.Spec.Containers, sourceStunnelSidecar())
	pod.Spec.Volumes = append(pod.Spec.Volumes, sourceStunnelVolumes(guest.Name)...)
}

// setLauncherMTLSEnv sets KUBESWIFT_MIGRATION_MTLS=1 on the launcher
// container so swiftletd enters secured mode (S1 loopback enforcement +
// plaintext-ack bypass). Idempotent: replaces any existing entry.
func setLauncherMTLSEnv(pod *corev1.Pod) {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != "launcher" {
			continue
		}
		filtered := c.Env[:0]
		for _, e := range c.Env {
			if e.Name != migrationsidecar.EnvMTLSEnabled {
				filtered = append(filtered, e)
			}
		}
		c.Env = append(filtered, corev1.EnvVar{
			Name:  migrationsidecar.EnvMTLSEnabled,
			Value: migrationsidecar.EnvMTLSEnabledValue,
		})
		return
	}
}

// EnsureMigrationStunnelConfig copies the kubeswift-migration-stunnel
// ConfigMap (server.conf + client.conf) from the system namespace into the
// guest namespace so the source sidecar can mount it. Same-namespace
// fast-path: when the guest runs in the system namespace, the chart/overlay
// already provisioned it there.
//
// The source ConfigMap missing in the system namespace is a precondition
// failure (operator enabled mTLS without installing the migration-mtls
// overlay / Helm value) — surfaced as an error so the reconcile requeues
// and the gap is visible, rather than creating a pod that mounts a missing
// ConfigMap and never starts. Create-if-absent, update-if-differs keeps the
// copy current. No ownerRef: like the swiftletd-reporter RoleBinding it is
// shared across all guests in the namespace and must outlive any one of them.
func EnsureMigrationStunnelConfig(ctx context.Context, c client.Client, systemNamespace, targetNamespace string) error {
	if targetNamespace == systemNamespace {
		return nil
	}
	srcKey := types.NamespacedName{Namespace: systemNamespace, Name: migrationsidecar.ConfigMapName}
	var source corev1.ConfigMap
	if err := c.Get(ctx, srcKey, &source); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("migration stunnel ConfigMap %s not found (install the migration-mtls overlay / Helm value before enabling mTLS): %w", srcKey, err)
		}
		return fmt.Errorf("get source migration stunnel ConfigMap %s: %w", srcKey, err)
	}

	targetKey := types.NamespacedName{Namespace: targetNamespace, Name: migrationsidecar.ConfigMapName}
	var existing corev1.ConfigMap
	err := c.Get(ctx, targetKey, &existing)
	if err == nil {
		if configMapDataEqual(existing.Data, source.Data) && configMapBinaryEqual(existing.BinaryData, source.BinaryData) {
			return nil
		}
		existing.Data = source.Data
		existing.BinaryData = source.BinaryData
		if uerr := c.Update(ctx, &existing); uerr != nil {
			return fmt.Errorf("update copied migration stunnel ConfigMap %s: %w", targetKey, uerr)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get target migration stunnel ConfigMap %s: %w", targetKey, err)
	}

	copyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migrationsidecar.ConfigMapName,
			Namespace: targetNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kubeswift",
				"app.kubernetes.io/component":  "migration-mtls",
				"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
			},
		},
		Data:       source.Data,
		BinaryData: source.BinaryData,
	}
	if cerr := c.Create(ctx, copyCM); cerr != nil {
		if apierrors.IsAlreadyExists(cerr) {
			return nil
		}
		return fmt.Errorf("create copied migration stunnel ConfigMap %s: %w", targetKey, cerr)
	}
	return nil
}

// EnsurePerGuestMigrationIdentitySecret idempotently ensures the per-guest
// identity Secret EXISTS (so the launcher pod's sidecar volume can mount
// it). It is created EMPTY and owned by the SwiftGuest (GC on guest delete).
// The SwiftMigration controller (PR 3d) populates it with the source node's
// issued cert/key/ca at migration time.
//
// Critically, this never clobbers an existing Secret's Data: a migration in
// flight may have just populated it, and a SwiftGuest reconcile must not
// wipe the identity out from under the sidecar.
func EnsurePerGuestMigrationIdentitySecret(ctx context.Context, c client.Client, scheme *runtime.Scheme, guest *swiftv1alpha1.SwiftGuest) error {
	key := types.NamespacedName{Namespace: guest.Namespace, Name: PerGuestMigrationIdentitySecretName(guest.Name)}
	var existing corev1.Secret
	err := c.Get(ctx, key, &existing)
	if err == nil {
		return nil // exists — do not touch its Data (may be migration-populated)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get per-guest migration identity Secret %s: %w", key, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kubeswift",
				"app.kubernetes.io/component":  "migration-mtls",
				"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
				"swift.kubeswift.io/guest":     guest.Name,
			},
		},
		// Opaque + empty: a kubernetes.io/tls Secret would require tls.crt
		// and tls.key at creation, but the placeholder has neither until a
		// migration populates it. stunnel reads files by path, type-agnostic.
		Type: corev1.SecretTypeOpaque,
	}
	if err := controllerutil.SetControllerReference(guest, secret, scheme); err != nil {
		return fmt.Errorf("set ownerRef on per-guest migration identity Secret %s: %w", key, err)
	}
	if cerr := c.Create(ctx, secret); cerr != nil {
		if apierrors.IsAlreadyExists(cerr) {
			return nil
		}
		return fmt.Errorf("create per-guest migration identity Secret %s: %w", key, cerr)
	}
	return nil
}

func configMapDataEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || av != bv {
			return false
		}
	}
	return true
}

func configMapBinaryEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || !bytes.Equal(av, bv) {
			return false
		}
	}
	return true
}
