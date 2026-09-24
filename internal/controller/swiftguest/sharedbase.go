package swiftguest

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	"github.com/kubeswift-io/kubeswift/internal/sharedbase"
)

// A shared-base guest's root disk is a thin snapshot in a dm-thin pool on its
// node (docs/design/shared-base-root-disk.md). Two pods touch it:
//
//   - a per-guest materialise Job, once: mounts the SwiftImage's prepared PVC,
//     builds the base on this node if needed, snapshots the guest, exits.
//   - the launcher's init container, every start: re-maps the existing disk,
//     which only matters after a node reboot, and never needs the image.
//
// They are separate because the prepared PVC is ReadWriteOnce and stays
// attached for the life of the pod mounting it. In the launcher it would pin
// the image to one node for as long as the guest ran, blocking every other
// guest of that image from being created anywhere else.

const (
	// defaultPoolSize sizes a node's pool when it is first created. The file is
	// preallocated, not sparse (§7.9), so this is real disk that a node gives up
	// the first time it runs a shared-base guest.
	defaultPoolSize = 40 << 30
	// PoolSizeEnv overrides it, as a quantity ("60Gi"). Read on the controller
	// and passed to the node command. Changing it later does not resize an
	// existing pool: the node command uses the backing file's actual size once
	// it exists.
	PoolSizeEnv = "KUBESWIFT_BASEDISK_POOL_SIZE"

	materialiseJobSuffix = "-basedisk"
	materialiseImageDir  = "/image"
	basediskCommand      = "/usr/local/bin/basedisk-materialize"

	reasonBaseDiskPending        = "BaseDiskPending"
	reasonBaseDiskFailed         = "BaseDiskFailed"
	reasonBaseDiskRefused        = "BaseDiskUnsupported"
	reasonBaseDiskNodeIneligible = "BaseDiskNodeIneligible"
	reasonBaseDiskNoNode         = "BaseDiskNoEligibleNode"
)

// PoolSize is the size a node's pool is created at.
//
// The error is for the operator's value, and is why this is checked once at
// startup rather than swallowed here: a pool size that does not parse would
// otherwise become the default silently, and nobody would learn that the 200Gi
// they asked for is 40.
func PoolSize() (uint64, error) {
	v := os.Getenv(PoolSizeEnv)
	if v == "" {
		return defaultPoolSize, nil
	}
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a quantity (e.g. 40Gi): %w", PoolSizeEnv, v, err)
	}
	if q.Sign() <= 0 {
		return 0, fmt.Errorf("%s=%q must be greater than zero", PoolSizeEnv, v)
	}
	return uint64(q.Value()), nil
}

// poolSize is PoolSize for callers that cannot report an error. Startup
// validates the value, so a bad one never reaches here.
func poolSize() uint64 {
	n, err := PoolSize()
	if err != nil {
		return defaultPoolSize
	}
	return n
}

// MaterialiseJobName is the deterministic name of a guest's materialise Job.
func MaterialiseJobName(guest *swiftv1alpha1.SwiftGuest) string {
	return names.JobName(guest.Name, materialiseJobSuffix)
}

