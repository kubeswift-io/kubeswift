package swiftsandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
	"github.com/kubeswift-io/kubeswift/internal/names"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/export"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/exportpolicy"
)

// spec.outputs export: destinations resolved against SwiftArtifactExportPolicy,
// the Exporting phase, export Jobs and spool cleanup. The push credential is
// only ever mounted by the export Job, in the KubeSwift namespace.

const (
	// sandboxOutputsFinalizer holds a sandbox with outputs until its export
	// Jobs are gone and its spool directory is cleaned.
	sandboxOutputsFinalizer = "kubeswift.io/sandbox-outputs"
	// annSandboxOutputs is swiftletd's collection report (outputs.rs). A hint:
	// the export Job verifies the package itself.
	annSandboxOutputs = "kubeswift.io/sandbox-outputs"

	// Labels and annotations on export Jobs and cleanup pods, which live in
	// the KubeSwift namespace and so cannot be owned by the sandbox.
	exportComponentLabel  = "app.kubernetes.io/component"
	exportComponentJob    = "sandbox-export"
	exportComponentClean  = "sandbox-output-cleanup"
	exportSandboxUIDLabel = "sandbox.kubeswift.io/export-sandbox-uid"
	exportOutputLabel     = "sandbox.kubeswift.io/export-output"
	exportForAnnotation   = "sandbox.kubeswift.io/export-for"

	conditionOutputsExported = "OutputsExported"

	// outputCollectionTimeout bounds the launcher's packaging once the
	// workload has exited 0.
	outputCollectionTimeout = 30 * time.Minute
	// outputCleanupTimeout bounds waiting for the spool cleanup.
	outputCleanupTimeout = 2 * time.Minute
	exportPollInterval   = 5 * time.Second
	// DefaultMaxConcurrentExports is the controller-wide cap on export Jobs.
	DefaultMaxConcurrentExports = 4

	exportAuthDir  = "/export-auth"
	exportSpoolDir = "/spool"
)

// Reasons beyond those in the API package.
const (
	reasonOutputLimitExceedsPolicy  = "OutputLimitExceedsPolicy"
	reasonOutputCredentialsMissing  = "OutputCredentialsMissing"
	reasonOutputExportUnavailable   = "OutputExportUnavailable"
	reasonOutputDestinationRevoked  = "OutputDestinationRevoked"
	reasonOutputCollectionFailed    = "OutputCollectionFailed"
	reasonOutputLost                = "OutputLost"
	reasonOutputUploadTimeout       = "UploadTimeout"
	reasonOutputRegistryUnavailable = "RegistryUnavailable"
)

// destination is where one output goes and under which limits.
type destination struct {
	repository    string
	policy        *sandboxv1alpha1.SwiftArtifactExportPolicy
	maxBytes      int64
	maxEntries    int64
	uploadTimeout time.Duration
	secret        string
}

