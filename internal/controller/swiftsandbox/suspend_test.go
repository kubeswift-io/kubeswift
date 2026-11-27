package swiftsandbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// suspendFixture: a running sandbox (spec.operatingMode as given) and its
// ready launcher pod on node n1, whose bridge has the given features.
func suspendFixture(t *testing.T, mode sandboxv1alpha1.SandboxOperatingMode, features string) (*SwiftSandboxReconciler, client.Client) {
	t.Helper()
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "default", UID: types.UID("u-1234"), Generation: 2},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image: "busybox", Memory: resource.MustParse("256Mi"), OperatingMode: mode,
		},
		Status: sandboxv1alpha1.SwiftSandboxStatus{
			Phase: sandboxv1alpha1.SwiftSandboxRunning, PodRef: "sb", NodeName: "n1",
			StartedAt: ptr.To(metav1.Now()),
			Network:   &sandboxv1alpha1.SandboxNetworkStatus{PrimaryIP: "192.168.99.10"},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "default", Annotations: map[string]string{
			bridgeFeaturesAnnotation: features, annHypervisorVersion: "v54.0",
		}},
		Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: launcherName, Image: "swiftletd:v1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: launcherName, Ready: true},
		}},
	}
	if err := controllerutil.SetControllerReference(sb, pod, scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sb, pod).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}).Build()
	return &SwiftSandboxReconciler{Client: c, APIReader: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(20), SystemNamespace: "kubeswift-system"}, c
}

func runOperatingMode(t *testing.T, r *SwiftSandboxReconciler, c client.Client) bool {
	t.Helper()
	sb := getSandbox(t, c, "sb")
	handled, _, err := r.reconcileOperatingMode(context.Background(), sb)
	if err != nil {
		t.Fatalf("reconcileOperatingMode: %v", err)
	}
	return handled
}

func getPod(t *testing.T, c client.Client) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err != nil {
		t.Fatal(err)
	}
	return &pod
}

func setPodAnnotations(t *testing.T, c client.Client, ann map[string]string) {
	t.Helper()
	pod := getPod(t, c)
	for k, v := range ann {
		pod.Annotations[k] = v
	}
	if err := c.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func suspendedCond(sb *sandboxv1alpha1.SwiftSandbox) *metav1.Condition {
	return apimeta.FindStatusCondition(sb.Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionSuspended)
}

// Running + operatingMode Suspended: the destination is recorded, with the
// cleanup finalizer, before the launcher is asked to write anything.
func TestSuspend_StartsAndRecordsFirst(t *testing.T) {
	r, c := suspendFixture(t, sandboxv1alpha1.SandboxOperatingSuspended, "files vmgenid resume-sync")
	if !runOperatingMode(t, r, c) {
		t.Fatal("not handled")
	}
	sb := getSandbox(t, c, "sb")
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxSuspending {
		t.Fatalf("phase %s", sb.Status.Phase)
	}
	ss := sb.Status.SuspendedState
	if ss == nil || ss.NodeName != "n1" || ss.Path != SuspendRoot+"/u-1234" || ss.LauncherImage != "swiftletd:v1" || ss.HypervisorVersion != "v54.0" {
		t.Fatalf("suspendedState %+v", ss)
	}
	if !controllerutil.ContainsFinalizer(sb, sandboxSuspendFinalizer) {
		t.Errorf("finalizers %v", sb.Finalizers)
	}
	pod := getPod(t, c)
	if pod.Annotations[annSnapshotAction] != "suspend" || pod.Annotations[annSnapshotActionID] != "u-1234-suspend-2" {
		t.Errorf("action annotations %v", pod.Annotations)
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(pod.Annotations[annSnapshotActionArgs]), &args)
	if args["destination_url"] != "file://"+SuspendRoot+"/u-1234/" {
		t.Errorf("args %v", args)
	}
}

// A kernel without resume-sync: refused once for this generation, the
// sandbox keeps running, and it is not tried again.
func TestSuspend_RefusedWithoutResumeSync(t *testing.T) {
	r, c := suspendFixture(t, sandboxv1alpha1.SandboxOperatingSuspended, "files vmgenid egress-vsock")
	runOperatingMode(t, r, c)
	sb := getSandbox(t, c, "sb")
	cond := suspendedCond(sb)
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxRunning || cond == nil || cond.Reason != reasonSuspendUnsupported || !strings.Contains(cond.Message, "resume-sync") {
		t.Fatalf("phase %s cond %+v", sb.Status.Phase, cond)
	}
	if _, ok := getPod(t, c).Annotations[annSnapshotAction]; ok {
		t.Error("action sent despite the refusal")
	}
	if runOperatingMode(t, r, c) {
		t.Error("a settled refusal was handled again")
	}
}

// The launcher reports the state written: the sandbox is Suspended with the
// reported size and address, and its launcher pod is deleted.
func TestSuspend_Done(t *testing.T) {
	r, c := suspendFixture(t, sandboxv1alpha1.SandboxOperatingSuspended, "resume-sync")
	runOperatingMode(t, r, c)
	setPodAnnotations(t, c, map[string]string{
		annSnapshotStatus: "ready", annSnapshotStatusID: "u-1234-suspend-2",
		annSnapshotStatusDetail: `{"version":1,"guestIp":"192.168.99.11","hypervisorVersion":"v54.0","takenAtUnix":1795738255,"sizeBytes":55574528}`,
	})
	runOperatingMode(t, r, c)
	sb := getSandbox(t, c, "sb")
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxSuspended {
		t.Fatalf("phase %s (%s)", sb.Status.Phase, sb.Status.Message)
	}
	ss := sb.Status.SuspendedState
	if ss.SizeBytes != 55574528 || ss.GuestIP != "192.168.99.11" || ss.TakenAt == nil || ss.TakenAt.Unix() != 1795738255 {
		t.Errorf("suspendedState %+v", ss)
	}
	if cond := suspendedCond(sb); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Suspended condition %+v", cond)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); !apierrors.IsNotFound(err) {
		t.Errorf("launcher pod still there (err %v)", err)
	}
	// Suspended, no launcher: nothing more to do, and no cold boot.
	if !runOperatingMode(t, r, c) {
		t.Error("a suspended sandbox fell through to the launch path")
	}
}

