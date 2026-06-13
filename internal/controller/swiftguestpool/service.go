package swiftguestpool

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// poolServiceName is the load-balanced Service fronting a pool's replicas.
func poolServiceName(pool *swiftv1alpha1.SwiftGuestPool) string {
	return pool.Name + "-svc"
}

// poolServiceResult carries what updateStatus needs to stamp status.serviceRef
// and the ServiceReady condition in its single status patch.
type poolServiceResult struct {
	name    string // "" when no Service is configured / minted
	ready   bool
	reason  string
	message string
}

// poolPrimaryBinding returns the template's primary nat/bridge binding (the
// default is nat). A pool Service only works with nat — see reconcilePoolService.
func poolPrimaryBinding(pool *swiftv1alpha1.SwiftGuestPool) string {
	net := pool.Spec.Template.Spec.Network
	if net != nil && net.Binding == "bridge" {
		return "bridge"
	}
	return "nat"
}

// reconcilePoolService mints, updates, or GCs the pool's one load-balanced
// Service (service exposure §6). It is a plain selector Service on the pool
// label: every replica pod carries that label (pod.go::podLabels) and installs
// the in-pod DNAT for the injected ports (createSwiftGuest), so Kubernetes
// endpoint management load-balances across the replicas with honest readiness
// (the launcher readiness probe gates each pod). No EndpointSlice bookkeeping;
// the pool's scale subresource is the HPA seam. Owned by the pool -> GC'd on
// pool delete. readyReplicas is the GuestRunning count (for the condition).
func (r *SwiftGuestPoolReconciler) reconcilePoolService(ctx context.Context, pool *swiftv1alpha1.SwiftGuestPool, readyReplicas int32) (poolServiceResult, error) {
	name := poolServiceName(pool)

	// Not configured (or removed): GC any Service we own.
	if pool.Spec.Service == nil {
		return poolServiceResult{}, r.deleteOwnedPoolService(ctx, pool, name)
	}

	// A pool Service needs the in-pod DNAT, which only a nat-bound primary
	// installs. A bridge-bound template reaches the NAD IP directly (no DNAT),
	// so a selector Service would route podIP:port to a dead end. No silent
	// failure: GC the Service and surface why via the ServiceReady condition.
	if poolPrimaryBinding(pool) == "bridge" {
		if err := r.deleteOwnedPoolService(ctx, pool, name); err != nil {
			return poolServiceResult{}, err
		}
		return poolServiceResult{
			reason:  "BridgeBindingUnsupported",
			message: "spec.service requires a nat-bound template (a bridge primary reaches the NAD IP, not the pod IP, so no in-pod DNAT is installed)",
		}, nil
	}

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pool.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kubeswift",
				"app.kubernetes.io/component":  "guestpool",
				"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
				swiftv1alpha1.LabelPoolName:    pool.Name,
			},
			Annotations: copyAnnotations(pool.Spec.Service.Annotations),
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{swiftv1alpha1.LabelPoolName: pool.Name},
			Ports:    poolServicePorts(pool.Spec.Service.Ports),
		},
	}
	if pool.Spec.Service.Headless {
		// Headless: DNS returns one A record per ready replica pod (client-side
		// LB / sharded workloads). clusterIP is immutable post-create, so a
		// later headless toggle requires recreating the Service (documented).
		desired.Spec.Type = corev1.ServiceTypeClusterIP
		desired.Spec.ClusterIP = corev1.ClusterIPNone
	} else {
		t := pool.Spec.Service.Type
		if t == "" {
			t = string(corev1.ServiceTypeClusterIP)
		}
		desired.Spec.Type = corev1.ServiceType(t)
		// LoadBalancerClass (Tailscale/MetalLB/...) is immutable post-create, so
		// set it only on the desired object; the apiserver rejects it on a
		// non-LoadBalancer Service.
		if desired.Spec.Type == corev1.ServiceTypeLoadBalancer {
			desired.Spec.LoadBalancerClass = pool.Spec.Service.LoadBalancerClass
		}
	}
	if err := controllerutil.SetControllerReference(pool, desired, r.Client.Scheme()); err != nil {
		return poolServiceResult{}, fmt.Errorf("set ownerRef on Service %q: %w", name, err)
	}

	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: name}, &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return poolServiceResult{}, fmt.Errorf("create Service %q: %w", name, err)
		}
		return poolServiceReadyResult(name, readyReplicas), nil
	}
	if err != nil {
		return poolServiceResult{}, fmt.Errorf("get Service %q: %w", name, err)
	}

	// Update in place if controller-owned fields drifted (ports/type/selector/
	// our annotations). Preserve apiserver-assigned fields (clusterIP, nodePorts)
	// and foreign annotations (overlay, not replace). loadBalancerClass is
	// immutable post-create (set only on Create).
	if existing.Spec.Type != desired.Spec.Type ||
		!equality.Semantic.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) ||
		!poolServicePortsEqual(existing.Spec.Ports, desired.Spec.Ports) ||
		annotationsNeedOverlay(existing.Annotations, desired.Annotations) {
		existing.Spec.Type = desired.Spec.Type
		existing.Spec.Selector = desired.Spec.Selector
		existing.Spec.Ports = desired.Spec.Ports
		existing.Annotations = overlayAnnotations(existing.Annotations, desired.Annotations)
		if err := r.Update(ctx, &existing); err != nil {
			return poolServiceResult{}, fmt.Errorf("update Service %q: %w", name, err)
		}
	}
	return poolServiceReadyResult(name, readyReplicas), nil
}