// resolveDestinations decides each output's repository, policy, credential
// and limits. A non-empty reason refuses the sandbox's outputs (fail closed):
// a policy that selects the namespace but does not compile refuses too.
func (r *SwiftSandboxReconciler) resolveDestinations(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) ([]destination, string, string, error) {
	if r.SystemNamespace == "" {
		return nil, reasonOutputExportUnavailable, "the controller has no namespace to keep export credentials in", nil
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: sb.Namespace}, &ns); err != nil {
		return nil, "", "", err
	}
	nsLabels := labels.Set(ns.Labels)
	if nsLabels == nil {
		nsLabels = labels.Set{}
	}
	nsLabels["kubernetes.io/metadata.name"] = sb.Namespace
	var list sandboxv1alpha1.SwiftArtifactExportPolicyList
	if err := r.List(ctx, &list); err != nil {
		return nil, "", "", err
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	var selecting []*sandboxv1alpha1.SwiftArtifactExportPolicy
	for i := range list.Items {
		p := &list.Items[i]
		sel, err := metav1.LabelSelectorAsSelector(&p.Spec.NamespaceSelector)
		if err != nil {
			return nil, sandboxv1alpha1.OutputReasonDestinationDenied,
				fmt.Sprintf("SwiftArtifactExportPolicy %s has an invalid namespaceSelector: %v", p.Name, err), nil
		}
		if !sel.Matches(nsLabels) {
			continue
		}
		if err := exportpolicy.Validate(&p.Spec); err != nil {
			return nil, sandboxv1alpha1.OutputReasonDestinationDenied,
				fmt.Sprintf("SwiftArtifactExportPolicy %s selects namespace %s but is invalid: %v", p.Name, sb.Namespace, err), nil
		}
		selecting = append(selecting, p)
	}
	if len(selecting) == 0 {
		return nil, sandboxv1alpha1.OutputReasonDestinationDenied,
			fmt.Sprintf("no SwiftArtifactExportPolicy selects namespace %s: outputs cannot be exported", sb.Namespace), nil
	}

	var out []destination
	for _, o := range sb.Spec.Outputs {
		repo := o.Repository
		if repo == "" {
			defaults := map[string]bool{}
			for _, p := range selecting {
				if p.Spec.DefaultRepository != "" {
					defaults[exportpolicy.Expand(p.Spec.DefaultRepository, sb.Namespace)+"/"+o.Name] = true
				}
			}
			switch len(defaults) {
			case 0:
				return nil, sandboxv1alpha1.OutputReasonDestinationDenied,
					fmt.Sprintf("output %s has no repository and no policy for namespace %s has a defaultRepository", o.Name, sb.Namespace), nil
			case 1:
				for d := range defaults {
					repo = d
				}
			default:
				return nil, sandboxv1alpha1.OutputReasonDestinationAmbiguous,
					fmt.Sprintf("output %s has no repository and several policies give a different defaultRepository", o.Name), nil
			}
		}
		if err := exportpolicy.ValidateRepository(repo); err != nil {
			return nil, sandboxv1alpha1.OutputReasonDestinationDenied, fmt.Sprintf("output %s: repository %q: %v", o.Name, repo, err), nil
		}
		var allowing []*sandboxv1alpha1.SwiftArtifactExportPolicy
		secrets := map[string]bool{}
		for _, p := range selecting {
			if exportpolicy.Allows(&p.Spec, sb.Namespace, repo) {
				allowing = append(allowing, p)
				secrets[p.Spec.CredentialsSecretRef.Name] = true
			}
		}
		if len(allowing) == 0 {
			return nil, sandboxv1alpha1.OutputReasonDestinationDenied,
				fmt.Sprintf("output %s: no SwiftArtifactExportPolicy allows repository %s for namespace %s", o.Name, repo, sb.Namespace), nil
		}
		if len(secrets) > 1 {
			return nil, sandboxv1alpha1.OutputReasonDestinationAmbiguous,
				fmt.Sprintf("output %s: policies with different credentials allow repository %s", o.Name, repo), nil
		}
		p := allowing[0]
		d := destination{repository: repo, policy: p, secret: p.Spec.CredentialsSecretRef.Name, uploadTimeout: exportpolicy.UploadTimeout(&p.Spec)}
		d.maxBytes = sandboxv1alpha1.DefaultOutputMaxSize.Value()
		if p.Spec.MaxSize != nil {
			d.maxBytes = p.Spec.MaxSize.Value()
		}
		if o.MaxSize != nil {
			if o.MaxSize.Value() > d.maxBytes {
				return nil, reasonOutputLimitExceedsPolicy, fmt.Sprintf("output %s: maxSize %s is over policy %s's %s",
					o.Name, o.MaxSize.String(), p.Name, resource.NewQuantity(d.maxBytes, resource.BinarySI).String()), nil
			}
			d.maxBytes = o.MaxSize.Value()
		}
		d.maxEntries = sandboxv1alpha1.DefaultOutputMaxEntries
		if p.Spec.MaxEntries != nil {
			d.maxEntries = *p.Spec.MaxEntries
		}
		if o.MaxEntries != nil {
			if *o.MaxEntries > d.maxEntries {
				return nil, reasonOutputLimitExceedsPolicy, fmt.Sprintf("output %s: maxEntries %d is over policy %s's %d",
					o.Name, *o.MaxEntries, p.Name, d.maxEntries), nil
			}
			d.maxEntries = *o.MaxEntries
		}
		if reason, msg, err := r.checkExportCredential(ctx, d.secret); err != nil || reason != "" {
			return nil, reason, msg, err
		}
		out = append(out, d)
	}
	return out, "", "", nil
}

// checkExportCredential: the Secret must exist in the KubeSwift namespace and
// be set aside for exports by an administrator.
func (r *SwiftSandboxReconciler) checkExportCredential(ctx context.Context, name string) (string, string, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var s corev1.Secret
	if err := reader.Get(ctx, client.ObjectKey{Namespace: r.SystemNamespace, Name: name}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return reasonOutputCredentialsMissing, fmt.Sprintf("export credential Secret %s/%s does not exist", r.SystemNamespace, name), nil
		}
		return "", "", err
	}
	if s.Labels[sandboxv1alpha1.ExportCredentialsLabel] != "true" {
		return reasonOutputCredentialsMissing, fmt.Sprintf("Secret %s/%s is not labelled %s=true", r.SystemNamespace, name, sandboxv1alpha1.ExportCredentialsLabel), nil
	}
	return "", "", nil
}

