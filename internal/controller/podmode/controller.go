// Package podmode is the pod-mode stand-in controller. For each stand-in Pod
// (rewritten at admission by internal/webhook/podmode) it runs the recorded
// workload in a SwiftSandbox in the pod-mode namespace, pinned to the
// stand-in's node, and points the stand-in's shim at the sandbox's launcher.
//
// Pods cannot own objects in another namespace, so the link is labels plus a
// finalizer: the SwiftSandbox carries the stand-in's UID and namespace, and the
// stand-in keeps the finalizer until its SwiftSandbox is gone. A SwiftSandbox
// whose stand-in no longer exists is deleted.
//
// Two NetworkPolicies let the shim reach its launcher and nothing else: the
// SwiftSandbox's ingress admits only its stand-in (by namespace and UID label),
// and a policy in the tenant namespace, owned by the stand-in, allows the
// stand-in egress only to its launcher's ports. NetworkPolicies are additive,
// so the second also opens that path through policies that would block it
// (agent-sandbox's managed policy allows only public destinations).
package podmode

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftsandbox"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

const (
	// SandboxPrefix starts the name of every pod-mode SwiftSandbox; the rest
	// is the stand-in's UID.
	SandboxPrefix = "pm-"
	// NetworkPolicyPrefix starts the name of a stand-in's egress policy.
	NetworkPolicyPrefix = "kubeswift-podmode-"

	deleteRequeue = 2 * time.Second
	configRequeue = 10 * time.Second
)

// Reconciler links stand-in Pods to SwiftSandboxes.
type Reconciler struct {
	client.Client
	// APIReader reads the stand-in's Secrets and ConfigMaps uncached; nil
	// uses Client.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
	// Namespace holds every pod-mode SwiftSandbox and launcher.
	Namespace string
	// ShimImage returns the image stand-ins run (pod-shim).
	ShimImage func() string
	// Relay resolves where shims reach the exec and logs relay; nil when
	// the relay is not served (no webhook server).
	Relay func(ctx context.Context) (*podmode.Relay, error)
	// RelayPeer is the controller-manager pods serving the relay, for the
	// stand-in's egress policy.
	RelayPeer *RelayPeer
}

// RelayPeer selects the pods that serve the relay.
type RelayPeer struct {
	Namespace string
	Labels    map[string]string
	Port      int32
}

// SandboxName is the SwiftSandbox of a stand-in.
func SandboxName(pod *corev1.Pod) string { return SandboxPrefix + string(pod.UID) }

