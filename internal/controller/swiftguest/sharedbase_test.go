package swiftguest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
	"github.com/kubeswift-io/kubeswift/internal/sharedbase"
)

const (
	sbGuestUID = types.UID("11111111-2222-3333-4444-555555555555")
	sbImageUID = types.UID("aaaaaaaa-0000-0000-0000-000000000001")
	sbPVCUID   = types.UID("bbbbbbbb-0000-0000-0000-000000000002")
)

// A disk-boot guest on a sharedBaseDisk class, with UIDs, because the disk's
// device name and the base's key are both derived from them.
func sharedBaseFixtures() []client.Object {
	g := asDiskBoot(kernelGuest())
	g.UID = sbGuestUID
	cls := testGuestClass()
	cls.Spec.SharedBaseDisk = true
	img := readyImage()
	img.UID = sbImageUID
	pvc := preparedPVC()
	pvc.UID = sbPVCUID
	return []client.Object{g, cls, img, pvc}
}

func materialiseJobOf(t *testing.T, c client.Client) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: testGuestName + materialiseJobSuffix}, &job); err != nil {
		t.Fatalf("no materialise Job: %v", err)
	}
	return &job
}

// succeed marks the Job complete and gives it a pod on node, as the Job
// controller would.
func succeed(t *testing.T, c client.Client, job *batchv1.Job, node string) {
	t.Helper()
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job.Name + "-x", Namespace: "ns",
			Labels: map[string]string{"batch.kubernetes.io/job-name": job.Name},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

// launchers are the guest's launcher pods — not the materialise Job's pod.
func launchers(t *testing.T, c client.Client) []corev1.Pod {
	t.Helper()
	var out []corev1.Pod
	for _, p := range launcherPods(t, c) {
		if _, isJob := p.Labels["batch.kubernetes.io/job-name"]; !isJob {
			out = append(out, p)
		}
	}
	return out
}

// THE lifecycle, end to end through Reconcile.
func TestReconcile_SharedBaseGuest_Lifecycle(t *testing.T) {
	c := guestClientBuilder(append(sharedBaseFixtures(), node("worker-1"), node("worker-2"))...).
		WithStatusSubresource(&batchv1.Job{}).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	// 1. The disk is built before any launcher exists.
	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if n := len(launchers(t, c)); n != 0 {
		t.Fatalf("a launcher was created before the guest's disk existed (%d)", n)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseScheduling {
		t.Errorf("phase = %q, want Scheduling", got.Status.Phase)
	}
	if cond := guestCondition(t, got, ConditionStorageReady); cond.Status != metav1.ConditionFalse || cond.Reason != reasonBaseDiskPending {
		t.Errorf("StorageReady = %s/%s %q; want False/%s saying the disk is being built", cond.Status, cond.Reason, cond.Message, reasonBaseDiskPending)
	}
	job := materialiseJobOf(t, c)

	// 2. The Job succeeds on worker-1. The pin is persisted — and STILL no
	//    launcher, because the launcher is pinned from stored status.
	succeed(t, c, job, "worker-1")
	got, _, err = reconcileGuest(t, r)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	sb := got.Status.SharedBaseDisk
	if sb == nil || sb.Node != "worker-1" || !sb.Created {
		t.Fatalf("status.sharedBaseDisk = %+v, want node worker-1, created", sb)
	}
	if sb.BaseKey != sharedbase.BaseKey(sbImageUID, sbPVCUID) {
		t.Errorf("baseKey = %q, want the image and PVC UIDs", sb.BaseKey)
	}
	if n := len(launchers(t, c)); n != 0 {
		t.Fatalf("a launcher was created in the same pass that recorded the pin (%d); it could not have been pinned", n)
	}

	// 3. The launcher: pinned to the disk's node, booting from the thin device.
	if _, _, err := reconcileGuest(t, r); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	pods := launchers(t, c)
	if len(pods) != 1 {
		t.Fatalf("got %d launchers, want 1", len(pods))
	}
	pod := pods[0]
	if pod.Spec.NodeName != "worker-1" {
		t.Fatalf("launcher on %q; it must run on worker-1, where its disk is", pod.Spec.NodeName)
	}
	assertBootsFromTheThinDevice(t, &pod)

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: testGuestName + IntentConfigMapSuffix}, &cm); err != nil {
		t.Fatalf("intent ConfigMap: %v", err)
	}
	var intent runtimeintent.RuntimeIntent
	for _, v := range cm.Data {
		if err := json.Unmarshal([]byte(v), &intent); err == nil && intent.RootDisk.Path != "" {
			break
		}
	}
	if want := sharedbase.DevicePath(sbGuestUID); intent.RootDisk.Path != want {
		t.Errorf("intent root disk = %q, want %q", intent.RootDisk.Path, want)
	}
}