// admitOutputs runs once per sandbox with outputs, before anything is
// created: validate, resolve, record each output Pending with its repository
// and policy, and hold the sandbox with a finalizer until cleaned. Returns a
// reason when the outputs are refused.
func (r *SwiftSandboxReconciler) admitOutputs(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (string, string, error) {
	dests, reason, msg, err := r.resolveDestinations(ctx, sb)
	if err != nil || reason != "" {
		return reason, msg, err
	}
	if err := patchFinalizers(ctx, r.Client, sb, func() { controllerutil.AddFinalizer(sb, sandboxOutputsFinalizer) }); err != nil {
		return "", "", err
	}
	sb.Status.Outputs = nil
	for i, o := range sb.Spec.Outputs {
		d := dests[i]
		sb.Status.Outputs = append(sb.Status.Outputs, sandboxv1alpha1.SandboxOutputStatus{
			Name: o.Name, State: sandboxv1alpha1.SandboxOutputPending, Repository: d.repository,
			Policy: &sandboxv1alpha1.ExportPolicyReference{Name: d.policy.Name, UID: d.policy.UID},
		})
	}
	return "", "", r.Status().Update(ctx, sb)
}

// refuseOutputs records every output Failed when the sandbox's outputs are
// refused before admission, so status.outputs says why per output.
func refuseOutputs(sb *sandboxv1alpha1.SwiftSandbox, reason, msg string) {
	sb.Status.Outputs = nil
	for _, o := range sb.Spec.Outputs {
		sb.Status.Outputs = append(sb.Status.Outputs, sandboxv1alpha1.SandboxOutputStatus{
			Name: o.Name, State: sandboxv1alpha1.SandboxOutputFailed, Reason: reason, Message: truncate(msg, 512),
		})
	}
}

// withOutputLimits is sb with each output's limits set to its destination's,
// for building the launcher (not persisted).
func withOutputLimits(sb *sandboxv1alpha1.SwiftSandbox, dests []destination) *sandboxv1alpha1.SwiftSandbox {
	c := sb.DeepCopy()
	for i := range c.Spec.Outputs {
		if i < len(dests) {
			c.Spec.Outputs[i].MaxSize = resource.NewQuantity(dests[i].maxBytes, resource.BinarySI)
			c.Spec.Outputs[i].MaxEntries = ptr.To(dests[i].maxEntries)
		}
	}
	return c
}

// outputReport is one entry of annSandboxOutputs.
type outputReport struct {
	Name          string `json:"name"`
	State         string `json:"state"`
	Reason        string `json:"reason"`
	Message       string `json:"message"`
	ContentDigest string `json:"contentDigest"`
	ContentBytes  int64  `json:"contentBytes"`
	Entries       int64  `json:"entries"`
}

func outputStatus(sb *sandboxv1alpha1.SwiftSandbox, name string) *sandboxv1alpha1.SandboxOutputStatus {
	for i := range sb.Status.Outputs {
		if sb.Status.Outputs[i].Name == name {
			return &sb.Status.Outputs[i]
		}
	}
	return nil
}

func setOutputsCondition(sb *sandboxv1alpha1.SwiftSandbox, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: conditionOutputsExported, Status: status, Reason: reason, Message: msg, ObservedGeneration: sb.Generation,
	})
}

