package swiftguest

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

// exposedServiceName is the per-guest Service name. One Service per guest carries
// all exposed ports (matches `virtctl expose`; design §3, §5).
func exposedServiceName(guest *swiftv1alpha1.SwiftGuest) string {
	return guest.Name + "-svc"
}

// exposedPorts returns the spec.network.ports[] that have Expose set (the ports
// that enter the Service). A nat-bound guest only; bridge-bound and exposeless
// ports are DNAT-only / NetworkPolicy targeting and never minted as a Service.
func exposedPorts(guest *swiftv1alpha1.SwiftGuest) []swiftv1alpha1.GuestPort {
	if guest.Spec.Network == nil || guest.Spec.Network.Binding == "bridge" {
		return nil
	}
	var out []swiftv1alpha1.GuestPort
	for _, p := range guest.Spec.Network.Ports {
		if p.Expose != "" {
			out = append(out, p)
		}
	}
	return out
}

// ensureExposedService mints, updates, or GCs the per-guest Service. A nat-bound
// guest's exposed port lands on the pod IP via the in-pod DNAT (network-init.sh),
// so a plain selector Service on swift.kubeswift.io/guest makes the VM a normal
// Kubernetes endpoint (design §5). The Service is owned by the SwiftGuest, so it
// is garbage-collected on guest delete. Returns the Service name ("" when none).
func (r *SwiftGuestReconciler) ensureExposedService(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (string, error) {
	name := exposedServiceName(guest)
	ports := exposedPorts(guest)

	if len(ports) == 0 {
		// No exposure (or it was removed): delete a previously-minted Service.
		var existing corev1.Service
		err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: name}, &existing)
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("get Service %q: %w", name, err)
		}
		// Only delete a Service we own (defensive — never touch a foreign object).
		if !metav1.IsControlledBy(&existing, guest) {
			return "", nil
		}
		if err := r.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("delete Service %q: %w", name, err)
		}
		return "", nil
	}

	// The Service type is the (webhook-validated consistent) Expose value of the
	// exposed ports.
	svcType := corev1.ServiceType(ports[0].Expose)
	svcPorts := make([]corev1.ServicePort, 0, len(ports))
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		svcPorts = append(svcPorts, corev1.ServicePort{
			Name:     p.Name,
			Port:     p.Port,
			Protocol: proto,
			// Service -> pod port (p.Port); the in-pod DNAT then forwards
			// pod:p.Port -> vm:p.TargetPort. So the Service targetPort is the
			// pod-side port, not the in-guest port.
			TargetPort: intstr.FromInt32(p.Port),
		})
	}

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kubeswift",
				"app.kubernetes.io/component":  "guest",
				"app.kubernetes.io/managed-by": "kubeswift-controller-manager",
				guestPodLabelKey:               guest.Name,
			},
			Annotations: serviceAnnotations(guest.Spec.Network),
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: map[string]string{guestPodLabelKey: guest.Name},
			Ports:    svcPorts,
		},
	}
	// LoadBalancerClass selects the LB implementation (Tailscale, MetalLB, ...).
	// Only valid with type=LoadBalancer; the apiserver rejects it otherwise.
	if svcType == corev1.ServiceTypeLoadBalancer && guest.Spec.Network != nil {
		desired.Spec.LoadBalancerClass = guest.Spec.Network.LoadBalancerClass
	}
	if err := controllerutil.SetControllerReference(guest, desired, r.Scheme); err != nil {
		return "", fmt.Errorf("set ownerRef on Service %q: %w", name, err)
	}

	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: name}, &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create Service %q: %w", name, err)
		}
		return name, nil
	}
	if err != nil {
		return "", fmt.Errorf("get Service %q: %w", name, err)
	}

	// Update in place if the controller-owned fields drifted (ports/type/selector/
	// our annotations). Preserve apiserver-assigned fields (clusterIP, nodePorts)
	// and foreign annotations (MetalLB/cloud-LB status) by overlaying, not
	// replacing. loadBalancerClass is immutable post-create, so it is set only on
	// Create — a change requires recreating the Service (documented).
	if existing.Spec.Type != desired.Spec.Type ||
		!equality.Semantic.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) ||
		!servicePortsEqual(existing.Spec.Ports, desired.Spec.Ports) ||
		annotationsNeedOverlay(existing.Annotations, desired.Annotations) {
		existing.Spec.Type = desired.Spec.Type
		existing.Spec.Selector = desired.Spec.Selector
		existing.Spec.Ports = desired.Spec.Ports
		existing.Annotations = overlayAnnotations(existing.Annotations, desired.Annotations)
		if err := r.Update(ctx, &existing); err != nil {
			return "", fmt.Errorf("update Service %q: %w", name, err)
		}
	}
	return name, nil
}

