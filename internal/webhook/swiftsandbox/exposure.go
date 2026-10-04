package swiftsandbox

import (
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// validateExposure checks spec.network.ports and spec.network.ingress.
func validateExposure(n sandboxv1alpha1.SandboxNetwork) error {
	if len(n.Ports) == 0 {
		if n.Ingress != nil {
			return fmt.Errorf("spec.network.ingress needs spec.network.ports: it narrows who may reach them")
		}
		return nil
	}
	if n.Mode == sandboxv1alpha1.SandboxNetworkNone {
		return fmt.Errorf("spec.network.ports needs a network: mode none has none")
	}
	if len(n.Ports) > 16 {
		return fmt.Errorf("spec.network.ports has %d entries, at most 16", len(n.Ports))
	}
	names, numbers := map[string]bool{}, map[int32]bool{}
	for i, p := range n.Ports {
		at := fmt.Sprintf("spec.network.ports[%d]", i)
		if errs := validation.IsValidPortName(p.Name); len(errs) > 0 {
			return fmt.Errorf("%s.name %q: %s", at, p.Name, errs[0])
		}
		if p.Port < 1 || p.Port > 65535 {
			return fmt.Errorf("%s.port %d is out of range 1-65535", at, p.Port)
		}
		if p.Protocol != "" && p.Protocol != corev1.ProtocolTCP {
			return fmt.Errorf("%s.protocol %s: only TCP is supported", at, p.Protocol)
		}
		if names[p.Name] {
			return fmt.Errorf("%s.name %q is declared twice", at, p.Name)
		}
		if numbers[p.Port] {
			return fmt.Errorf("%s.port %d is declared twice", at, p.Port)
		}
		names[p.Name], numbers[p.Port] = true, true
	}
	if n.Ingress != nil {
		if len(n.Ingress.From) > 16 {
			return fmt.Errorf("spec.network.ingress.from has %d entries, at most 16", len(n.Ingress.From))
		}
		for i, peer := range n.Ingress.From {
			if err := validatePeer(peer); err != nil {
				return fmt.Errorf("spec.network.ingress.from[%d]: %w", i, err)
			}
		}
	}
	return nil
}

// validatePeer applies the NetworkPolicy peer rules, so the policy the
// controller writes cannot be refused by the API server.
func validatePeer(p networkingv1.NetworkPolicyPeer) error {
	opts := metav1validation.LabelSelectorValidationOptions{}
	if p.PodSelector != nil {
		if errs := metav1validation.ValidateLabelSelector(p.PodSelector, opts, field.NewPath("podSelector")); len(errs) > 0 {
			return errs.ToAggregate()
		}
	}
	if p.NamespaceSelector != nil {
		if errs := metav1validation.ValidateLabelSelector(p.NamespaceSelector, opts, field.NewPath("namespaceSelector")); len(errs) > 0 {
			return errs.ToAggregate()
		}
	}
	if p.IPBlock == nil {
		if p.PodSelector == nil && p.NamespaceSelector == nil {
			return fmt.Errorf("set podSelector, namespaceSelector or ipBlock")
		}
		return nil
	}
	if p.PodSelector != nil || p.NamespaceSelector != nil {
		return fmt.Errorf("ipBlock cannot be combined with podSelector or namespaceSelector")
	}
	_, block, err := net.ParseCIDR(p.IPBlock.CIDR)
	if err != nil {
		return fmt.Errorf("ipBlock.cidr %q is not a CIDR", p.IPBlock.CIDR)
	}
	for _, e := range p.IPBlock.Except {
		ip, except, err := net.ParseCIDR(e)
		if err != nil {
			return fmt.Errorf("ipBlock.except %q is not a CIDR", e)
		}
		eOnes, _ := except.Mask.Size()
		bOnes, _ := block.Mask.Size()
		if !block.Contains(ip) || eOnes <= bOnes {
			return fmt.Errorf("ipBlock.except %q must be a strict subset of %s", e, p.IPBlock.CIDR)
		}
	}
	return nil
}

// reservedDomain reports whether a label or annotation key belongs to
// KubeSwift (kubeswift.io or any *.kubeswift.io prefix).
func reservedDomain(key string) bool {
	prefix, _, ok := strings.Cut(key, "/")
	return ok && (prefix == "kubeswift.io" || strings.HasSuffix(prefix, ".kubeswift.io"))
}

// podNetworkAnnotationPrefixes select or configure a pod's networks; KubeSwift
// wires them on its launchers itself.
var podNetworkAnnotationPrefixes = []string{"k8s.v1.cni.cncf.io/", "v1.multus-cni.io/", "k8s.ovn.org/"}

// ValidatePodMetadata checks spec.podMetadata. The controller runs it too:
// these keys land on a privileged launcher pod.
func ValidatePodMetadata(m *sandboxv1alpha1.SandboxPodMetadata) error {
	if m == nil {
		return nil
	}
	if errs := metav1validation.ValidateLabels(m.Labels, field.NewPath("spec", "podMetadata", "labels")); len(errs) > 0 {
		return errs.ToAggregate()
	}
	if errs := apivalidation.ValidateAnnotations(m.Annotations, field.NewPath("spec", "podMetadata", "annotations")); len(errs) > 0 {
		return errs.ToAggregate()
	}
	for k := range m.Labels {
		if reservedDomain(k) {
			return fmt.Errorf("spec.podMetadata.labels[%s]: keys under kubeswift.io are KubeSwift's own", k)
		}
	}
	for k := range m.Annotations {
		if reservedDomain(k) {
			return fmt.Errorf("spec.podMetadata.annotations[%s]: keys under kubeswift.io are KubeSwift's own", k)
		}
		for _, p := range podNetworkAnnotationPrefixes {
			if strings.HasPrefix(k, p) {
				return fmt.Errorf("spec.podMetadata.annotations[%s]: pod-network annotations are KubeSwift's to set on a launcher", k)
			}
		}
	}
	return nil
}
