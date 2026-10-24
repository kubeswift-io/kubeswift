package swiftsandbox

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

// The sandbox console: a launcher pod's guest console (cold) or checked-out
// workload output (warm slot), read with the pod logs API as the `console`
// container, never by an exec into the privileged launcher.
//
// The launcher (swiftletd, rust/swiftletd/src/console.rs) writes the console
// into the `console` volume, which holds nothing else. The `console`
// container runs sandbox-materialize follow-console: unprivileged, no service
// account token, that volume read-only, no other volume. It is an ordinary
// container started with the launcher, not an init-ordered sidecar (that
// costs every cold start a kubelet step, about a second), so it ends with the
// launcher by itself: swiftletd holds a FIFO in the volume open for its life,
// and the console container stops at its EOF. It always exits 0: the
// launcher's exit alone decides the pod's phase. Reading it takes `get pods/log` on the launcher pod, in the
// sandbox's namespace (`kubectl logs <pod> -c console`); no exec, attach or
// node access. A warm slot's console container follows only the workload
// log, written from the checkout's dispatch on, so the slot's boot (and
// nothing from before the checkout) is in it.

const (
	// ConsoleContainerName is the container whose logs are the console.
	ConsoleContainerName = "console"
	consoleVolume        = "console"
	consoleDir           = "/var/lib/kubeswift/console"
	// consoleSerialLog and consoleWorkloadLog match console.rs.
	consoleSerialLog   = consoleDir + "/serial.log"
	consoleWorkloadLog = consoleDir + "/workload.log"
	consoleAliveFIFO   = consoleDir + "/.launcher"
	// consoleNoToken masks the pod's service account token in the console
	// container: the launcher needs it, the console never does.
	consoleNoToken     = "console-no-token"
	serviceAccountPath = "/var/run/secrets/kubernetes.io/serviceaccount"
	consoleUID         = int64(65532)
)

// ConsoleEnabled adds the console container to sandbox launcher pods built
// from now on (--sandbox-console). Existing pods are not changed.
var ConsoleEnabled = true

// addConsole gives pod the console volume, the launcher its env and mount,
// and the console container following file.
func addConsole(pod *corev1.Pod, file string) {
	if !ConsoleEnabled {
		return
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes,
		corev1.Volume{Name: consoleVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: consoleNoToken, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(1<<10, resource.BinarySI)}}},
	)
	for i := range pod.Spec.Containers {
		if c := &pod.Spec.Containers[i]; c.Name == launcherName {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: consoleVolume, MountPath: consoleDir})
			c.Env = append(c.Env, corev1.EnvVar{Name: "KUBESWIFT_CONSOLE_DIR", Value: consoleDir})
		}
	}
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
		Name:  ConsoleContainerName,
		Image: SandboxMaterializeImage(),
		Args:  consoleArgs(file),
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
			// It copies through a fixed buffer: its memory does not grow with
			// what the workload writes.
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: consoleVolume, MountPath: consoleDir, ReadOnly: true},
			{Name: consoleNoToken, MountPath: serviceAccountPath, ReadOnly: true},
		},
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	})
}

// followWorkloadLog points a warm slot's console container at the workload
// log: the slot's boot is not its sandbox's console.
func followWorkloadLog(pod *corev1.Pod) {
	for i := range pod.Spec.Containers {
		if c := &pod.Spec.Containers[i]; c.Name == ConsoleContainerName {
			c.Args = consoleArgs(consoleWorkloadLog)
		}
	}
}

func consoleArgs(file string) []string {
	return []string{"follow-console", "--file", file, "--until-closed", consoleAliveFIFO}
}