// observeOutputs handles a launcher pod of a sandbox with outputs once its
// workload has ended. It returns handled=false to let the normal pod path
// run (the workload is still running, or it failed: outputs are skipped and
// the sandbox fails as any other). spec.timeout and liveness no longer apply
// once the workload has exited: what remains is the launcher's packaging.
func (r *SwiftSandboxReconciler) observeOutputs(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, pod *corev1.Pod) (bool, ctrl.Result, error) {
	podDone := pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
	code, ok := sandboxWorkloadExitCode(pod)
	if !ok {
		// Still running, or ended without an exit line (finishTerminal says
		// why, and never reports Completed for it).
		return false, ctrl.Result{}, nil
	}
	if code != 0 {
		r.markOutputs(sb, sandboxv1alpha1.SandboxOutputSkipped, sandboxv1alpha1.OutputReasonWorkloadFailed,
			fmt.Sprintf("the workload exited %d; nothing is exported", code))
		return false, ctrl.Result{}, nil
	}
	sb.Status.ExitCode = ptr.To(int32(0))
	raw, have := pod.Annotations[annSandboxOutputs]
	if !have {
		if podDone {
			r.markOutputs(sb, sandboxv1alpha1.SandboxOutputFailed, reasonOutputCollectionFailed, "the launcher ended without reporting its outputs")
			res, err := r.fail(ctx, sb, sandboxv1alpha1.OutputReasonExportFailed, "the launcher ended without reporting its outputs")
			return true, res, err
		}
		if c := apimeta.FindStatusCondition(sb.Status.Conditions, conditionOutputsExported); c != nil && c.Reason == "Collecting" &&
			time.Since(c.LastTransitionTime.Time) > outputCollectionTimeout {
			_ = r.Delete(ctx, pod)
			r.markOutputs(sb, sandboxv1alpha1.SandboxOutputFailed, reasonOutputCollectionFailed, "packaging took longer than "+outputCollectionTimeout.String())
			res, err := r.fail(ctx, sb, sandboxv1alpha1.OutputReasonExportFailed, "packaging the outputs took longer than "+outputCollectionTimeout.String())
			return true, res, err
		}
		sb.Status.Phase = sandboxv1alpha1.SwiftSandboxExporting
		sb.Status.Message = "workload exited 0; packaging outputs"
		for i := range sb.Status.Outputs {
			if sb.Status.Outputs[i].State == sandboxv1alpha1.SandboxOutputPending {
				sb.Status.Outputs[i].State = sandboxv1alpha1.SandboxOutputCollecting
			}
		}
		setOutputsCondition(sb, metav1.ConditionFalse, "Collecting", "the launcher is packaging the outputs")
		if err := r.Status().Update(ctx, sb); err != nil {
			return true, ctrl.Result{}, err
		}
		return true, ctrl.Result{RequeueAfter: exportPollInterval}, nil
	}
	var reports []outputReport
	if err := json.Unmarshal([]byte(raw), &reports); err != nil {
		r.markOutputs(sb, sandboxv1alpha1.SandboxOutputFailed, reasonOutputCollectionFailed, "the launcher's report is unreadable")
		res, err := r.fail(ctx, sb, sandboxv1alpha1.OutputReasonExportFailed, "the launcher's output report is unreadable")
		return true, res, err
	}
	byName := map[string]outputReport{}
	for _, rep := range reports {
		byName[rep.Name] = rep
	}
	now := metav1.Now()
	for i := range sb.Status.Outputs {
		o := &sb.Status.Outputs[i]
		rep, ok := byName[o.Name]
		switch {
		case !ok:
			o.State, o.Reason, o.Message = sandboxv1alpha1.SandboxOutputFailed, reasonOutputCollectionFailed, "not reported by the launcher"
		case rep.State == "Packaged":
			o.State = sandboxv1alpha1.SandboxOutputPackaged
			o.Reason, o.Message = "", ""
			o.ContentDigest, o.ContentBytes, o.Entries = rep.ContentDigest, rep.ContentBytes, rep.Entries
			o.PackagedAt = &now
		default:
			o.State = sandboxv1alpha1.SandboxOutputFailed
			o.Reason, o.Message = firstNonEmpty(rep.Reason, reasonOutputCollectionFailed), truncate(rep.Message, 512)
		}
	}
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxExporting
	sb.Status.Message = "workload exited 0; exporting outputs"
	setOutputsCondition(sb, metav1.ConditionFalse, "Exporting", "pushing the outputs")
	if err := r.Status().Update(ctx, sb); err != nil {
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{Requeue: true}, nil
}

func (r *SwiftSandboxReconciler) markOutputs(sb *sandboxv1alpha1.SwiftSandbox, state sandboxv1alpha1.SandboxOutputState, reason, msg string) {
	for i := range sb.Status.Outputs {
		o := &sb.Status.Outputs[i]
		if outputTerminal(o.State) {
			continue
		}
		o.State, o.Reason, o.Message = state, reason, msg
	}
}

func outputTerminal(s sandboxv1alpha1.SandboxOutputState) bool {
	return s == sandboxv1alpha1.SandboxOutputExported || s == sandboxv1alpha1.SandboxOutputFailed || s == sandboxv1alpha1.SandboxOutputSkipped
}

// reconcileExport drives an Exporting sandbox: one export Job per packaged
// output, results from the Jobs' reports, then cleanup and the terminal phase.
func (r *SwiftSandboxReconciler) reconcileExport(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ctrl.Result, error) {
	// The workload is over: a native GPU is not needed for pushing.
	if res, err := r.releaseGPUWhenDone(ctx, sb); err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	nodeGone, err := r.nodeGone(ctx, sb.Status.NodeName)
	if err != nil {
		return ctrl.Result{}, err
	}
	active, err := r.activeExports(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	for i := range sb.Status.Outputs {
		o := &sb.Status.Outputs[i]
		if outputTerminal(o.State) {
			continue
		}
		if nodeGone {
			o.State, o.Reason, o.Message = sandboxv1alpha1.SandboxOutputFailed, reasonOutputLost,
				fmt.Sprintf("node %s is gone, and the packaged output with it", sb.Status.NodeName)
			metrics.SandboxOutputExportsTotal.WithLabelValues("failed", o.Reason).Inc()
			continue
		}
		switch o.State {
		case sandboxv1alpha1.SandboxOutputPackaged:
			d, reason, msg, err := r.reauthorize(ctx, sb, o)
			if err != nil {
				return ctrl.Result{}, err
			}
			if reason != "" {
				o.State, o.Reason, o.Message = sandboxv1alpha1.SandboxOutputFailed, reason, msg
				metrics.SandboxOutputExportsTotal.WithLabelValues("failed", o.Reason).Inc()
				continue
			}
			if active >= r.maxConcurrentExports() {
				o.Message = "waiting for an export slot"
				continue
			}
			if err := r.ensureExportJob(ctx, sb, o, d); err != nil {
				return ctrl.Result{}, err
			}
			active++
			o.State, o.Message = sandboxv1alpha1.SandboxOutputUploading, ""
		case sandboxv1alpha1.SandboxOutputUploading:
			if err := r.observeExportJob(ctx, sb, o); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	allDone := true
	var failed []string
	for _, o := range sb.Status.Outputs {
		if !outputTerminal(o.State) {
			allDone = false
		} else if o.State != sandboxv1alpha1.SandboxOutputExported {
			failed = append(failed, fmt.Sprintf("%s (%s)", o.Name, o.Reason))
		}
	}
	if !allDone {
		if err := r.Status().Update(ctx, sb); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: exportPollInterval}, nil
	}
	// Every result is recorded before anything that held it is removed.
	if err := r.Status().Update(ctx, sb); err != nil {
		return ctrl.Result{}, err
	}
	done, err := r.finishOutputs(ctx, sb)
	if err != nil || !done {
		return ctrl.Result{RequeueAfter: exportPollInterval}, err
	}
	if len(failed) > 0 {
		setOutputsCondition(sb, metav1.ConditionFalse, sandboxv1alpha1.OutputReasonExportFailed, "not exported: "+strings.Join(failed, ", "))
		return r.fail(ctx, sb, sandboxv1alpha1.OutputReasonExportFailed, "outputs not exported: "+strings.Join(failed, ", "))
	}
	setOutputsCondition(sb, metav1.ConditionTrue, "Exported", fmt.Sprintf("%d output(s) exported", len(sb.Status.Outputs)))
	return r.terminal(ctx, sb, sandboxv1alpha1.SwiftSandboxCompleted, "Completed", "workload exited 0; outputs exported")
}

// reauthorize checks, before the push, that the recorded destination is still
// allowed: the same policy (by UID), the same repository, the credential still
// in place. A changed policy never redirects an export; it ends it.
func (r *SwiftSandboxReconciler) reauthorize(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, o *sandboxv1alpha1.SandboxOutputStatus) (destination, string, string, error) {
	dests, reason, msg, err := r.resolveDestinations(ctx, sb)
	if err != nil {
		return destination{}, "", "", err
	}
	if reason != "" {
		return destination{}, reasonOutputDestinationRevoked, msg, nil
	}
	for i, out := range sb.Spec.Outputs {
		if out.Name != o.Name {
			continue
		}
		d := dests[i]
		if d.repository != o.Repository || o.Policy == nil || d.policy.UID != o.Policy.UID {
			return destination{}, reasonOutputDestinationRevoked,
				fmt.Sprintf("the destination resolved at admission (%s, policy %s) is no longer the one allowed", o.Repository, policyName(o.Policy)), nil
		}
		return d, "", "", nil
	}
	return destination{}, reasonOutputDestinationRevoked, "output no longer declared", nil
}

func policyName(p *sandboxv1alpha1.ExportPolicyReference) string {
	if p == nil {
		return "none"
	}
	return p.Name
}

func (r *SwiftSandboxReconciler) maxConcurrentExports() int {
	if r.MaxConcurrentExports > 0 {
		return r.MaxConcurrentExports
	}
	return DefaultMaxConcurrentExports
}

// activeExports counts export Jobs still running, cluster-wide.
func (r *SwiftSandboxReconciler) activeExports(ctx context.Context) (int, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(r.SystemNamespace), client.MatchingLabels{exportComponentLabel: exportComponentJob}); err != nil {
		return 0, err
	}
	n := 0
	for _, j := range jobs.Items {
		if !jobFinished(&j) {
			n++
		}
	}
	return n, nil
}

func jobFinished(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *SwiftSandboxReconciler) nodeGone(ctx context.Context, node string) (bool, error) {
	if node == "" {
		return true, nil
	}
	var n corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func exportJobName(sb *sandboxv1alpha1.SwiftSandbox, output string) string {
	return names.JobName(string(sb.UID)+"-"+output, "-export")
}

func cleanupJobName(sb *sandboxv1alpha1.SwiftSandbox) string {
	return names.JobName(string(sb.UID), "-out-clean")
}

func exportTag(sb *sandboxv1alpha1.SwiftSandbox, output string) string {
	return string(sb.UID) + "." + output
}

// exportSpec is what the Job pushes: computed by the controller from the
// sandbox's spec, its recorded destination and its UID, never read back from
// the launcher.
func exportSpec(sb *sandboxv1alpha1.SwiftSandbox, o *sandboxv1alpha1.SandboxOutputStatus, d destination) export.Spec {
	var spec sandboxv1alpha1.SandboxOutput
	for _, out := range sb.Spec.Outputs {
		if out.Name == o.Name {
			spec = out
		}
	}
	ann := map[string]string{}
	for k, v := range spec.Annotations {
		ann[k] = v
	}
	// Platform statements, the only annotations a signer may rely on.
	ann["io.kubeswift.export.namespace"] = sb.Namespace
	ann["io.kubeswift.export.output"] = o.Name
	return export.Spec{
		Archive:      exportSpoolDir + "/" + o.Name + ".tar",
		Repository:   o.Repository,
		Tag:          exportTag(sb, o.Name),
		ArtifactType: firstNonEmpty(spec.ArtifactType, sandboxv1alpha1.DefaultOutputArtifactType),
		Annotations:  ann,
		MaxBytes:     d.maxBytes,
		MaxEntries:   d.maxEntries,
	}
}

func (r *SwiftSandboxReconciler) ensureExportJob(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, o *sandboxv1alpha1.SandboxOutputStatus, d destination) error {
	job, err := buildExportJob(sb, o, d, r.SystemNamespace, SandboxMaterializeImage())
	if err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func exportLabels(sb *sandboxv1alpha1.SwiftSandbox, component, output string) map[string]string {
	l := map[string]string{
		"app.kubernetes.io/name":       "kubeswift",
		"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
		exportComponentLabel:           component,
		exportSandboxUIDLabel:          string(sb.UID),
	}
	if output != "" {
		l[exportOutputLabel] = output
	}
	return l
}

// buildExportJob is the only workload that mounts the push credential. It
// runs in the KubeSwift namespace on the sandbox's node, reads only that
// sandbox's spool directory, has no service account token, no capabilities
// and a read-only root, and is bounded by the policy's upload timeout.
func buildExportJob(sb *sandboxv1alpha1.SwiftSandbox, o *sandboxv1alpha1.SandboxOutputStatus, d destination, namespace, image string) (*batchv1.Job, error) {
	spec, err := json.Marshal(exportSpec(sb, o, d))
	if err != nil {
		return nil, err
	}
	deadline := int64(d.uploadTimeout.Seconds())
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: exportJobName(sb, o.Name), Namespace: namespace,
			Labels:      exportLabels(sb, exportComponentJob, o.Name),
			Annotations: map[string]string{exportForAnnotation: sb.Namespace + "/" + sb.Name},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To(int32(3)),
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: exportLabels(sb, exportComponentJob, o.Name)},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					NodeName:                     sb.Status.NodeName,
					AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{{
						Name:  "export",
						Image: image,
						Args:  []string{"export-output", "--docker-config=" + exportAuthDir + "/config.json"},
						Env:   withRegistryCA([]corev1.EnvVar{{Name: "KUBESWIFT_EXPORT_SPEC", Value: string(spec)}}),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
						},
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:                ptr.To(int64(0)),
							RunAsNonRoot:             ptr.To(false),
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "spool", MountPath: exportSpoolDir, ReadOnly: true},
							{Name: "auth", MountPath: exportAuthDir, ReadOnly: true},
						},
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
					}},
					Volumes: []corev1.Volume{
						{Name: "spool", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
							Path: OutputSpoolPath(sb), Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}},
						{Name: "auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: d.secret,
							Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
						}}},
					},
				},
			},
		},
	}, nil
}