// ensureSharedBaseDisk drives a shared-base guest's root disk to existence and
// reports whether the launcher may start. Everything it learns goes on status —
// the node, whether the disk was created, and on StorageReady the reason it is
// not ready — so a guest waiting on its disk says why.
func (r *SwiftGuestReconciler) ensureSharedBaseDisk(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
	status *swiftv1alpha1.SwiftGuestStatus,
) (bool, error) {
	// A clone resumes memory that was captured against the SOURCE guest's
	// disk. Building a fresh disk from the image instead would hand the
	// resumed kernel a filesystem that does not match what it has in RAM —
	// the "EXT4-fs: bad geometry" failure EnsureRootDiskClone documents. The
	// clone paths build their own disk and know nothing of a thin pool, so
	// this is refused until they do.
	if guest.Spec.CloneFromSnapshot != nil || rg.RootDisk.FromOCI {
		SetStorageReadyCondition(status, false, reasonBaseDiskRefused,
			"cloneFromSnapshot cannot be used with a sharedBaseDisk class yet: a clone resumes memory "+
				"captured against its source's disk, and a fresh disk built from the image would not match "+
				"it. Use a class with sharedBaseDisk: false for cloned guests")
		return false, nil
	}

	// Before anything of this guest's can exist on a node: the finalizer is
	// what makes deleting the guest release its disk.
	if err := r.ensureSharedBaseFinalizer(ctx, guest); err != nil {
		return false, err
	}

	if sb := status.SharedBaseDisk; sb != nil && sb.Created {
		// Built before the size was recorded: take it from the class now, which
		// is the size it is being mapped at today, and stop reading the class.
		if sb.SizeBytes == 0 {
			sb.SizeBytes = sharedBaseGuestBytes(rg)
		}
		rg.SharedBaseDevicePath = sharedbase.DevicePath(guest.UID)
		SetStorageReadyCondition(status, true, "",
			fmt.Sprintf("shared-base root disk %s on node %s", sharedbase.DeviceName(guest.UID), sb.Node))
		return true, nil
	}

	// A pool is this node's own disk, preallocated, so a node holds one only
	// when it is labelled for it. Checked before the Job exists, because the
	// alternative is a Job sitting unschedulable with nothing saying why.
	if reason, msg, err := r.baseDiskNodeCheck(ctx, guest); err != nil {
		return false, err
	} else if reason != "" {
		SetStorageReadyCondition(status, false, reason, msg)
		return false, nil
	}

	baseKey, imagePVC, err := r.sharedBaseKey(ctx, guest, rg)
	if err != nil {
		SetStorageReadyCondition(status, false, reasonBaseDiskPending, err.Error())
		return false, nil
	}

	jobName := MaterialiseJobName(guest)
	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: jobName}, &job)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, r.materialiseJob(guest, rg, jobName, baseKey, imagePVC)); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		// The size the Job is building the disk at, recorded before it can
		// finish, so every later start maps the device at it.
		recordSharedBaseDisk(status, "", baseKey, sharedBaseDiskBytes(guest, rg))
		SetStorageReadyCondition(status, false, reasonBaseDiskPending,
			"building the shared-base root disk (materialise Job "+jobName+" created)")
		return false, nil
	}
	if err != nil {
		return false, err
	}

	pod, err := r.jobPod(ctx, guest.Namespace, jobName)
	if err != nil {
		return false, err
	}

	switch {
	case isJobComplete(&job):
		if pod == nil || pod.Spec.NodeName == "" {
			// A Job that succeeded but whose pod is gone (garbage collected)
			// cannot say which node holds the disk. Fall back to the node
			// recorded while it ran.
			if status.SharedBaseDisk == nil || status.SharedBaseDisk.Node == "" {
				SetStorageReadyCondition(status, false, reasonBaseDiskFailed,
					"materialise Job "+jobName+" succeeded, but its pod is gone and no node was recorded, so the "+
						"disk cannot be located; delete the Job to build it again")
				return false, nil
			}
		} else {
			recordSharedBaseDisk(status, pod.Spec.NodeName, baseKey, sharedBaseDiskBytes(guest, rg))
		}
		status.SharedBaseDisk.Created = true
		// NOT ready in this pass, deliberately. The launcher is pinned from the
		// STORED status — pinnedNode reads guest.Status — and the node was only
		// just recorded on this reconcile's copy. Starting the launcher now
		// would create it unpinned, free to land on a node that does not hold
		// its disk. Returning not-ready makes Reconcile persist the node first;
		// the next pass takes the Created branch above and the launcher is
		// pinned from the moment it exists.
		SetStorageReadyCondition(status, false, reasonBaseDiskPending,
			fmt.Sprintf("shared-base root disk built on node %s; starting the launcher there", status.SharedBaseDisk.Node))
		return false, nil

	case isJobFailed(&job):
		SetStorageReadyCondition(status, false, reasonBaseDiskFailed,
			fmt.Sprintf("materialise Job %s failed: %s. It is not retried automatically, because what "+
				"fails here — a full pool, an image larger than the pool — does not fix itself; "+
				"delete the Job to retry once the cause is addressed", jobName, terminationMessage(pod)))
		return false, nil
	}

	// Running. Record the node as soon as the Job is placed, so that a retry
	// builds on the same node rather than leaving a partial disk orphaned on
	// this one and starting again elsewhere. Only while the disk is not yet
	// created: once it is, the node never moves.
	if pod != nil && pod.Spec.NodeName != "" && (status.SharedBaseDisk == nil || !status.SharedBaseDisk.Created) {
		recordSharedBaseDisk(status, pod.Spec.NodeName, baseKey, sharedBaseDiskBytes(guest, rg))
	}
	msg := "building the shared-base root disk (materialise Job " + jobName + " running"
	if status.SharedBaseDisk != nil && status.SharedBaseDisk.Node != "" {
		msg += " on node " + status.SharedBaseDisk.Node
	}
	SetStorageReadyCondition(status, false, reasonBaseDiskPending, msg+")")
	return false, nil
}