// assertBootsFromTheThinDevice: the root-disk volume is the host's
// device-mapper directory and NEVER the image PVC, and the reactivate init
// container runs first.
func assertBootsFromTheThinDevice(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	var rootDisk *corev1.Volume
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		if v.Name == "root-disk" {
			rootDisk = v
		}
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "img-prepared" {
			t.Errorf("the launcher mounts the IMAGE PVC (volume %q); a shared-base guest would boot from the wrong disk", v.Name)
		}
	}
	if rootDisk == nil || rootDisk.HostPath == nil || rootDisk.HostPath.Path != sharedbase.DeviceDir {
		t.Fatalf("root-disk volume = %+v, want hostPath %s", rootDisk, sharedbase.DeviceDir)
	}
	if len(pod.Spec.InitContainers) == 0 || pod.Spec.InitContainers[0].Name != "basedisk-reactivate" {
		t.Fatalf("the first init container is not basedisk-reactivate: %v", initNames(pod))
	}
	args := strings.Join(pod.Spec.InitContainers[0].Args, " ")
	for _, want := range []string{"--mode=reactivate", "--device=" + sharedbase.DeviceName(sbGuestUID)} {
		if !strings.Contains(args, want) {
			t.Errorf("reactivate args %q missing %q", args, want)
		}
	}
	if strings.Contains(args, "--image") {
		t.Error("the reactivate container is given the image; restarts must not depend on the SwiftImage")
	}
}

func initNames(pod *corev1.Pod) []string {
	var n []string
	for _, c := range pod.Spec.InitContainers {
		n = append(n, c.Name)
	}
	return n
}

// The Job's shape: everything it needs, and nothing that would outlast it.
func TestMaterialiseJob_Shape(t *testing.T) {
	c := guestClientBuilder(sharedBaseFixtures()...).WithStatusSubresource(&batchv1.Job{}).Build()
	if _, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}); err != nil {
		t.Fatal(err)
	}
	job := materialiseJobOf(t, c)
	spec := job.Spec.Template.Spec

	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("the Job retries by itself; its retries could land on any node — a retry must be a new Job on the recorded node")
	}
	// Labels on the Job, never on its pod template — launcher lookups select
	// pods by swift.kubeswift.io/guest.
	if _, ok := job.Spec.Template.Labels["swift.kubeswift.io/guest"]; ok {
		t.Error("the materialise pod carries swift.kubeswift.io/guest and would be mistaken for the launcher")
	}
	var imageRO bool
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "img-prepared" && v.PersistentVolumeClaim.ReadOnly {
			imageRO = true
		}
	}
	if !imageRO {
		t.Error("the image PVC is not mounted read-only")
	}
	ctr := spec.Containers[0]
	if ctr.SecurityContext == nil || ctr.SecurityContext.Privileged == nil || !*ctr.SecurityContext.Privileged {
		t.Error("the materialise container is not privileged; it cannot create loop or device-mapper devices")
	}
	if ctr.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Error("without FallbackToLogsOnError a failure's reason never reaches StorageReady")
	}
	args := strings.Join(ctr.Args, " ")
	for _, want := range []string{
		"--mode=create",
		"--base-key=" + sharedbase.BaseKey(sbImageUID, sbPVCUID),
		"--guest-key=" + sharedbase.GuestKey("ns", testGuestName, sbGuestUID),
		"--device=" + sharedbase.DeviceName(sbGuestUID),
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
}

