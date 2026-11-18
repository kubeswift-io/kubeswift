package swiftsandbox

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// The egress audit of a proxied sandbox: the guest's DNS queries (the
// launcher's dnsmasq), the built-in broker's per-connection decisions and
// the egress credential deliveries, as JSON lines read with the pod logs API
// (`kubectl logs <pod> -c egress-audit`), never by an exec into the
// privileged launcher. It never holds a credential value.
//
// The mechanism is the console's (console.go): swiftletd appends to
// audit.log in the `egress-audit` volume, which holds nothing else, bounded
// (KUBESWIFT_EGRESS_AUDIT_MAX_BYTES); the `egress-audit` container runs
// sandbox-materialize follow-console on it, unprivileged, without a service
// account token, the volume read-only, and stops when swiftletd's FIFO in
// the volume closes. An egress proxy container keeps its own access log
// (`-c egress-proxy`).
const (
	// EgressAuditContainerName is the container whose logs are the audit.
	EgressAuditContainerName = "egress-audit"
	egressAuditVolume        = "egress-audit"
	egressAuditNoToken       = "egress-audit-no-token"
	// egressAuditDir, egressAuditLog and egressAuditFIFO match audit.rs.
	egressAuditDir  = "/var/lib/kubeswift/egress-audit"
	egressAuditLog  = egressAuditDir + "/audit.log"
	egressAuditFIFO = egressAuditDir + "/.launcher"
)

// addEgressAudit gives a proxied launcher the audit volume, the launcher its
// mount and env, and the egress-audit container.
func addEgressAudit(pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) {
	if !sb.Spec.Network.Mode.EgressMediated() {
		return
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes,
		corev1.Volume{Name: egressAuditVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: egressAuditNoToken, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(1<<10, resource.BinarySI)}}},
	)
	for i := range pod.Spec.Containers {
		if c := &pod.Spec.Containers[i]; c.Name == launcherName {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: egressAuditVolume, MountPath: egressAuditDir})
			c.Env = append(c.Env, corev1.EnvVar{Name: "KUBESWIFT_EGRESS_AUDIT_DIR", Value: egressAuditDir})
		}
	}
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:  EgressAuditContainerName,
		Image: SandboxMaterializeImage(),
		Args:  []string{"follow-console", "--file", egressAuditLog, "--until-closed", egressAuditFIFO},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To(consoleUID),
			RunAsGroup:               ptr.To(consoleUID),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Privileged:               ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m"), corev1.ResourceMemory: resource.MustParse("8Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: egressAuditVolume, MountPath: egressAuditDir, ReadOnly: true},
			{Name: egressAuditNoToken, MountPath: serviceAccountPath, ReadOnly: true},
		},
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	})
}
