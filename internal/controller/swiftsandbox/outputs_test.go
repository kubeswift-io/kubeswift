package swiftsandbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/export"
)

const (
	testSystemNS = "kubeswift-system"
	testSBUID    = "0a1b2c3d-0000-4000-8000-000000000001"
)

func exportPolicy(name, uid, secret string) *sandboxv1alpha1.SwiftArtifactExportPolicy {
	return &sandboxv1alpha1.SwiftArtifactExportPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Spec: sandboxv1alpha1.SwiftArtifactExportPolicySpec{
			NamespaceSelector:    metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "default"}},
			Repositories:         []string{"registry.example.com/outputs/{namespace}/*"},
			DefaultRepository:    "registry.example.com/outputs/{namespace}",
			CredentialsSecretRef: sandboxv1alpha1.ExportCredentialsReference{Name: secret},
			MaxSize:              ptr.To(resource.MustParse("100Mi")),
		},
	}
}

func exportSecret(name string, labelled bool) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testSystemNS},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
	}
	if labelled {
		s.Labels = map[string]string{sandboxv1alpha1.ExportCredentialsLabel: "true"}
	}
	return s
}

func defaultNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"kubernetes.io/metadata.name": "default"}}}
}

func outputSandbox(image string) *sandboxv1alpha1.SwiftSandbox {
	sb := plainSandbox(image)
	sb.UID = testSBUID
	sb.Spec.Outputs = []sandboxv1alpha1.SandboxOutput{{Name: "cache", Path: "/output"}}
	return sb
}

func exportReconciler(objs ...client.Object) (*SwiftSandboxReconciler, client.Client) {
	r, c := sandboxReconciler(objs...)
	r.SystemNamespace = testSystemNS
	return r, c
}

func guestRunningReason(t *testing.T, c client.Client) string {
	t.Helper()
	cond := apimeta.FindStatusCondition(getSandbox(t, c, "sb").Status.Conditions, sandboxv1alpha1.SwiftSandboxConditionGuestRunning)
	if cond == nil {
		return ""
	}
	return cond.Reason
}

// Outputs are refused, before anything is created, cold or pooled, when the
// destination cannot be resolved to one allowed, credentialed repository.
func TestReconcile_OutputsRefused(t *testing.T) {
	over := outputSandbox("busybox:1")
	over.Spec.Outputs[0].MaxSize = ptr.To(resource.MustParse("1Gi"))
	elsewhere := outputSandbox("busybox:1")
	elsewhere.Spec.Outputs[0].Repository = "registry.example.com/outputs/other-team/cache"
	for _, tc := range []struct {
		name   string
		sb     *sandboxv1alpha1.SwiftSandbox
		noNS   bool
		objs   []client.Object
		reason string
	}{
		{"no system namespace", outputSandbox("busybox:1"), true, nil, reasonOutputExportUnavailable},
		{"no policy", outputSandbox("busybox:1"), false, nil, sandboxv1alpha1.OutputReasonDestinationDenied},
		{"repository outside the policy", elsewhere, false,
			[]client.Object{exportPolicy("p", "uid-p", "push"), exportSecret("push", true)}, sandboxv1alpha1.OutputReasonDestinationDenied},
		{"credential missing", outputSandbox("busybox:1"), false,
			[]client.Object{exportPolicy("p", "uid-p", "push")}, reasonOutputCredentialsMissing},
		{"credential not set aside for exports", outputSandbox("busybox:1"), false,
			[]client.Object{exportPolicy("p", "uid-p", "push"), exportSecret("push", false)}, reasonOutputCredentialsMissing},
		{"limit over the policy", over, false,
			[]client.Object{exportPolicy("p", "uid-p", "push"), exportSecret("push", true)}, reasonOutputLimitExceedsPolicy},
		{"two credentials for one repository", outputSandbox("busybox:1"), false,
			[]client.Object{exportPolicy("p", "uid-p", "push"), exportPolicy("q", "uid-q", "push2"), exportSecret("push", true), exportSecret("push2", true)},
			sandboxv1alpha1.OutputReasonDestinationAmbiguous},
	} {
		for _, pooled := range []bool{false, true} {
			sb := tc.sb.DeepCopy()
			if pooled {
				sb.Spec.PoolRef = &corev1.LocalObjectReference{Name: "pool"}
			}
			objs := append([]client.Object{sb, defaultNamespace(), readyKernel("default", defaultKernelProfile)}, tc.objs...)
			r, c := exportReconciler(objs...)
			if tc.noNS {
				r.SystemNamespace = ""
			}
			reconcileSB(t, r, "sb")
			got := getSandbox(t, c, "sb")
			if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || guestRunningReason(t, c) != tc.reason {
				t.Errorf("%s (pooled=%v): phase %s reason %q, want Failed/%s", tc.name, pooled, got.Status.Phase, guestRunningReason(t, c), tc.reason)
			}
			if len(got.Status.Outputs) != 1 || got.Status.Outputs[0].State != sandboxv1alpha1.SandboxOutputFailed || got.Status.Outputs[0].Reason != tc.reason {
				t.Errorf("%s (pooled=%v): status.outputs %+v, want the output Failed/%s", tc.name, pooled, got.Status.Outputs, tc.reason)
			}
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
				t.Errorf("%s (pooled=%v): a launcher was created (err %v)", tc.name, pooled, err)
			}
		}
	}
}