// copyAnnotations returns a nil-safe copy of the operator-provided annotations.
func copyAnnotations(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// annotationsNeedOverlay reports whether any desired annotation is missing or
// differs in existing (overlay semantics — foreign annotations are preserved).
func annotationsNeedOverlay(existing, desired map[string]string) bool {
	for k, v := range desired {
		if existing[k] != v {
			return true
		}
	}
	return false
}

// overlayAnnotations returns existing with desired keys overlaid (preserving
// foreign keys; removing a key from spec needs manual cleanup — documented).
func overlayAnnotations(existing, desired map[string]string) map[string]string {
	if len(desired) == 0 {
		return existing
	}
	if existing == nil {
		existing = make(map[string]string, len(desired))
	}
	for k, v := range desired {
		existing[k] = v
	}
	return existing
}

// poolServiceReadyResult reports ServiceReady from the GuestRunning replica
// count. Honest wording: a replica counted here is GuestRunning; its endpoint
// flips Ready only once the in-guest service answers the readiness probe.
func poolServiceReadyResult(name string, readyReplicas int32) poolServiceResult {
	if readyReplicas > 0 {
		return poolServiceResult{
			name: name, ready: true, reason: "ReplicasAvailable",
			message: fmt.Sprintf("%d replica(s) GuestRunning; endpoints become Ready as each in-guest service answers", readyReplicas),
		}
	}
	return poolServiceResult{
		name: name, ready: false, reason: "NoReadyReplicas",
		message: "no GuestRunning replicas backing the Service yet",
	}
}

// deleteOwnedPoolService deletes the pool Service iff we own it (never touches a
// foreign object of the same name).
func (r *SwiftGuestPoolReconciler) deleteOwnedPoolService(ctx context.Context, pool *swiftv1alpha1.SwiftGuestPool, name string) error {
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: name}, &existing)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Service %q: %w", name, err)
	}
	if !metav1.IsControlledBy(&existing, pool) {
		return nil
	}
	if err := r.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Service %q: %w", name, err)
	}
	return nil
}

// poolServicePorts maps the pool's declared ports to ServicePorts. The Service
// targetPort is the pod-side port (p.Port); the in-pod DNAT then forwards
// pod:p.Port -> vm:p.TargetPort, so the Service never sees the in-guest port.
func poolServicePorts(ports []swiftv1alpha1.GuestPort) []corev1.ServicePort {
	out := make([]corev1.ServicePort, 0, len(ports))
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		out = append(out, corev1.ServicePort{
			Name:       p.Name,
			Port:       p.Port,
			Protocol:   proto,
			TargetPort: intstr.FromInt32(p.Port),
		})
	}
	return out
}

// poolReplicaPorts copies the pool's service ports onto each replica with Expose
// cleared: the replica installs the DNAT + readiness probe but mints no
// per-replica Service. The pool owns the single load-balanced Service.
func poolReplicaPorts(ports []swiftv1alpha1.GuestPort) []swiftv1alpha1.GuestPort {
	out := make([]swiftv1alpha1.GuestPort, 0, len(ports))
	for _, p := range ports {
		q := p
		q.Expose = ""
		out = append(out, q)
	}
	return out
}

// poolServicePortsEqual compares controller-owned port fields, ignoring
// apiserver-assigned NodePort values.
func poolServicePortsEqual(a, b []corev1.ServicePort) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Port != b[i].Port ||
			a[i].Protocol != b[i].Protocol || a[i].TargetPort != b[i].TargetPort {
			return false
		}
	}
	return true
}