// Reconcile implements reconcile.Reconciler for a stand-in Pod.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			_, err := r.deleteSandboxes(ctx, req.Namespace, req.Name, "")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
	}
	if !podmode.IsStandIn(&pod) && !controllerutil.ContainsFinalizer(&pod, podmode.Finalizer) {
		return ctrl.Result{}, nil
	}

	done := pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
	if pod.DeletionTimestamp != nil || done {
		return r.release(ctx, &pod)
	}
	if !podmode.ShapeOK(&pod, r.ShimImage()) {
		// The admission policy keeps a stand-in to the shim; this one was
		// changed anyway (policy missing or bypassed). Nothing but the shim
		// may run in a stand-in, so it goes, and its microVM with it.
		r.Recorder.Event(&pod, corev1.EventTypeWarning, "StandInTampered",
			"this pod-mode stand-in no longer has exactly the shim container; deleting it and its microVM")
		log.Info("deleting a tampered pod-mode stand-in", "pod", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, &pod))
	}
	if !controllerutil.ContainsFinalizer(&pod, podmode.Finalizer) {
		// Link only stand-ins in an opted-in namespace: a Pod labelled as a
		// stand-in by hand elsewhere gets no microVM.
		ok, err := r.namespaceOptedIn(ctx, pod.Namespace)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ok {
			r.Recorder.Eventf(&pod, corev1.EventTypeWarning, "NamespaceNotOptedIn",
				"namespace %s is not labelled %s=%s; this stand-in gets no microVM",
				pod.Namespace, podmode.NamespaceLabel, podmode.NamespaceLabelValue)
			return ctrl.Result{}, nil
		}
	}
	if !controllerutil.ContainsFinalizer(&pod, podmode.Finalizer) || pod.Labels[podmode.LabelUID] != string(pod.UID) {
		patch := client.MergeFrom(pod.DeepCopy())
		controllerutil.AddFinalizer(&pod, podmode.Finalizer)
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[podmode.LabelUID] = string(pod.UID)
		if err := r.Patch(ctx, &pod, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	// A SwiftSandbox left by an earlier Pod of the same name.
	if _, err := r.deleteSandboxes(ctx, pod.Namespace, pod.Name, pod.UID); err != nil {
		return ctrl.Result{}, err
	}
	if pod.Spec.NodeName == "" {
		return ctrl.Result{}, nil
	}
	wl, err := podmode.ParseWorkload(&pod)
	if err != nil {
		r.Recorder.Event(&pod, corev1.EventTypeWarning, "WorkloadInvalid", err.Error())
		return ctrl.Result{}, nil
	}

	if err := r.ensureEgressPolicy(ctx, &pod, wl); err != nil {
		return ctrl.Result{}, err
	}
	// This run already ended: never boot it again, even if its SwiftSandbox
	// was removed by someone else; only a restart starts a new run.
	if run, code, ok := podmode.ParseExit(pod.Annotations[podmode.AnnotationExit]); ok && run == RunOf(&pod) {
		var cur sandboxv1alpha1.SwiftSandbox
		err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: SandboxName(&pod)}, &cur)
		if apierrors.IsNotFound(err) {
			return r.afterExit(ctx, &pod, nil, code)
		}
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	sb, err := r.ensureSandbox(ctx, &pod, wl)
	if ce, ok := isConfigError(err); ok {
		// As the kubelet does for a container, wait for the Secret or
		// ConfigMap rather than start without it.
		r.Recorder.Event(&pod, corev1.EventTypeWarning, "MicroVMConfigError", ce.Msg)
		return ctrl.Result{RequeueAfter: configRequeue}, r.setReady(ctx, &pod, false, "MicroVMConfigError", ce.Msg)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if sb.DeletionTimestamp != nil {
		// The previous run's microVM is going (a restart); the next one is
		// created once it is gone.
		return ctrl.Result{RequeueAfter: deleteRequeue},
			r.setReady(ctx, &pod, false, "MicroVMRestarting", "the previous microVM is being removed")
	}
	if runOfSandbox(sb) != RunOf(&pod) {
		// A microVM of another run (the controller restarted between
		// numbering the run and deleting the old one).
		return ctrl.Result{RequeueAfter: deleteRequeue}, client.IgnoreNotFound(r.Delete(ctx, sb))
	}
	switch sb.Status.Phase {
	case sandboxv1alpha1.SwiftSandboxCompleted, sandboxv1alpha1.SwiftSandboxFailed:
		return r.exited(ctx, &pod, sb)
	}
	if err := r.ensureTarget(ctx, &pod, sb); err != nil {
		return ctrl.Result{}, err
	}
	if sb.Status.Phase == sandboxv1alpha1.SwiftSandboxRunning && pod.Annotations[podmode.AnnotationTarget] != "" {
		return ctrl.Result{}, r.setReady(ctx, &pod, true, "MicroVMRunning", "")
	}
	return ctrl.Result{}, r.setReady(ctx, &pod, false, "MicroVMStarting",
		fmt.Sprintf("SwiftSandbox %s/%s is %s", sb.Namespace, sb.Name, phaseOr(sb.Status.Phase)))
}

func phaseOr(p sandboxv1alpha1.SwiftSandboxPhase) string {
	if p == "" {
		return "Pending"
	}
	return string(p)
}

// RunOf is the stand-in's current run (AnnotationRun, 1 when unset).
func RunOf(pod *corev1.Pod) int {
	if n, err := strconv.Atoi(pod.Annotations[podmode.AnnotationRun]); err == nil && n > 0 {
		return n
	}
	return 1
}

func runOfSandbox(sb *sandboxv1alpha1.SwiftSandbox) int {
	if n, err := strconv.Atoi(sb.Annotations[podmode.AnnotationRun]); err == nil && n > 0 {
		return n
	}
	return 1
}

// exitCode is the workload's exit code: the SwiftSandbox's when known, else
// 0 for Completed and 1 for Failed.
func exitCode(sb *sandboxv1alpha1.SwiftSandbox) int {
	if sb.Status.ExitCode != nil {
		return int(*sb.Status.ExitCode)
	}
	if sb.Status.Phase == sandboxv1alpha1.SwiftSandboxCompleted {
		return 0
	}
	return 1
}

// shimStatus is the stand-in container's restart count and whether it runs.
func shimStatus(pod *corev1.Pod) (restarts int, running bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		return int(cs.RestartCount), cs.State.Running != nil
	}
	return 0, false
}