// observeExportJob records a finished Job's result. Only a report from a pod
// the Job controls counts.
func (r *SwiftSandboxReconciler) observeExportJob(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, o *sandboxv1alpha1.SandboxOutputStatus) error {
	var job batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.SystemNamespace, Name: exportJobName(sb, o.Name)}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			// Gone before its result was recorded: push again (same content,
			// same digest).
			o.State = sandboxv1alpha1.SandboxOutputPackaged
			return nil
		}
		return err
	}
	var complete, failed bool
	var failMsg, failReason string
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			complete = true
		case batchv1.JobFailed:
			failed, failMsg, failReason = true, c.Message, c.Reason
		}
	}
	if !complete && !failed {
		return nil
	}
	rep, ok, err := r.exportReport(ctx, &job)
	if err != nil {
		return err
	}
	if complete && ok && rep.Reason == "" && rep.Digest != "" {
		now := metav1.Now()
		o.State, o.Reason, o.Message = sandboxv1alpha1.SandboxOutputExported, "", ""
		o.Digest, o.Reference, o.Size = rep.Digest, rep.Reference, rep.Size
		o.ContentDigest, o.ContentBytes, o.Entries = rep.ContentDigest, rep.ContentBytes, rep.Entries
		o.ExportedAt = &now
		metrics.SandboxOutputExportsTotal.WithLabelValues("exported", "").Inc()
		return nil
	}
	defer func() { metrics.SandboxOutputExportsTotal.WithLabelValues("failed", o.Reason).Inc() }()
	o.State = sandboxv1alpha1.SandboxOutputFailed
	switch {
	case ok && rep.Reason != "":
		o.Reason, o.Message = rep.Reason, truncate(rep.Message, 512)
	case failReason == "DeadlineExceeded":
		o.Reason, o.Message = reasonOutputUploadTimeout, "the push did not finish within the policy's uploadTimeout"
		if ok && rep.Message != "" {
			o.Message = truncate(o.Message+"; last error: "+rep.Message, 512)
		}
	default:
		o.Reason = reasonOutputRegistryUnavailable
		o.Message = truncate(firstNonEmpty(rep.Message, failMsg, "the export Job failed"), 512)
	}
	return nil
}