// serviceAnnotations returns the operator-provided annotations to stamp on the
// exposed Service (nil-safe copy).
func serviceAnnotations(net *swiftv1alpha1.GuestNetworkSpec) map[string]string {
	if net == nil || len(net.ServiceAnnotations) == 0 {
		return nil
	}
	out := make(map[string]string, len(net.ServiceAnnotations))
	for k, v := range net.ServiceAnnotations {
		out[k] = v
	}
	return out
}

// annotationsNeedOverlay reports whether any desired annotation is missing or
// differs in existing (we overlay rather than replace, so foreign annotations
// added by other controllers are never clobbered; removing a key from spec
// therefore needs a manual cleanup — documented).
func annotationsNeedOverlay(existing, desired map[string]string) bool {
	for k, v := range desired {
		if existing[k] != v {
			return true
		}
	}
	return false
}

// overlayAnnotations returns existing with desired keys overlaid (preserving
// foreign keys).
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

// isLauncherReady reports whether the launcher pod's Ready condition is true.
// With the exposed-port readiness probe (pod.go::applyExposedPorts) this means
// the in-guest service is up — the honest ServiceReady signal.
func isLauncherReady(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// servicePortsEqual compares the controller-owned port fields, ignoring
// apiserver-assigned NodePort values.
func servicePortsEqual(a, b []corev1.ServicePort) bool {
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

// setExposedPortsStatus reflects the exposed ports + Service into status.network
// and sets the PortsProgrammed/ServiceReady conditions. No silent failures: the
// status echoes exactly what was programmed.
func setExposedPortsStatus(guest *swiftv1alpha1.SwiftGuest, status *swiftv1alpha1.SwiftGuestStatus, svcName string, podReady bool) {
	if guest.Spec.Network == nil || len(guest.Spec.Network.Ports) == 0 {
		return
	}
	if status.Network == nil {
		status.Network = &swiftv1alpha1.GuestNetworkStatus{}
	}
	echoed := make([]swiftv1alpha1.ExposedPortStatus, 0, len(guest.Spec.Network.Ports))
	for _, p := range guest.Spec.Network.Ports {
		target := p.TargetPort
		if target == 0 {
			target = p.Port
		}
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		echoed = append(echoed, swiftv1alpha1.ExposedPortStatus{
			Name:       p.Name,
			Port:       p.Port,
			TargetPort: target,
			Protocol:   proto,
		})
	}
	status.Network.ExposedPorts = echoed
	if svcName != "" {
		status.Network.ServiceRef = &corev1.LocalObjectReference{Name: svcName}
	} else {
		status.Network.ServiceRef = nil
	}

	// PortsProgrammed: the in-pod DNAT is installed by the network-init init
	// container, so a Running guest (past init) has its ports programmed.
	progressed := status.Phase == swiftv1alpha1.SwiftGuestPhaseRunning
	setNetworkCondition(status, swiftv1alpha1.ConditionPortsProgrammed, progressed,
		"PortsProgrammed", "DNAT rules installed by network-init")

	// ServiceReady: a Service exists and its endpoint (the launcher pod) is Ready
	// — which, via the launcher readiness probe, means the in-guest service is up.
	if svcName != "" {
		setNetworkCondition(status, swiftv1alpha1.ConditionServiceReady, podReady,
			"ServiceReady", "Service endpoint is Ready")
	}
}

func setNetworkCondition(status *swiftv1alpha1.SwiftGuestStatus, condType string, ok bool, reason, msg string) {
	st := metav1.ConditionFalse
	if ok {
		st = metav1.ConditionTrue
	}
	cond := metav1.Condition{Type: condType, Status: st, Reason: reason, Message: msg}
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			if status.Conditions[i].Status != st {
				cond.LastTransitionTime = metav1.Now()
			} else {
				cond.LastTransitionTime = status.Conditions[i].LastTransitionTime
			}
			status.Conditions[i] = cond
			return
		}
	}
	cond.LastTransitionTime = metav1.Now()
	status.Conditions = append(status.Conditions, cond)
}
