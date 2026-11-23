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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
)

// Reconciler links stand-in Pods to SwiftSandboxes.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Namespace holds every pod-mode SwiftSandbox and launcher.
	Namespace string
	// ShimImage returns the image stand-ins run (sandbox-materialize).
	ShimImage func() string
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

	sb, err := r.ensureSandbox(ctx, &pod, wl)
	if err != nil {
		return ctrl.Result{}, err
	}
	if sb.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed {
		r.Recorder.Eventf(&pod, corev1.EventTypeWarning, "MicroVMFailed",
			"SwiftSandbox %s/%s failed: %s", sb.Namespace, sb.Name, sb.Status.Message)
	}
	if err := r.ensureEgressPolicy(ctx, &pod, wl); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.ensureTarget(ctx, &pod, sb)
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
	want, err := SandboxFor(pod, wl, r.Namespace, hostname)
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

// SandboxFor is the SwiftSandbox that runs a stand-in's workload.
func SandboxFor(pod *corev1.Pod, wl *podmode.Workload, namespace, hostname string) (*sandboxv1alpha1.SwiftSandbox, error) {
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
			Annotations: map[string]string{podmode.AnnotationStandIn: pod.Namespace + "/" + pod.Name},
		},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image:        wl.Image,
			Command:      wl.Command,
			Args:         wl.Args,
			Env:          wl.Env,
			WorkingDir:   wl.WorkingDir,
			CPU:          wl.CPU,
			Memory:       mem,
			NodeSelector: map[string]string{corev1.LabelHostname: hostname},
			Network:      sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkRestricted},
		},
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

func standInPeer(pod *corev1.Pod) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: pod.Namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{podmode.LabelUID: string(pod.UID)}},
	}
}

// EgressPolicyFor is the tenant-namespace NetworkPolicy that lets a stand-in
// reach its launcher's ports, and nothing else.
func EgressPolicyFor(pod *corev1.Pod, wl *podmode.Workload, namespace string) *networkingv1.NetworkPolicy {
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
	if len(wl.Ports) == 0 {
		return np
	}
	tcp := corev1.ProtocolTCP
	rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{swiftsandbox.SandboxLabelKey: SandboxName(pod)}},
	}}}
	for _, p := range wl.Ports {
		port := intstr.FromInt32(p.Port)
		rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}
	np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{rule}
	return np
}

func (r *Reconciler) ensureEgressPolicy(ctx context.Context, pod *corev1.Pod, wl *podmode.Workload) error {
	want := EgressPolicyFor(pod, wl, r.Namespace)
	if err := controllerutil.SetControllerReference(pod, want, r.Scheme); err != nil {
		return err
	}
	var got networkingv1.NetworkPolicy
	err := r.Get(ctx, client.ObjectKeyFromObject(want), &got)
	if apierrors.IsNotFound(err) {
		return client.IgnoreAlreadyExists(r.Create(ctx, want))
	}
	return err
}

// ensureTarget points the stand-in's shim at the launcher pod IP.
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
	if ip == "" || pod.Annotations[podmode.AnnotationTarget] == ip {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[podmode.AnnotationTarget] = ip
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
