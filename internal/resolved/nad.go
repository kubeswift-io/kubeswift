package resolved

import (
	"context"
	"encoding/json"
	"net/netip"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// networkAttachmentDefinitionGVK identifies the Multus NAD CRD for an unstructured
// Get (the type is not registered in the scheme).
var networkAttachmentDefinitionGVK = schema.GroupVersionKind{
	Group:   "k8s.cni.cncf.io",
	Version: "v1",
	Kind:    "NetworkAttachmentDefinition",
}

// nadIPAM is the subset of the IPAM configs that carry a gateway: whereabouts and
// host-local (top-level gateway), host-local ranges, and static addresses.
type nadIPAM struct {
	Gateway string `json:"gateway"`
	Ranges  [][]struct {
		Gateway string `json:"gateway"`
	} `json:"ranges"`
	Addresses []struct {
		Gateway string `json:"gateway"`
	} `json:"addresses"`
}

// nadPluginConf is the part of a CNI plugin config that holds the IPAM block.
type nadPluginConf struct {
	IPAM *nadIPAM `json:"ipam"`
}

// nadConf is a NAD spec.config: a single plugin config, or a conflist under plugins.
type nadConf struct {
	nadPluginConf
	Plugins []nadPluginConf `json:"plugins"`
}

// NADGateway returns the gateway declared in the IPAM config of the NAD ref names,
// or "" when it declares no IPv4 one. The launcher hands a primary-on-NAD guest the
// default route of its Multus interface as router, and an IPAM like whereabouts
// installs no route unless told to; this gateway is the fallback for that case.
// A missing NAD or NAD CRD yields "" (the pod cannot start without the NAD
// anyway, and resolution must not fail on clusters without Multus).
func NADGateway(ctx context.Context, c client.Client, namespace string, ref *swiftv1alpha1.NetworkReference) (string, error) {
	if ref.Namespace != "" {
		namespace = ref.Namespace
	}
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(networkAttachmentDefinitionGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, nad); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return "", nil
		}
		return "", err
	}
	cfgStr, _, _ := unstructured.NestedString(nad.Object, "spec", "config")
	var cfg nadConf
	if cfgStr == "" || json.Unmarshal([]byte(cfgStr), &cfg) != nil {
		return "", nil
	}
	if gw := ipamGateway(cfg.IPAM); gw != "" {
		return gw, nil
	}
	for _, p := range cfg.Plugins {
		if gw := ipamGateway(p.IPAM); gw != "" {
			return gw, nil
		}
	}
	return "", nil
}

// ipamGateway returns the first IPv4 gateway an IPAM config declares.
func ipamGateway(ipam *nadIPAM) string {
	if ipam == nil {
		return ""
	}
	if gw := ipv4Gateway(ipam.Gateway); gw != "" {
		return gw
	}
	for _, set := range ipam.Ranges {
		for _, r := range set {
			if gw := ipv4Gateway(r.Gateway); gw != "" {
				return gw
			}
		}
	}
	for _, a := range ipam.Addresses {
		if gw := ipv4Gateway(a.Gateway); gw != "" {
			return gw
		}
	}
	return ""
}

// ipv4Gateway returns gw in canonical form when it is a plain IPv4 address, else "".
// The NAD config is user-controlled and the gateway ends up in privileged shell code.
func ipv4Gateway(gw string) string {
	addr, err := netip.ParseAddr(gw)
	if err != nil || !addr.Is4() {
		return ""
	}
	return addr.String()
}