// Invalid outputs are reported as such.
func TestReconcile_InvalidOutputs(t *testing.T) {
	sb := plainSandbox("busybox:1")
	sb.Spec.Outputs = []sandboxv1alpha1.SandboxOutput{{Name: "out", Path: "/proc/x"}}
	r, c := exportReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	if got := guestRunningReason(t, c); got != "InvalidOutputs" {
		t.Errorf("reason %q, want InvalidOutputs", got)
	}
	if o := getSandbox(t, c, "sb").Status.Outputs; len(o) != 1 || o[0].Reason != "InvalidOutputs" {
		t.Errorf("status.outputs %+v", o)
	}
}

// Admission records each destination and its policy and holds the sandbox;
// the launcher is then built with the policy's ceilings.
func TestReconcile_OutputsAdmittedThenLaunched(t *testing.T) {
	sb := outputSandbox(testImage(t))
	r, c := exportReconciler(sb, defaultNamespace(), readyKernel("default", defaultKernelProfile),
		exportPolicy("p", "uid-p", "push"), exportSecret("push", true))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if len(got.Status.Outputs) != 1 {
		t.Fatalf("status.outputs %+v", got.Status.Outputs)
	}
	o := got.Status.Outputs[0]
	if o.State != sandboxv1alpha1.SandboxOutputPending || o.Repository != "registry.example.com/outputs/default/cache" ||
		o.Policy == nil || o.Policy.UID != "uid-p" {
		t.Errorf("admitted %+v", o)
	}
	if !controllerutilContains(got.Finalizers, sandboxOutputsFinalizer) {
		t.Errorf("finalizers %v", got.Finalizers)
	}
	reconcileSB(t, r, "sb")
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: intentConfigMapName(sb)}, &cm); err != nil {
		t.Fatal(err)
	}
	var intent runtimeintent.RuntimeIntent
	if err := json.Unmarshal([]byte(cm.Data[swiftguest.IntentFile]), &intent); err != nil {
		t.Fatal(err)
	}
	if len(intent.Outputs) != 1 || intent.Outputs[0].MaxBytes != 100<<20 {
		t.Errorf("intent outputs %+v, want the policy's 100Mi", intent.Outputs)
	}
}

func controllerutilContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// exportingSandbox is a sandbox whose workload ran on node n1, admitted
// against policy p.
func exportingSandbox() *sandboxv1alpha1.SwiftSandbox {
	sb := outputSandbox("busybox:1")
	sb.Finalizers = []string{sandboxTerminationFinalizer, sandboxOutputsFinalizer}
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxRunning
	sb.Status.NodeName = "n1"
	sb.Status.PodRef = "sb"
	sb.Status.StartedAt = &metav1.Time{Time: time.Now().Add(-time.Minute)}
	sb.Status.Outputs = []sandboxv1alpha1.SandboxOutputStatus{{
		Name: "cache", State: sandboxv1alpha1.SandboxOutputPending, Repository: "registry.example.com/outputs/default/cache",
		Policy: &sandboxv1alpha1.ExportPolicyReference{Name: "p", UID: "uid-p"},
	}}
	return sb
}