// The launcher failed the suspend (and resumed the microVM): Running again,
// SuspendFailed for this generation, nothing left recorded.
func TestSuspend_Failed(t *testing.T) {
	r, c := suspendFixture(t, sandboxv1alpha1.SandboxOperatingSuspended, "resume-sync")
	runOperatingMode(t, r, c)
	setPodAnnotations(t, c, map[string]string{
		annSnapshotStatus: "failed", annSnapshotStatusID: "u-1234-suspend-2", annSnapshotStatusDetail: "snapshot: disk full",
	})
	runOperatingMode(t, r, c)
	sb := getSandbox(t, c, "sb")
	cond := suspendedCond(sb)
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxRunning || cond == nil || cond.Reason != reasonSuspendFailed || !strings.Contains(cond.Message, "disk full") {
		t.Fatalf("phase %s cond %+v", sb.Status.Phase, cond)
	}
	if sb.Status.SuspendedState != nil || controllerutil.ContainsFinalizer(sb, sandboxSuspendFinalizer) {
		t.Errorf("left behind: state %+v finalizers %v", sb.Status.SuspendedState, sb.Finalizers)
	}
	if runOperatingMode(t, r, c) {
		t.Error("a failed suspend was retried in the same generation")
	}
}

// The launcher pod vanished before it reported: the microVM is gone.
func TestSuspend_LauncherLost(t *testing.T) {
	r, c := suspendFixture(t, sandboxv1alpha1.SandboxOperatingSuspended, "resume-sync")
	runOperatingMode(t, r, c)
	if err := c.Delete(context.Background(), getPod(t, c)); err != nil {
		t.Fatal(err)
	}
	runOperatingMode(t, r, c)
	if sb := getSandbox(t, c, "sb"); sb.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed {
		t.Fatalf("phase %s", sb.Status.Phase)
	}
}

// Created Suspended: held Pending, no launcher.
func TestSuspend_CreatedSuspendedNotStarted(t *testing.T) {
	sb := plainSandbox("busybox")
	sb.Spec.OperatingMode = sandboxv1alpha1.SandboxOperatingSuspended
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sb).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}).Build()
	r := &SwiftSandboxReconciler{Client: c, APIReader: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(5)}
	if !runOperatingMode(t, r, c) {
		t.Fatal("not handled")
	}
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxPending || got.Status.Message != notStartedMessage {
		t.Fatalf("phase %s message %q", got.Status.Phase, got.Status.Message)
	}
}