// baseDiskNodeCheck reports why this guest's disk may not be built yet, as a
// condition reason and message, or "" when it may.
//
// A guest pinned to a node needs that node labelled; an unpinned guest needs at
// least one labelled node for the scheduler to choose from.
func (r *SwiftGuestReconciler) baseDiskNodeCheck(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (reason, message string, err error) {
	size := resource.NewQuantity(int64(poolSize()), resource.BinarySI)
	if node, source, perr := pinnedNode(guest); perr == nil && node != "" {
		var n corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
			if apierrors.IsNotFound(err) {
				return reasonBaseDiskNodeIneligible, fmt.Sprintf("node %q (%s) does not exist", node, source), nil
			}
			return "", "", err
		}
		if n.Labels[sharedbase.NodeLabel] != sharedbase.NodeLabelValue {
			return reasonBaseDiskNodeIneligible, fmt.Sprintf(
				"this guest is pinned to node %q (%s), which is not labelled %s=%s and so may not hold a "+
					"shared-base pool — %s of that node's disk, preallocated. Label the node, or unpin the "+
					"guest so it can be placed on a node that is",
				node, source, sharedbase.NodeLabel, sharedbase.NodeLabelValue, size), nil
		}
		return "", "", nil
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes, client.MatchingLabels{sharedbase.NodeLabel: sharedbase.NodeLabelValue}); err != nil {
		return "", "", err
	}
	if len(nodes.Items) == 0 {
		return reasonBaseDiskNoNode, fmt.Sprintf(
			"no node is labelled %s=%s, so there is nowhere to build this guest's disk. A pool is %s of a "+
				"node's own disk, preallocated, so a node takes one only when it is labelled for it: "+
				"kubectl label node <node> %s=%s",
			sharedbase.NodeLabel, sharedbase.NodeLabelValue, size, sharedbase.NodeLabel, sharedbase.NodeLabelValue), nil
	}
	return "", "", nil
}

// sharedBaseKey derives the content identity of the guest's image and the
// prepared PVC to build it from.
func (r *SwiftGuestReconciler) sharedBaseKey(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest) (string, string, error) {
	if guest.Spec.ImageRef == nil || rg.PreparedImage.PVCName == "" {
		return "", "", fmt.Errorf("the image is not prepared yet")
	}
	var img imagev1alpha1.SwiftImage
	if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: guest.Spec.ImageRef.Name}, &img); err != nil {
		return "", "", fmt.Errorf("SwiftImage %s: %w", guest.Spec.ImageRef.Name, err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: rg.PreparedImage.PVCName}, &pvc); err != nil {
		return "", "", fmt.Errorf("prepared image PVC %s: %w", rg.PreparedImage.PVCName, err)
	}
	if pvc.DeletionTimestamp != nil {
		// The recreated-SwiftImage case BaseKey exists for: an old PVC kept
		// Terminating by pvc-protection. Do not build from it.
		return "", "", fmt.Errorf("prepared image PVC %s is being deleted", pvc.Name)
	}
	return sharedbase.BaseKey(img.UID, pvc.UID), pvc.Name, nil
}