func endedLauncher(sb *sandboxv1alpha1.SwiftSandbox, exitCode string, report string) *corev1.Pod {
	ann := map[string]string{annSandboxExitCode: exitCode}
	if report != "" {
		ann[annSandboxOutputs] = report
	}
	pod := runningLauncher(sb, ann)
	pod.Status.Phase = corev1.PodSucceeded
	return pod
}

const packagedReport = `[{"name":"cache","state":"Packaged","contentDigest":"sha256:aa","contentBytes":12,"entries":3}]`

func exportFixture(t *testing.T, sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod, extra ...client.Object) (*SwiftSandboxReconciler, client.Client) {
	t.Helper()
	objs := []client.Object{sb, pod, defaultNamespace(), readyKernel("default", defaultKernelProfile),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		exportPolicy("p", "uid-p", "push"), exportSecret("push", true)}
	return exportReconciler(append(objs, extra...)...)
}

func exportJob(t *testing.T, c client.Client) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testSystemNS, Name: exportJobName(&sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{UID: testSBUID}}, "cache")}, &job); err != nil {
		t.Fatalf("export Job: %v", err)
	}
	return &job
}

// finishJob marks the Job finished with a report from a pod it controls.
func finishJob(t *testing.T, c client.Client, job *batchv1.Job, cond batchv1.JobConditionType, condReason string, rep *export.Result) {
	t.Helper()
	ctx := context.Background()
	job.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, Reason: condReason}}
	if err := c.Status().Update(ctx, job); err != nil {
		if err := c.Update(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	if rep == nil {
		return
	}
	b, _ := json.Marshal(rep)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-x", Namespace: job.Namespace, Labels: map[string]string{"job-name": job.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "export",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: string(b), FinishedAt: metav1.Now()}}}}},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

func finishCleanup(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	var job batchv1.Job
	key := client.ObjectKey{Namespace: testSystemNS, Name: cleanupJobName(&sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{UID: testSBUID}})}
	if err := c.Get(ctx, key, &job); err != nil {
		t.Fatalf("cleanup Job: %v", err)
	}
	if args := job.Spec.Template.Spec.Containers[0].Args; args[2] != "--sandbox-uid="+testSBUID {
		t.Errorf("cleanup args %v", args)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished <= 0 {
		t.Errorf("cleanup Job has no TTL: a finished one would be left behind (#838)")
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, &job); err != nil {
		if err := c.Update(ctx, &job); err != nil {
			t.Fatal(err)
		}
	}
}

// The whole path: packaged, one export Job built only from the controller's
// own data, the Job's report recorded, the spool cleaned, then Completed.
func TestReconcile_ExportRoundTrip(t *testing.T) {
	sb := exportingSandbox()
	r, c := exportFixture(t, sb, endedLauncher(sb, "0", packagedReport))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxExporting || got.Status.Outputs[0].State != sandboxv1alpha1.SandboxOutputPackaged ||
		got.Status.Outputs[0].ContentDigest != "sha256:aa" {
		t.Fatalf("after packaging: phase %s outputs %+v", got.Status.Phase, got.Status.Outputs)
	}

	reconcileSB(t, r, "sb")
	if s := getSandbox(t, c, "sb").Status.Outputs[0].State; s != sandboxv1alpha1.SandboxOutputUploading {
		t.Fatalf("state %s, want Uploading", s)
	}
	job := exportJob(t, c)
	ps := job.Spec.Template.Spec
	if ps.NodeName != "n1" || ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken {
		t.Errorf("job pod placement/token: %+v", ps)
	}
	if *job.Spec.ActiveDeadlineSeconds != int64(sandboxv1alpha1.DefaultOutputUploadTimeout.Seconds()) {
		t.Errorf("deadline %d", *job.Spec.ActiveDeadlineSeconds)
	}
	var spool, auth bool
	for _, v := range ps.Volumes {
		if v.HostPath != nil {
			spool = v.HostPath.Path == OutputSpoolDir+"/"+testSBUID
		}
		if v.Secret != nil {
			auth = v.Secret.SecretName == "push"
		}
	}
	if !spool || !auth {
		t.Errorf("volumes %+v", ps.Volumes)
	}
	sc := ps.Containers[0].SecurityContext
	if sc == nil || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) != 1 {
		t.Errorf("security context %+v", sc)
	}
	var spec export.Spec
	if err := json.Unmarshal([]byte(ps.Containers[0].Env[0].Value), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Repository != "registry.example.com/outputs/default/cache" || spec.Tag != testSBUID+".cache" ||
		spec.Archive != exportSpoolDir+"/cache.tar" || spec.MaxBytes != 100<<20 ||
		spec.Annotations["io.kubeswift.export.namespace"] != "default" || spec.ArtifactType != sandboxv1alpha1.DefaultOutputArtifactType {
		t.Errorf("export spec %+v", spec)
	}

	finishJob(t, c, job, batchv1.JobComplete, "", &export.Result{
		Reference: "registry.example.com/outputs/default/cache@sha256:bb", Digest: "sha256:bb", Size: 99,
		ContentDigest: "sha256:aa", ContentBytes: 12, Entries: 3})
	reconcileSB(t, r, "sb")
	got = getSandbox(t, c, "sb")
	o := got.Status.Outputs[0]
	if o.State != sandboxv1alpha1.SandboxOutputExported || o.Digest != "sha256:bb" || o.ExportedAt == nil {
		t.Fatalf("after the Job: %+v", o)
	}
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxExporting {
		t.Errorf("phase %s before the spool is cleaned", got.Status.Phase)
	}
	finishCleanup(t, c)
	reconcileSB(t, r, "sb")
	got = getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxCompleted || controllerutilContains(got.Finalizers, sandboxOutputsFinalizer) {
		t.Errorf("phase %s finalizers %v, want Completed and released", got.Status.Phase, got.Finalizers)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("export Job kept: %v", err)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, conditionOutputsExported); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("OutputsExported %+v", c)
	}
}

// A Job's failure reaches status with its reason, and the sandbox fails.
func TestReconcile_ExportFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cond       batchv1.JobConditionType
		condReason string
		rep        *export.Result
		reason     string
	}{
		{"credential refused", batchv1.JobComplete, "", &export.Result{Reason: export.ReasonUnauthorized, Message: "401"}, export.ReasonUnauthorized},
		{"archive refused", batchv1.JobComplete, "", &export.Result{Reason: export.ReasonInvalidEntry, Message: "symlink"}, export.ReasonInvalidEntry},
		{"registry unreachable", batchv1.JobFailed, "BackoffLimitExceeded", &export.Result{Message: "dial tcp: connection refused"}, reasonOutputRegistryUnavailable},
		{"upload timeout", batchv1.JobFailed, "DeadlineExceeded", nil, reasonOutputUploadTimeout},
		{"upload timeout after errors", batchv1.JobFailed, "DeadlineExceeded", &export.Result{Message: "no such host"}, reasonOutputUploadTimeout},
	} {
		sb := exportingSandbox()
		r, c := exportFixture(t, sb, endedLauncher(sb, "0", packagedReport))
		reconcileSB(t, r, "sb")
		reconcileSB(t, r, "sb")
		finishJob(t, c, exportJob(t, c), tc.cond, tc.condReason, tc.rep)
		reconcileSB(t, r, "sb")
		finishCleanup(t, c)
		reconcileSB(t, r, "sb")
		got := getSandbox(t, c, "sb")
		o := got.Status.Outputs[0]
		if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || o.State != sandboxv1alpha1.SandboxOutputFailed || o.Reason != tc.reason {
			t.Errorf("%s: phase %s output %+v, want Failed/%s", tc.name, got.Status.Phase, o, tc.reason)
		}
		if tc.rep != nil && tc.rep.Message != "" && !strings.Contains(o.Message, tc.rep.Message) {
			t.Errorf("%s: message %q does not carry the Job's error %q", tc.name, o.Message, tc.rep.Message)
		}
	}
}