// Running and operatingMode Running: not ours.
func TestSuspend_RunningIsNotHandled(t *testing.T) {
	r, c := suspendFixture(t, sandboxv1alpha1.SandboxOperatingRunning, "resume-sync")
	if runOperatingMode(t, r, c) {
		t.Error("handled a running sandbox that should keep running")
	}
}

// The cleanup Job runs on the state's node, removes that sandbox's
// directory only, and is routed back to the sandbox.
func TestSuspend_CleanupJob(t *testing.T) {
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns1", UID: "u-1"},
		Status:     sandboxv1alpha1.SwiftSandboxStatus{SuspendedState: &sandboxv1alpha1.SandboxSuspendedState{NodeName: "n2"}},
	}
	job := buildSuspendCleanupJob(sb, "u-1-suspend-clean", "kubeswift-system", "materialize:v1")
	spec := job.Spec.Template.Spec
	if spec.NodeName != "n2" {
		t.Errorf("node %q", spec.NodeName)
	}
	args := strings.Join(spec.Containers[0].Args, " ")
	if args != "clean-suspend --root=/suspend --sandbox-uid=u-1" {
		t.Errorf("args %q", args)
	}
	if spec.Volumes[0].HostPath.Path != SuspendRoot {
		t.Errorf("hostPath %q", spec.Volumes[0].HostPath.Path)
	}
	reqs := exportObjectToSandbox(context.Background(), job)
	if len(reqs) != 1 || reqs[0].Namespace != "ns1" || reqs[0].Name != "sb" {
		t.Errorf("job maps to %v", reqs)
	}
}

// suspendedFixture: a Suspended sandbox (spec as given) whose state was taken
// an hour ago on node n1, which exists unless nodeGone.
func suspendedFixture(t *testing.T, mode sandboxv1alpha1.SandboxOperatingMode, nodeGone bool) (*SwiftSandboxReconciler, client.Client) {
	t.Helper()
	started := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	taken := metav1.NewTime(time.Now().Add(-time.Hour))
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "default", UID: "u-1234", Generation: 3,
			Finalizers: []string{sandboxSuspendFinalizer}},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{Image: "registry.example/busybox:1.37", OperatingMode: mode},
		Status: sandboxv1alpha1.SwiftSandboxStatus{
			Phase: sandboxv1alpha1.SwiftSandboxSuspended, StartedAt: &started,
			Rootfs: &sandboxv1alpha1.SandboxRootfsStatus{Digest: "sha256:" + strings.Repeat("a", 64)},
			SuspendedState: &sandboxv1alpha1.SandboxSuspendedState{
				NodeName: "n1", Path: SuspendRoot + "/u-1234", LauncherImage: "swiftletd:old", TakenAt: &taken,
			},
		},
	}
	objs := []client.Object{sb}
	if !nodeGone {
		objs = append(objs, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}})
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}).Build()
	return &SwiftSandboxReconciler{Client: c, APIReader: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(20)}, c
}

// Set back to Running: Resuming, and spec.timeout's clock skips the hour
// the sandbox was suspended.
func TestResume_Starts(t *testing.T) {
	r, c := suspendedFixture(t, sandboxv1alpha1.SandboxOperatingRunning, false)
	before := getSandbox(t, c, "sb").Status.StartedAt.Time
	runOperatingMode(t, r, c)
	sb := getSandbox(t, c, "sb")
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxResuming || !resuming(sb) {
		t.Fatalf("phase %s", sb.Status.Phase)
	}
	if moved := sb.Status.StartedAt.Sub(before); moved < 59*time.Minute || moved > 61*time.Minute {
		t.Errorf("StartedAt moved %s, want about 1h", moved)
	}
	if cond := suspendedCond(sb); cond == nil || cond.Reason != reasonResuming {
		t.Errorf("condition %+v", cond)
	}
	// Resuming is the launch path's: not handled here.
	if runOperatingMode(t, r, c) {
		t.Error("Resuming handled by reconcileOperatingMode")
	}
}