// exited handles a run whose workload ended. It tells the shim the exit code
// (the shim exits with it, so the Pod ends as any Pod would); if the kubelet
// then restarts the shim, as restartPolicy says it does, the next run gets a
// new microVM.
func (r *Reconciler) exited(ctx context.Context, pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) (ctrl.Result, error) {
	run, code := RunOf(pod), exitCode(sb)
	restarts, _ := shimStatus(pod)
	want := podmode.FormatExit(run, code)
	if pod.Annotations[podmode.AnnotationExit] != want {
		if sb.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed {
			r.Recorder.Eventf(pod, corev1.EventTypeWarning, "MicroVMFailed",
				"SwiftSandbox %s/%s failed (exit code %d): %s", sb.Namespace, sb.Name, code, sb.Status.Message)
		} else {
			r.Recorder.Eventf(pod, corev1.EventTypeNormal, "MicroVMExited",
				"the workload exited with code %d", code)
		}
		patch := client.MergeFrom(pod.DeepCopy())
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[podmode.AnnotationExit] = want
		pod.Annotations[podmode.AnnotationExitRestarts] = strconv.Itoa(restarts)
		if err := r.Patch(ctx, pod, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.afterExit(ctx, pod, sb, code)
}

// afterExit keeps a run that ended not ready and, once the kubelet restarted
// the shim as restartPolicy says, numbers the next run and removes this run's
// microVM (sb, nil when already gone).
func (r *Reconciler) afterExit(ctx context.Context, pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox, code int) (ctrl.Result, error) {
	run := RunOf(pod)
	restarts, running := shimStatus(pod)
	if err := r.setReady(ctx, pod, false, "MicroVMExited", fmt.Sprintf("the workload exited with code %d", code)); err != nil {
		return ctrl.Result{}, err
	}
	at, err := strconv.Atoi(pod.Annotations[podmode.AnnotationExitRestarts])
	if err != nil || !podmode.Restarts(pod.Spec.RestartPolicy, code) || restarts <= at || !running {
		return ctrl.Result{}, nil
	}
	// The kubelet restarted the shim: number the next run, then remove this
	// run's microVM; the next reconcile creates the new one.
	patch := client.MergeFrom(pod.DeepCopy())
	pod.Annotations[podmode.AnnotationRun] = strconv.Itoa(run + 1)
	if err := r.Patch(ctx, pod, patch); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(pod, corev1.EventTypeNormal, "MicroVMRestarting", "starting run %d in a new microVM", run+1)
	if sb == nil {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{RequeueAfter: deleteRequeue}, client.IgnoreNotFound(r.Delete(ctx, sb))
}

// setReady sets the stand-in's readiness gate condition.
func (r *Reconciler) setReady(ctx context.Context, pod *corev1.Pod, ready bool, reason, msg string) error {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == podmode.ConditionMicroVMReady && c.Status == status && c.Reason == reason && c.Message == msg {
			return nil
		}
	}
	patch := client.StrategicMergeFrom(pod.DeepCopy())
	cond := corev1.PodCondition{
		Type: podmode.ConditionMicroVMReady, Status: status, Reason: reason, Message: msg,
		LastTransitionTime: metav1.Now(),
	}
	replaced := false
	for i, c := range pod.Status.Conditions {
		if c.Type == podmode.ConditionMicroVMReady {
			if c.Status == status {
				cond.LastTransitionTime = c.LastTransitionTime
			}
			pod.Status.Conditions[i] = cond
			replaced = true
		}
	}
	if !replaced {
		pod.Status.Conditions = append(pod.Status.Conditions, cond)
	}
	return r.Status().Patch(ctx, pod, patch)
}

// release deletes the stand-in's SwiftSandbox and drops the finalizer once it
// is gone (with its launcher: the SwiftSandbox holds its own finalizers).
func (r *Reconciler) release(ctx context.Context, pod *corev1.Pod) (ctrl.Result, error) {
	remaining, err := r.deleteSandboxes(ctx, pod.Namespace, pod.Name, "")
	if err != nil {
		return ctrl.Result{}, err
	}
	if remaining > 0 {
		return ctrl.Result{RequeueAfter: deleteRequeue}, nil
	}
	if err := r.deleteConfigSecret(ctx, ConfigSecretName(pod)); err != nil {
		return ctrl.Result{}, err
	}
	if !controllerutil.ContainsFinalizer(pod, podmode.Finalizer) {
		return ctrl.Result{}, nil
	}
	patch := client.MergeFromWithOptions(pod.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.RemoveFinalizer(pod, podmode.Finalizer)
	return ctrl.Result{}, client.IgnoreNotFound(r.Patch(ctx, pod, patch))
}

// deleteSandboxes deletes the SwiftSandboxes of the stand-in namespace/name,
// except the one of UID keep, and returns how many still exist.
func (r *Reconciler) deleteSandboxes(ctx context.Context, namespace, name string, keep types.UID) (int, error) {
	var list sandboxv1alpha1.SwiftSandboxList
	if err := r.List(ctx, &list, client.InNamespace(r.Namespace),
		client.MatchingLabels{podmode.LabelStandInNamespace: namespace}); err != nil {
		return 0, err
	}
	remaining := 0
	for i := range list.Items {
		sb := &list.Items[i]
		if sb.Annotations[podmode.AnnotationStandIn] != namespace+"/"+name {
			continue
		}
		if keep != "" && sb.Labels[podmode.LabelUID] == string(keep) {
			continue
		}
		remaining++
		if sb.DeletionTimestamp == nil {
			if err := r.Delete(ctx, sb); client.IgnoreNotFound(err) != nil {
				return 0, err
			}
		}
		if err := r.deleteConfigSecret(ctx, sb.Name); err != nil {
			return 0, err
		}
	}
	return remaining, nil
}

func (r *Reconciler) namespaceOptedIn(ctx context.Context, name string) (bool, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &ns); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return ns.Labels[podmode.NamespaceLabel] == podmode.NamespaceLabelValue && ns.DeletionTimestamp == nil, nil
}

// ensureSandbox creates the stand-in's SwiftSandbox if it does not exist. An
// existing one is left as it is: a SwiftSandbox's spec is the workload as it
// was at admission, and the workload annotation cannot change afterwards.
func (r *Reconciler) ensureSandbox(ctx context.Context, pod *corev1.Pod, wl *podmode.Workload) (*sandboxv1alpha1.SwiftSandbox, error) {
	var sb sandboxv1alpha1.SwiftSandbox
	key := types.NamespacedName{Namespace: r.Namespace, Name: SandboxName(pod)}
	err := r.Get(ctx, key, &sb)
	if err == nil || !apierrors.IsNotFound(err) {
		return &sb, err
	}
	hostname, err := r.nodeHostname(ctx, pod.Spec.NodeName)
	if err != nil {
		return nil, err
	}
	cfg, err := ResolveConfig(ctx, r.reader(), pod, wl)
	if err != nil {
		return nil, err
	}
	if len(cfg.Data) > 0 {
		if err := r.ensureConfigSecret(ctx, pod, cfg.Data); err != nil {
			return nil, err
		}
	}
	want, err := SandboxFor(pod, wl, cfg, r.Namespace, hostname)
	if err != nil {
		r.Recorder.Event(pod, corev1.EventTypeWarning, "WorkloadInvalid", err.Error())
		return nil, reconcile.TerminalError(err)
	}
	if err := r.Create(ctx, want); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return want, r.Get(ctx, key, want)
		}
		r.Recorder.Eventf(pod, corev1.EventTypeWarning, "MicroVMCreateFailed", "creating SwiftSandbox %s: %v", key, err)
		return nil, err
	}
	r.Recorder.Eventf(pod, corev1.EventTypeNormal, "MicroVMCreated", "SwiftSandbox %s on node %s", key, pod.Spec.NodeName)
	return want, nil
}