// A policy replaced after admission never redirects an export: it ends it.
func TestReconcile_ExportPolicyChangedBeforePush(t *testing.T) {
	sb := exportingSandbox()
	pod := endedLauncher(sb, "0", packagedReport)
	objs := []client.Object{sb, pod, defaultNamespace(), readyKernel("default", defaultKernelProfile),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
		exportPolicy("p", "uid-p-recreated", "push"), exportSecret("push", true)}
	r, c := exportReconciler(objs...)
	reconcileSB(t, r, "sb")
	reconcileSB(t, r, "sb")
	o := getSandbox(t, c, "sb").Status.Outputs[0]
	if o.State != sandboxv1alpha1.SandboxOutputFailed || o.Reason != reasonOutputDestinationRevoked {
		t.Errorf("output %+v, want Failed/%s", o, reasonOutputDestinationRevoked)
	}
	var jobs batchv1.JobList
	_ = c.List(context.Background(), &jobs, client.InNamespace(testSystemNS), client.MatchingLabels{exportComponentLabel: exportComponentJob})
	if len(jobs.Items) != 0 {
		t.Errorf("an export Job was created: %v", jobs.Items[0].Name)
	}
}

// Nothing is exported from a workload that did not exit 0, or whose exit
// was not reported.
func TestReconcile_OutputsNotExported(t *testing.T) {
	sb := exportingSandbox()
	r, c := exportFixture(t, sb, endedLauncher(sb, "3", ""))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || got.Status.Outputs[0].State != sandboxv1alpha1.SandboxOutputSkipped ||
		got.Status.Outputs[0].Reason != sandboxv1alpha1.OutputReasonWorkloadFailed {
		t.Errorf("exit 3: phase %s outputs %+v", got.Status.Phase, got.Status.Outputs)
	}

	sb = exportingSandbox()
	pod := endedLauncher(sb, "", "")
	delete(pod.Annotations, annSandboxExitCode)
	r, c = exportFixture(t, sb, pod)
	reconcileSB(t, r, "sb")
	got = getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || got.Status.Outputs[0].State != sandboxv1alpha1.SandboxOutputSkipped {
		t.Errorf("no exit line: phase %s outputs %+v", got.Status.Phase, got.Status.Outputs)
	}

	sb = exportingSandbox()
	r, c = exportFixture(t, sb, endedLauncher(sb, "0", ""))
	reconcileSB(t, r, "sb")
	got = getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || got.Status.Outputs[0].State != sandboxv1alpha1.SandboxOutputFailed {
		t.Errorf("no report: phase %s outputs %+v", got.Status.Phase, got.Status.Outputs)
	}
	var jobs batchv1.JobList
	_ = c.List(context.Background(), &jobs, client.InNamespace(testSystemNS), client.MatchingLabels{exportComponentLabel: exportComponentJob})
	if len(jobs.Items) != 0 {
		t.Errorf("an export Job was created")
	}
}