// The node holding the state is gone: SnapshotLost, Failed; the finalizer
// stays for the cleanup.
func TestResume_NodeGone(t *testing.T) {
	r, c := suspendedFixture(t, sandboxv1alpha1.SandboxOperatingRunning, true)
	runOperatingMode(t, r, c)
	sb := getSandbox(t, c, "sb")
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed {
		t.Fatalf("phase %s", sb.Status.Phase)
	}
	if cond := suspendedCond(sb); cond == nil || cond.Reason != reasonSnapshotLost {
		t.Errorf("condition %+v", cond)
	}
	if !controllerutil.ContainsFinalizer(sb, sandboxSuspendFinalizer) {
		t.Error("cleanup finalizer dropped")
	}
}

// The launcher that resumes: pinned to the node, by the recorded launcher
// image, restoring the recorded path, with the image by digest.
func TestResume_Launch(t *testing.T) {
	_, c := suspendedFixture(t, sandboxv1alpha1.SandboxOperatingRunning, false)
	sb := getSandbox(t, c, "sb")
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxResuming
	pinned, err := pinnedImage(sb)
	if err != nil || pinned != "registry.example/busybox@sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("pinned %q %v", pinned, err)
	}
	if got := podNodeSelector(sb)[corev1.LabelHostname]; got != "n1" {
		t.Errorf("node selector hostname %q", got)
	}
	intent := &runtimeintent.RuntimeIntent{}
	resumeLaunch(sb, intent)
	if intent.Restore == nil || intent.Restore.SnapshotPath != SuspendRoot+"/u-1234" || !intent.Restore.AutoResume || intent.Restore.MemoryRestoreMode != "copy" {
		t.Errorf("restore %+v", intent.Restore)
	}
	cur := swiftguest.LauncherImage()
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "network-init", Image: cur}, {Name: "materialize", Image: "m"}},
		Containers:     []corev1.Container{{Name: launcherName, Image: cur}, {Name: "console", Image: "m"}},
	}}
	resumeLauncherImage(sb, pod)
	if pod.Spec.InitContainers[0].Image != "swiftletd:old" || pod.Spec.Containers[0].Image != "swiftletd:old" ||
		pod.Spec.InitContainers[1].Image != "m" || pod.Spec.Containers[1].Image != "m" {
		t.Errorf("images %+v %+v", pod.Spec.InitContainers, pod.Spec.Containers)
	}
}

func TestResume_Drift(t *testing.T) {
	sb := &sandboxv1alpha1.SwiftSandbox{
		Spec: sandboxv1alpha1.SwiftSandboxSpec{Model: &sandboxv1alpha1.SandboxModel{ImageRef: "m:1"}},
		Status: sandboxv1alpha1.SwiftSandboxStatus{
			Model:     &sandboxv1alpha1.SandboxModelStatus{Digest: "sha256:m1"},
			Artifacts: []sandboxv1alpha1.SandboxArtifactStatus{{Name: "a", Digest: "sha256:a1"}},
		},
	}
	if d := resumeDrift(sb, "sha256:m1", []sandboxv1alpha1.SandboxArtifactStatus{{Name: "a", Digest: "sha256:a1"}}); d != "" {
		t.Errorf("unchanged content reported: %s", d)
	}
	if d := resumeDrift(sb, "sha256:m2", nil); !strings.Contains(d, "model") {
		t.Errorf("model drift: %q", d)
	}
	if d := resumeDrift(sb, "sha256:m1", []sandboxv1alpha1.SandboxArtifactStatus{{Name: "a", Digest: "sha256:a2"}}); !strings.Contains(d, "artifact a") {
		t.Errorf("artifact drift: %q", d)
	}
}