// exportReport reads the newest report of the Job's own pods (a definitive
// failure is reported by a pod that exited 0; a retried one by the last).
func (r *SwiftSandboxReconciler) exportReport(ctx context.Context, job *batchv1.Job) (export.Result, bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return export.Result{}, false, err
	}
	var best export.Result
	var bestAt time.Time
	found := false
	for i := range pods.Items {
		p := &pods.Items[i]
		if !metav1.IsControlledBy(p, job) {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			t := cs.State.Terminated
			if t == nil || t.Message == "" {
				continue
			}
			var rep export.Result
			if json.Unmarshal([]byte(t.Message), &rep) != nil {
				continue
			}
			if !found || t.FinishedAt.Time.After(bestAt) {
				best, bestAt, found = rep, t.FinishedAt.Time, true
			}
		}
	}
	return best, found, nil
}

// cleanOutputs removes the sandbox's export Jobs and its spool directory on
// the node. Bounded: a cleanup that cannot finish is reported, not waited on
// forever. Returns true when done.
func (r *SwiftSandboxReconciler) cleanOutputs(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, nodeGone bool) (bool, error) {
	if r.SystemNamespace == "" {
		return true, nil
	}
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(r.SystemNamespace),
		client.MatchingLabels{exportComponentLabel: exportComponentJob, exportSandboxUIDLabel: string(sb.UID)}); err != nil {
		return false, err
	}
	for i := range jobs.Items {
		if err := r.Delete(ctx, &jobs.Items[i], client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	if nodeGone || sb.Status.NodeName == "" {
		return true, nil
	}
	// A v0.19.0 controller ran the cleanup as a bare pod: one left over is
	// removed (it holds nothing).
	legacy := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: r.SystemNamespace, Name: cleanupJobName(sb)}}
	if err := r.Delete(ctx, legacy); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	name := cleanupJobName(sb)
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: r.SystemNamespace, Name: name}, &job)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, buildCleanupJob(sb, name, r.SystemNamespace, SandboxMaterializeImage())); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// The Job is left for its TTL to remove, so a reconcile that runs from a
	// stale copy of the sandbox after this one cannot leave anything behind
	// (#838): it finds this Job, or creates one that removes itself.
	complete, failed := false, false
	for _, c := range job.Status.Conditions {
		if c.Status == corev1.ConditionTrue {
			complete = complete || c.Type == batchv1.JobComplete
			failed = failed || c.Type == batchv1.JobFailed
		}
	}
	switch {
	case complete:
	case failed || time.Since(job.CreationTimestamp.Time) > outputCleanupTimeout+time.Minute:
		if r.Recorder != nil {
			r.Recorder.Eventf(sb, corev1.EventTypeWarning, "OutputCleanupAbandoned",
				"could not clean %s on node %s; remove it by hand", OutputSpoolPath(sb), sb.Status.NodeName)
		}
	default:
		return false, nil
	}
	return true, nil
}