// Packaging in progress holds the sandbox Exporting, past spec.timeout.
func TestReconcile_CollectingIgnoresTimeout(t *testing.T) {
	sb := exportingSandbox()
	sb.Spec.Timeout = &metav1.Duration{Duration: time.Second}
	pod := runningLauncher(sb, map[string]string{annSandboxExitCode: "0"})
	r, c := exportFixture(t, sb, pod)
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxExporting || got.Status.Outputs[0].State != sandboxv1alpha1.SandboxOutputCollecting {
		t.Errorf("phase %s outputs %+v, want Exporting/Collecting", got.Status.Phase, got.Status.Outputs)
	}
}

// Deleting a sandbox mid-export removes its Jobs and spool, then releases it.
func TestReconcile_DeletedDuringExport(t *testing.T) {
	sb := exportingSandbox()
	r, c := exportFixture(t, sb, endedLauncher(sb, "0", packagedReport))
	reconcileSB(t, r, "sb")
	reconcileSB(t, r, "sb")
	job := exportJob(t, c)
	ctx := context.Background()
	if err := c.Delete(ctx, getSandbox(t, c, "sb")); err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err == nil {
		_ = c.Delete(ctx, &pod)
	}
	cleanup := client.ObjectKey{Namespace: testSystemNS, Name: cleanupJobName(sb)}
	for i := 0; i < 10; i++ {
		if _, err := r.Reconcile(ctx, ctrlRequest("sb")); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, cleanup, &batchv1.Job{}); err == nil {
			break
		}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err == nil {
			_ = c.Delete(ctx, &pod)
		}
	}
	finishCleanup(t, c)
	if _, err := r.Reconcile(ctx, ctrlRequest("sb")); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("export Job kept after deletion: %v", err)
	}
	var left sandboxv1alpha1.SwiftSandbox
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb"}, &left); !apierrors.IsNotFound(err) {
		t.Errorf("sandbox still held: finalizers %v", left.Finalizers)
	}
}

