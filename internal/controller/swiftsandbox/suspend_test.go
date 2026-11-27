package swiftsandbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