// A failed materialisation says why, on StorageReady, and does not loop.
func TestReconcile_SharedBaseGuest_JobFailureIsSurfaced(t *testing.T) {
	c := guestClientBuilder(append(sharedBaseFixtures(), node("worker-1"))...).WithStatusSubresource(&batchv1.Job{}).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	if _, _, err := reconcileGuest(t, r); err != nil {
		t.Fatal(err)
	}
	job := materialiseJobOf(t, c)
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x", Namespace: "ns",
			Labels: map[string]string{"batch.kubernetes.io/job-name": job.Name}},
		Spec: corev1.PodSpec{NodeName: "worker-1"},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Message: "basedisk-materialize: creating or opening thin pool: no space left on device",
			}},
		}}},
	}); err != nil {
		t.Fatal(err)
	}

	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	cond := guestCondition(t, got, ConditionStorageReady)
	if cond.Reason != reasonBaseDiskFailed || !strings.Contains(cond.Message, "no space left on device") {
		t.Errorf("StorageReady = %s %q; want %s carrying the Job's own error", cond.Reason, cond.Message, reasonBaseDiskFailed)
	}
	if !strings.Contains(cond.Message, "delete the Job to retry") {
		t.Errorf("the message should say how to retry: %q", cond.Message)
	}
	if n := len(launchers(t, c)); n != 0 {
		t.Errorf("a launcher was created for a guest whose disk failed to build")
	}
}

// A clone resumes memory captured against its source's disk; a fresh disk built
// from the image would not match it. ensureSharedBaseDisk refuses, with the
// reason, and creates no Job.
//
// Called directly: through Reconcile, a clone whose snapshot is missing is held
// earlier, at clone preparation, for that — accurate — reason and never gets
// here. Either way no materialise Job is created for a clone.
func TestEnsureSharedBaseDisk_RefusesAClone(t *testing.T) {
	objs := sharedBaseFixtures()
	g := objs[0].(*swiftv1alpha1.SwiftGuest)
	g.Spec.CloneFromSnapshot = &swiftv1alpha1.CloneFromSnapshotSource{SnapshotRef: corev1.LocalObjectReference{Name: "snap"}}
	c := guestClientBuilder(objs...).WithStatusSubresource(&batchv1.Job{}).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	status := g.Status.DeepCopy()
	rg := &resolved.ResolvedGuest{SharedBaseDisk: true}
	rg.PreparedImage.PVCName = "img-prepared"
	ready, err := r.ensureSharedBaseDisk(context.Background(), g, rg, status)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("a clone was allowed onto a shared-base disk")
	}
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: testGuestName + materialiseJobSuffix}, &job); err == nil {
		t.Error("a materialise Job was created for a clone; it would build a disk that does not match the resumed memory")
	}
	if cond := findCondition(status, ConditionStorageReady); cond == nil || cond.Reason != reasonBaseDiskRefused {
		t.Errorf("StorageReady = %+v; want %s", cond, reasonBaseDiskRefused)
	}
}

// Every launcher builder goes through rootDiskVolume. Checked on the three that
// construct the volume, so none can mount the image PVC for a shared-base guest.
func TestRootDiskVolume_EveryBuilder(t *testing.T) {
	rg := &resolved.ResolvedGuest{SharedBaseDevicePath: sharedbase.DevicePath(sbGuestUID)}
	for name, v := range map[string]corev1.Volume{
		"shared-base": rootDiskVolume(rg, "img-prepared"),
	} {
		if v.HostPath == nil || v.HostPath.Path != sharedbase.DeviceDir || v.PersistentVolumeClaim != nil {
			t.Errorf("%s: root-disk = %+v, want hostPath %s and no PVC", name, v, sharedbase.DeviceDir)
		}
	}
	if v := rootDiskVolume(&resolved.ResolvedGuest{}, "clone-pvc"); v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "clone-pvc" {
		t.Errorf("a guest without a shared-base disk lost its PVC root disk: %+v", v)
	}
	// The mount follows.
	if m, _ := rootDiskMount(rg); m == nil || m.MountPath != sharedbase.DeviceDir {
		t.Errorf("root-disk mount = %+v, want %s", m, sharedbase.DeviceDir)
	}
}