// nodeHostname is the node's kubernetes.io/hostname label, which is what a
// nodeSelector can pin to (it need not equal the node name).
func (r *Reconciler) nodeHostname(ctx context.Context, nodeName string) (string, error) {
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return "", err
	}
	if h := node.Labels[corev1.LabelHostname]; h != "" {
		return h, nil
	}
	return nodeName, nil
}

// SandboxFor is the SwiftSandbox that runs a stand-in's workload, with the
// environment and files cfg resolved (nil: the workload's env as it is).
func SandboxFor(pod *corev1.Pod, wl *podmode.Workload, cfg *Config, namespace, hostname string) (*sandboxv1alpha1.SwiftSandbox, error) {
	mem, err := resource.ParseQuantity(wl.Memory)
	if err != nil {
		return nil, fmt.Errorf("workload memory %q: %w", wl.Memory, err)
	}
	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SandboxName(pod),
			Namespace: namespace,
			Labels: map[string]string{
				podmode.LabelUID:              string(pod.UID),
				podmode.LabelStandInNamespace: pod.Namespace,
			},
			Annotations: map[string]string{
				podmode.AnnotationStandIn: pod.Namespace + "/" + pod.Name,
				podmode.AnnotationRun:     strconv.Itoa(RunOf(pod)),
			},
		},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image:        wl.Image,
			Command:      wl.Command,
			Args:         wl.Args,
			Env:          wl.Env,
			WorkingDir:   wl.WorkingDir,
			SecretFiles:  cfgFiles(cfg),
			CPU:          wl.CPU,
			Memory:       mem,
			NodeSelector: map[string]string{corev1.LabelHostname: hostname},
			Network:      sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkRestricted},
		},
	}
	if cfg != nil {
		sb.Spec.Env = cfg.Env
	}
	if wl.RunAsUser != nil || wl.RunAsGroup != nil || wl.RunAsNonRoot != nil {
		sb.Spec.SecurityContext = &sandboxv1alpha1.SandboxSecurityContext{
			RunAsUser: wl.RunAsUser, RunAsGroup: wl.RunAsGroup, RunAsNonRoot: wl.RunAsNonRoot,
		}
	}
	if len(wl.Ports) > 0 {
		for _, p := range wl.Ports {
			sb.Spec.Network.Ports = append(sb.Spec.Network.Ports,
				sandboxv1alpha1.SandboxPort{Name: p.Name, Port: p.Port, Protocol: corev1.ProtocolTCP})
		}
		// Only this stand-in may reach the launcher's ports.
		sb.Spec.Network.Ingress = &sandboxv1alpha1.SandboxIngress{From: []networkingv1.NetworkPolicyPeer{standInPeer(pod)}}
	}
	return sb, nil
}

