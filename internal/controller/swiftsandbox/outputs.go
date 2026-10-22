package swiftsandbox

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

// spec.outputs plumbing on the launcher pod. swiftletd builds each output's
// staging filesystem and share itself (rust/swiftletd/src/outputs.rs); the
// pod gives it room to do so and the node spool to leave packages in.
const (
	outputStagingVolume = "sandbox-output-staging"
	outputStagingDir    = "/var/lib/kubeswift/sandbox-output-staging"
	outputSpoolVolume   = "sandbox-output-spool"
	// OutputSpoolDir is the node directory packaged outputs are left in, one
	// subdirectory per sandbox UID. The launcher and the export Job mount
	// only that subdirectory.
	OutputSpoolDir      = "/var/lib/kubeswift/sandbox-outputs"
	outputTagPrefix     = "sbxout"
	staticOutputReserve = 64 << 20
)

// OutputImageSize is the staging image swiftletd makes for maxBytes of
// content (mirrors outputs::image_size).
func OutputImageSize(maxBytes int64) int64 {
	return maxBytes + max(int64(staticOutputReserve), maxBytes/16)
}

// outputLimits are an output's effective limits. A policy's ceilings apply
// when the destination is resolved.
func outputLimits(o sandboxv1alpha1.SandboxOutput) (maxBytes, maxEntries int64) {
	maxBytes = sandboxv1alpha1.DefaultOutputMaxSize.Value()
	if o.MaxSize != nil {
		maxBytes = o.MaxSize.Value()
	}
	maxEntries = sandboxv1alpha1.DefaultOutputMaxEntries
	if o.MaxEntries != nil {
		maxEntries = *o.MaxEntries
	}
	return maxBytes, maxEntries
}

// outputIntents are spec.outputs as swiftletd receives them, and the bridge's
// kubeswift.outputs value (tag:path,...).
func outputIntents(sb *sandboxv1alpha1.SwiftSandbox) ([]runtimeintent.OutputIntent, string) {
	var outs []runtimeintent.OutputIntent
	var mounts []string
	for i, o := range sb.Spec.Outputs {
		maxBytes, maxEntries := outputLimits(o)
		tag := fmt.Sprintf("%s%d", outputTagPrefix, i)
		outs = append(outs, runtimeintent.OutputIntent{
			Name: o.Name, Tag: tag, Path: o.Path, MaxBytes: maxBytes, MaxEntries: maxEntries,
		})
		mounts = append(mounts, tag+":"+o.Path)
	}
	return outs, strings.Join(mounts, ",")
}

// OutputSpoolPath is a sandbox's spool directory on its node.
func OutputSpoolPath(sb *sandboxv1alpha1.SwiftSandbox) string {
	return OutputSpoolDir + "/" + string(sb.UID)
}

// addOutputVolumes gives the launcher its staging emptyDir (sized for the
// images, which are sparse) and its own spool directory, and asks the
// scheduler for the disk both can take.
func addOutputVolumes(pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) {
	if len(sb.Spec.Outputs) == 0 {
		return
	}
	var staging, spool int64
	for _, o := range sb.Spec.Outputs {
		maxBytes, maxEntries := outputLimits(o)
		staging += OutputImageSize(maxBytes)
		spool += maxBytes + maxEntries*2048
	}
	limit := resource.NewQuantity(staging, resource.BinarySI)
	pod.Spec.Volumes = append(pod.Spec.Volumes,
		corev1.Volume{Name: outputStagingVolume, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: limit}}},
		corev1.Volume{Name: outputSpoolVolume, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: OutputSpoolPath(sb), Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}},
	)
	c := &pod.Spec.Containers[0]
	c.VolumeMounts = append(c.VolumeMounts,
		corev1.VolumeMount{Name: outputStagingVolume, MountPath: outputStagingDir},
		corev1.VolumeMount{Name: outputSpoolVolume, MountPath: OutputSpoolDir},
	)
	if c.Resources.Requests == nil {
		c.Resources.Requests = corev1.ResourceList{}
	}
	c.Resources.Requests[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(staging+spool, resource.BinarySI)
}