// A restored launcher's resume-sync report decides when the resume is done.
func TestResume_Settled(t *testing.T) {
	r := &SwiftSandboxReconciler{Recorder: record.NewFakeRecorder(10)}
	mk := func(ann string, condAge time.Duration) (*sandboxv1alpha1.SwiftSandbox, *corev1.Pod) {
		sb := &sandboxv1alpha1.SwiftSandbox{Status: sandboxv1alpha1.SwiftSandboxStatus{Conditions: []metav1.Condition{{
			Type: sandboxv1alpha1.SwiftSandboxConditionSuspended, Status: metav1.ConditionFalse, Reason: reasonResuming,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-condAge)),
		}}}}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}, Spec: corev1.PodSpec{NodeName: "n1"}}
		if ann != "" {
			pod.Annotations[annResumed] = ann
		}
		return sb, pod
	}
	sb, pod := mk("", time.Second)
	if r.resumeSettled(sb, pod) {
		t.Error("settled with no report yet")
	}
	sb, pod = mk(`{"ok":true,"synced":["clock","entropy"],"clockOffsetMs":-74392}`, time.Second)
	if !r.resumeSettled(sb, pod) {
		t.Fatal("not settled on an ok report")
	}
	if c := suspendedCond(sb); c.Reason != reasonResumed || !strings.Contains(c.Message, "-74392 ms") {
		t.Errorf("condition %+v", c)
	}
	sb, pod = mk(`{"ok":false,"error":"the guest agent did not answer"}`, time.Second)
	if !r.resumeSettled(sb, pod) || suspendedCond(sb).Reason != reasonResumeSyncFailed {
		t.Errorf("failed report: %+v", suspendedCond(sb))
	}
	sb, pod = mk("", resumeSyncWait+time.Second)
	if !r.resumeSettled(sb, pod) || suspendedCond(sb).Reason != reasonResumeSyncFailed {
		t.Errorf("no report in time: %+v", suspendedCond(sb))
	}
}

// Waiting for something (the old launcher to stop, the registry) during a
// resume keeps it Resuming: Pending dropped the resume and the next pass
// booted the sandbox cold (found on a dev cluster).
func TestResume_WaitKeepsResuming(t *testing.T) {
	r, c := suspendedFixture(t, sandboxv1alpha1.SandboxOperatingRunning, false)
	sb := getSandbox(t, c, "sb")
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxResuming
	if _, err := r.waitForReference(context.Background(), sb, "PreviousLauncherStopping", "waiting"); err != nil {
		t.Fatal(err)
	}
	if got := getSandbox(t, c, "sb"); got.Status.Phase != sandboxv1alpha1.SwiftSandboxResuming || !resuming(got) {
		t.Fatalf("phase %s after a wait", got.Status.Phase)
	}
	// A cold start still waits as Pending.
	cold := getSandbox(t, c, "sb")
	cold.Status.Phase = sandboxv1alpha1.SwiftSandboxMaterializing
	cold.Status.SuspendedState = nil
	if _, err := r.waitForReference(context.Background(), cold, "KernelNotReady", "waiting"); err != nil {
		t.Fatal(err)
	}
	if got := getSandbox(t, c, "sb"); got.Status.Phase != sandboxv1alpha1.SwiftSandboxPending {
		t.Fatalf("cold wait phase %s", got.Status.Phase)
	}
}

// finishResume keeps the status set in the same pass (the finalizer patch
// refreshes the object from the server).
func TestResume_FinishKeepsStatus(t *testing.T) {
	r, c := suspendedFixture(t, sandboxv1alpha1.SandboxOperatingRunning, false)
	sb := getSandbox(t, c, "sb")
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionSuspended, Status: metav1.ConditionFalse, Reason: reasonResumed, Message: "m",
	})
	if err := r.finishResume(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	if cond := suspendedCond(sb); cond == nil || cond.Reason != reasonResumed {
		t.Errorf("condition after finishResume %+v", cond)
	}
	if sb.Status.SuspendedState != nil || controllerutil.ContainsFinalizer(sb, sandboxSuspendFinalizer) {
		t.Errorf("state %+v finalizers %v", sb.Status.SuspendedState, sb.Finalizers)
	}
}

func TestResume_RestoreErrorIsReported(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		annResumed: `{"ok":false,"error":"restore: rootfs /x is not the file the guest was suspended with (rebuilt or replaced)"}`,
	}}}
	if got := restoreError(pod); !strings.Contains(got, "not the file") {
		t.Errorf("restoreError %q", got)
	}
	pod.Annotations[annResumed] = `{"ok":true}`
	if got := restoreError(pod); got != "" {
		t.Errorf("ok report gave %q", got)
	}
}