func cfgFiles(cfg *Config) []sandboxv1alpha1.SandboxSecretFile {
	if cfg == nil {
		return nil
	}
	return cfg.Files
}

func standInPeer(pod *corev1.Pod) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: pod.Namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{podmode.LabelUID: string(pod.UID)}},
	}
}

// EgressPolicyFor is the tenant-namespace NetworkPolicy that lets a stand-in
// reach its launcher's ports, and nothing else.
func EgressPolicyFor(pod *corev1.Pod, wl *podmode.Workload, namespace string, relay *RelayPeer) *networkingv1.NetworkPolicy {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NetworkPolicyPrefix + string(pod.UID),
			Namespace: pod.Namespace,
			Labels:    map[string]string{podmode.LabelUID: string(pod.UID)},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{podmode.LabelUID: string(pod.UID)}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
	tcp := corev1.ProtocolTCP
	if relay != nil {
		port := intstr.FromInt32(relay.Port)
		np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: relay.Namespace}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: relay.Labels},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		})
	}
	if len(wl.Ports) == 0 {
		return np
	}
	rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{swiftsandbox.SandboxLabelKey: SandboxName(pod)}},
	}}}
	for _, p := range wl.Ports {
		port := intstr.FromInt32(p.Port)
		rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}
	np.Spec.Egress = append(np.Spec.Egress, rule)
	return np
}