// outputCleanupTTL is how long a finished cleanup Job is kept.
const outputCleanupTTL = 5 * time.Minute

// buildCleanupJob removes one sandbox's spool directory on its node. A Job,
// not a bare pod, so that it removes itself once finished.
func buildCleanupJob(sb *sandboxv1alpha1.SwiftSandbox, name, namespace, image string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels:      exportLabels(sb, exportComponentClean, ""),
			Annotations: map[string]string{exportForAnnotation: sb.Namespace + "/" + sb.Name},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(2)),
			ActiveDeadlineSeconds:   ptr.To(int64(outputCleanupTimeout.Seconds())),
			TTLSecondsAfterFinished: ptr.To(int32(outputCleanupTTL.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: exportLabels(sb, exportComponentClean, "")},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					NodeName:                     sb.Status.NodeName,
					AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{{
						Name:  "clean",
						Image: image,
						Args:  []string{"clean-outputs", "--root=" + exportSpoolDir, "--sandbox-uid=" + string(sb.UID)},
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:                ptr.To(int64(0)),
							RunAsNonRoot:             ptr.To(false),
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "spool", MountPath: exportSpoolDir}},
					}},
					// The spool root, so the sandbox's directory itself goes too; the
					// UID is the controller's, checked again by clean-outputs.
					Volumes: []corev1.Volume{{Name: "spool", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
						Path: OutputSpoolDir, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}}},
				},
			},
		},
	}
}