// jobPod returns a pod of the named Job, if one exists.
func (r *SwiftGuestReconciler) jobPod(ctx context.Context, namespace, jobName string) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace),
		client.MatchingLabels{"batch.kubernetes.io/job-name": jobName}); err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	return &pods.Items[0], nil
}

func terminationMessage(pod *corev1.Pod) string {
	if pod == nil {
		return "its pod is gone"
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && strings.TrimSpace(t.Message) != "" {
			return strings.TrimSpace(t.Message)
		}
	}
	return "no error was recorded; see the Job's pod logs"
}

// materialiseJob builds the per-guest Job that creates the disk.
func (r *SwiftGuestReconciler) materialiseJob(guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest, name, baseKey, imagePVC string) *batchv1.Job {
	args := append(nodeCommandArgs(guest, rg, "create"),
		"--base-key="+baseKey,
		"--image="+materialiseImageDir+"/"+runtimeintent.RootDiskImageFile,
	)
	spec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		// Both when the scheduler places this and when it is bound directly: a
		// pinned pod is still refused by the kubelet if the node does not match,
		// so a node that loses the label takes no new disks either way.
		NodeSelector:                 map[string]string{sharedbase.NodeLabel: sharedbase.NodeLabelValue},
		AutomountServiceAccountToken: ptr.To(false),
		ImagePullSecrets:             LauncherImagePullSecrets(),
		Containers: []corev1.Container{{
			Name:            "materialise",
			Image:           LauncherImage(),
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{basediskCommand},
			Args:            args,
			SecurityContext: privilegedContext(),
			// The command's own error becomes the pod's termination message,
			// which is what the controller puts on StorageReady.
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			VolumeMounts: append(nodeStateMounts(),
				corev1.VolumeMount{Name: "image", MountPath: materialiseImageDir, ReadOnly: true}),
		}},
		Volumes: append(nodeStateVolumes(), corev1.Volume{
			Name: "image",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: imagePVC, ReadOnly: true,
			}},
		}),
	}
	// The first attempt goes wherever the scheduler puts it, and that node is
	// then recorded. A later attempt goes back there (C3a's pin), so it reuses
	// or repairs what the first one left instead of starting a second disk on
	// another node.
	if node, _, err := pinnedNode(guest); err == nil && node != "" {
		spec.NodeName = node
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: guest.Namespace,
			// On the Job, deliberately NOT on its pod template: launcher lookups
			// select pods by swift.kubeswift.io/guest, and a materialise pod
			// carrying it would be mistaken for the guest's launcher.
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guest.Name,
				"swift.kubeswift.io/role":  "root-disk-materialise",
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, swiftGuestGVK)},
		},
		Spec: batchv1.JobSpec{
			// One attempt per Job. A retry is a new Job, placed on the recorded
			// node — a Job's own retries could land anywhere.
			BackoffLimit: ptr.To(int32(0)),
			Template:     corev1.PodTemplateSpec{Spec: spec},
		},
	}
}

// sharedBaseReactivateContainer re-maps the guest's existing disk before the
// launcher starts. After a node reboot the device is gone and this brings it
// back; otherwise it finds it mapped and returns. It never needs the image, so
// deleting the guest's SwiftImage does not stop it restarting.
func sharedBaseReactivateContainer(guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest) corev1.Container {
	return corev1.Container{
		Name:                     "basedisk-reactivate",
		Image:                    LauncherImage(),
		ImagePullPolicy:          corev1.PullIfNotPresent,
		Command:                  []string{basediskCommand},
		Args:                     nodeCommandArgs(guest, rg, "reactivate"),
		SecurityContext:          privilegedContext(),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		VolumeMounts:             nodeStateMounts(),
	}
}