func (r *Reconciler) ensureEgressPolicy(ctx context.Context, pod *corev1.Pod, wl *podmode.Workload) error {
	want := EgressPolicyFor(pod, wl, r.Namespace, r.RelayPeer)
	if err := controllerutil.SetControllerReference(pod, want, r.Scheme); err != nil {
		return err
	}
	var got networkingv1.NetworkPolicy
	err := r.Get(ctx, client.ObjectKeyFromObject(want), &got)
	if apierrors.IsNotFound(err) {
		return client.IgnoreAlreadyExists(r.Create(ctx, want))
	}
	if err != nil || equality.Semantic.DeepEqual(got.Spec, want.Spec) {
		return err
	}
	got.Spec = want.Spec
	return r.Update(ctx, &got)
}

// ensureTarget points the stand-in's shim at the launcher pod IP and the relay.
func (r *Reconciler) ensureTarget(ctx context.Context, pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.Namespace),
		client.MatchingLabels{swiftsandbox.SandboxLabelKey: sb.Name}); err != nil {
		return err
	}
	ip := ""
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" {
			ip = p.Status.PodIP
			break
		}
	}
	want := map[string]string{}
	if ip != "" {
		want[podmode.AnnotationTarget] = ip
	}
	if r.Relay != nil {
		rel, err := r.Relay(ctx)
		if err != nil {
			return err
		}
		b, err := json.Marshal(rel)
		if err != nil {
			return err
		}
		want[podmode.AnnotationRelay] = string(b)
	}
	patch := client.MergeFrom(pod.DeepCopy())
	changed := false
	for k, v := range want {
		if pod.Annotations[k] != v {
			if pod.Annotations == nil {
				pod.Annotations = map[string]string{}
			}
			pod.Annotations[k] = v
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.Patch(ctx, pod, patch)
}

// SetupWithManager watches stand-ins, their SwiftSandboxes and launcher pods.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("podmode").
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapPod)).
		Watches(&sandboxv1alpha1.SwiftSandbox{}, handler.EnqueueRequestsFromMapFunc(r.mapSandbox)).
		Complete(r)
}

// mapPod enqueues a stand-in itself, and for a pod-mode launcher pod the
// stand-in of its SwiftSandbox.
func (r *Reconciler) mapPod(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	if podmode.IsStandIn(pod) || controllerutil.ContainsFinalizer(pod, podmode.Finalizer) {
		return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(pod)}}
	}
	name := pod.Labels[swiftsandbox.SandboxLabelKey]
	if pod.Namespace != r.Namespace || name == "" {
		return nil
	}
	var sb sandboxv1alpha1.SwiftSandbox
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: name}, &sb); err != nil {
		return nil
	}
	return r.mapSandbox(ctx, &sb)
}

// mapSandbox enqueues a pod-mode SwiftSandbox's stand-in.
func (r *Reconciler) mapSandbox(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != r.Namespace {
		return nil
	}
	ns, name, ok := splitStandIn(obj.GetAnnotations()[podmode.AnnotationStandIn])
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
}

func splitStandIn(v string) (string, string, bool) {
	for i := 0; i < len(v); i++ {
		if v[i] == '/' {
			return v[:i], v[i+1:], i > 0 && i < len(v)-1
		}
	}
	return "", "", false
}