// Export Jobs past the cap wait; only the controller's own labels count.
func TestReconcile_ExportConcurrencyCap(t *testing.T) {
	sb := exportingSandbox()
	busy := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: testSystemNS,
		Labels: map[string]string{exportComponentLabel: exportComponentJob}}}
	r, c := exportFixture(t, sb, endedLauncher(sb, "0", packagedReport), busy)
	r.MaxConcurrentExports = 1
	reconcileSB(t, r, "sb")
	reconcileSB(t, r, "sb")
	o := getSandbox(t, c, "sb").Status.Outputs[0]
	if o.State != sandboxv1alpha1.SandboxOutputPackaged || !strings.Contains(o.Message, "waiting") {
		t.Errorf("output %+v, want Packaged and waiting", o)
	}
}

func ctrlRequest(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "default", Name: name}}
}

// The node, and the packaged output with it, is gone: reported, not retried.
func TestReconcile_ExportNodeGone(t *testing.T) {
	sb := exportingSandbox()
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxExporting
	sb.Status.Outputs[0].State = sandboxv1alpha1.SandboxOutputPackaged
	pod := endedLauncher(sb, "0", packagedReport)
	r, c := exportReconciler(sb, pod, defaultNamespace(), exportPolicy("p", "uid-p", "push"), exportSecret("push", true))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || got.Status.Outputs[0].Reason != reasonOutputLost {
		t.Errorf("phase %s outputs %+v, want Failed/%s", got.Status.Phase, got.Status.Outputs, reasonOutputLost)
	}
}

// Only the controller's export objects map back, by their annotation.
func TestExportObjectToSandbox(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Labels:      map[string]string{exportComponentLabel: exportComponentJob},
		Annotations: map[string]string{exportForAnnotation: "team-a/sb"}}}
	if reqs := exportObjectToSandbox(context.Background(), job); len(reqs) != 1 || reqs[0].Namespace != "team-a" || reqs[0].Name != "sb" {
		t.Errorf("requests %v", reqs)
	}
	job.Labels = nil
	if reqs := exportObjectToSandbox(context.Background(), job); len(reqs) != 0 {
		t.Errorf("an unlabelled object mapped: %v", reqs)
	}
}

// A reconcile from a stale copy of a cleaned sandbox (#838): the cleanup it
// starts is a Job that removes itself; a v0.19.0 bare cleanup pod is removed.
func TestReconcile_StaleCleanupLeavesNothing(t *testing.T) {
	sb := exportingSandbox()
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxCompleted
	sb.Status.Outputs[0].State = sandboxv1alpha1.SandboxOutputExported
	launcher := endedLauncher(sb, "0", packagedReport)
	legacy := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: cleanupJobName(sb), Namespace: testSystemNS},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	r, c := exportFixture(t, sb, launcher, legacy)
	reconcileSB(t, r, "sb")
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(legacy), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("legacy cleanup pod kept: %v", err)
	}
	finishCleanup(t, c)
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if controllerutilContains(got.Finalizers, sandboxOutputsFinalizer) {
		t.Errorf("finalizer kept after cleanup: %v", got.Finalizers)
	}
	// The finished Job is not deleted by the controller: its TTL does it, so a
	// later stale reconcile finds it instead of starting an orphan.
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: testSystemNS, Name: cleanupJobName(sb)}, &job); err != nil {
		t.Errorf("cleanup Job: %v", err)
	}
}