// deleteOutputs is the deletion path: Jobs and spool removed, bounded, then
// the finalizer released.
func (r *SwiftSandboxReconciler) deleteOutputs(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (bool, error) {
	nodeGone, err := r.nodeGone(ctx, sb.Status.NodeName)
	if err != nil {
		return false, err
	}
	done, err := r.cleanOutputs(ctx, sb, nodeGone)
	if err != nil {
		return false, err
	}
	if !done && sb.DeletionTimestamp != nil && time.Since(sb.DeletionTimestamp.Time) < outputCleanupTimeout+time.Minute {
		return false, nil
	}
	return true, patchFinalizers(ctx, r.Client, sb, func() { controllerutil.RemoveFinalizer(sb, sandboxOutputsFinalizer) })
}

// exportingOutputs: the sandbox is Exporting and every output is packaged
// (or already past it), so only export Jobs remain.
func exportingOutputs(sb *sandboxv1alpha1.SwiftSandbox) bool {
	if sb.Status.Phase != sandboxv1alpha1.SwiftSandboxExporting {
		return false
	}
	for _, o := range sb.Status.Outputs {
		if o.State == sandboxv1alpha1.SandboxOutputPending || o.State == sandboxv1alpha1.SandboxOutputCollecting {
			return false
		}
	}
	return true
}

// launchDestinations resolves the destinations for building the launcher and
// checks they are the ones recorded at admission.
func (r *SwiftSandboxReconciler) launchDestinations(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) ([]destination, string, string, error) {
	dests, reason, msg, err := r.resolveDestinations(ctx, sb)
	if err != nil || reason != "" {
		return nil, reason, msg, err
	}
	for i, d := range dests {
		o := outputStatus(sb, sb.Spec.Outputs[i].Name)
		if o == nil || o.Repository != d.repository || o.Policy == nil || o.Policy.UID != d.policy.UID {
			return nil, reasonOutputDestinationRevoked,
				fmt.Sprintf("output %s: the destination recorded at admission is no longer the one allowed", sb.Spec.Outputs[i].Name), nil
		}
	}
	return dests, "", "", nil
}

// launcherStopped: the sandbox's launcher no longer runs, so nothing writes
// to its spool directory.
func (r *SwiftSandboxReconciler) launcherStopped(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (bool, error) {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: sb.Namespace, Name: firstNonEmpty(sb.Status.PodRef, sb.Name)}, &pod)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !metav1.IsControlledBy(&pod, sb) {
		return true, nil
	}
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed, nil
}

// finishOutputs cleans a sandbox's export Jobs and spool once its launcher
// has stopped, then releases sandboxOutputsFinalizer. True when done.
func (r *SwiftSandboxReconciler) finishOutputs(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (bool, error) {
	if !controllerutil.ContainsFinalizer(sb, sandboxOutputsFinalizer) {
		return true, nil
	}
	stopped, err := r.launcherStopped(ctx, sb)
	if err != nil || !stopped {
		return false, err
	}
	nodeGone, err := r.nodeGone(ctx, sb.Status.NodeName)
	if err != nil {
		return false, err
	}
	done, err := r.cleanOutputs(ctx, sb, nodeGone)
	if err != nil || !done {
		return false, err
	}
	return true, patchFinalizers(ctx, r.Client, sb, func() { controllerutil.RemoveFinalizer(sb, sandboxOutputsFinalizer) })
}

// exportObjectToSandbox maps an export or cleanup Job, or one of their pods,
// to its sandbox.
func exportObjectToSandbox(_ context.Context, obj client.Object) []reconcile.Request {
	c := obj.GetLabels()[exportComponentLabel]
	if c != exportComponentJob && c != exportComponentClean {
		return nil
	}
	ns, name, ok := strings.Cut(obj.GetAnnotations()[exportForAnnotation], "/")
	if !ok || ns == "" || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: ns, Name: name}}}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