// recordSharedBaseDisk updates what is known about the guest's disk without
// dropping what is already recorded. The size is written once: it is the size
// the device exists at, not a setting to be re-read.
func recordSharedBaseDisk(status *swiftv1alpha1.SwiftGuestStatus, node, baseKey string, sizeBytes int64) {
	if status.SharedBaseDisk == nil {
		status.SharedBaseDisk = &swiftv1alpha1.SharedBaseDiskStatus{}
	}
	sb := status.SharedBaseDisk
	if node != "" {
		sb.Node = node
	}
	if baseKey != "" {
		sb.BaseKey = baseKey
	}
	if sb.SizeBytes == 0 {
		sb.SizeBytes = sizeBytes
	}
}

// sharedBaseDiskBytes is the size to map the guest's disk at: the size it was
// built at, once that is known, and the class's size only before it exists.
//
// A thin device has no size of its own — one is chosen every time it is mapped.
// Reading the class each time made a class edit resize existing disks: lowering
// rootDisk.size brought every guest on that class back on a device shorter than
// its filesystem, at its next restart.
func sharedBaseDiskBytes(guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest) int64 {
	if sb := guest.Status.SharedBaseDisk; sb != nil && sb.SizeBytes > 0 {
		return sb.SizeBytes
	}
	return sharedBaseGuestBytes(rg)
}

func nodeCommandArgs(guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest, mode string) []string {
	return []string{
		"--mode=" + mode,
		"--root=" + sharedbase.StateRoot,
		"--pool=" + sharedbase.Pool,
		"--pool-data-bytes=" + strconv.FormatUint(poolSize(), 10),
		"--guest-key=" + sharedbase.GuestKey(guest.Namespace, guest.Name, guest.UID),
		"--device=" + sharedbase.DeviceName(guest.UID),
		"--guest-bytes=" + strconv.FormatInt(sharedBaseDiskBytes(guest, rg), 10),
	}
}

// sharedBaseGuestBytes is the size the guest's disk is presented at.
func sharedBaseGuestBytes(rg *resolved.ResolvedGuest) int64 {
	if !rg.RootDisk.Size.IsZero() {
		return rg.RootDisk.Size.Value()
	}
	return 40 << 30 // the same default the clone paths use
}

// The node command needs the host's /dev — to create loop devices and map the
// guest's disk — and the node state directory, which must outlive every pod:
// the registry in it is what maps a guest to its disk across restarts.
func nodeStateVolumes() []corev1.Volume {
	dir := corev1.HostPathDirectoryOrCreate
	return []corev1.Volume{
		{Name: "host-dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev"}}},
		{Name: "kubeswift-state", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: sharedbase.StateRoot, Type: &dir}}},
	}
}

func nodeStateMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "host-dev", MountPath: "/dev"},
		{Name: "kubeswift-state", MountPath: sharedbase.StateRoot},
	}
}

// rootDiskVolume is the launcher's "root-disk" volume: the guest's PVC, or —
// for a shared-base guest — the host's device-mapper directory, holding the
// thin device the intent points at.
//
// EVERY launcher builder goes through this: disk boot, GPU and restore. If any
// of them built the volume from the PVC name directly, a shared-base guest on
// that path would boot from the wrong disk — the image PVC — with nothing
// failing to say so.
func rootDiskVolume(rg *resolved.ResolvedGuest, claimName string) corev1.Volume {
	if rg != nil && rg.SharedBaseDevicePath != "" {
		dir := corev1.HostPathDirectory
		return corev1.Volume{Name: "root-disk", VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: sharedbase.DeviceDir, Type: &dir},
		}}
	}
	return corev1.Volume{Name: "root-disk", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
	}}
}

// withSharedBaseDisk adds what a shared-base guest's launcher needs beyond the
// root-disk volume: the reactivate init container, first, and the node-state
// volumes it mounts. A no-op for every other guest.
func withSharedBaseDisk(pod *corev1.Pod, guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest) {
	if rg == nil || rg.SharedBaseDevicePath == "" {
		return
	}
	pod.Spec.InitContainers = append([]corev1.Container{sharedBaseReactivateContainer(guest, rg)}, pod.Spec.InitContainers...)
	pod.Spec.Volumes = append(pod.Spec.Volumes, nodeStateVolumes()...)
}
