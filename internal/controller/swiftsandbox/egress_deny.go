package swiftsandbox

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// Platform egress deny list for `restricted` sandboxes (WS2e).
//
// restricted mode drops the metadata range and the private ranges, but a
// cluster whose nodes have public addresses left the kubelet (:10250) and the
// API server (:6443) reachable from the guest. The controller therefore
// resolves every node's InternalIP and ExternalIP, the API server's own
// endpoint addresses (on a distribution whose control plane is not a Node,
// blocking node addresses alone leaves :6443 open), and an admin-set list
// (chart sandboxNetwork.restrictedDeny), and network-init drops them before
// spec.network.egress.allow: a tenant cannot open them. The rules see a
// packet's destination before kube-proxy translates it on the node, so the
// list also holds the frontends (ClusterIPs, externalIPs, load-balancer IPs)
// of every Service with an endpoint in that set, and an allow rule naming such
// a Service waits (EgressServiceReachesNode).
//
// The list reaches the launcher as a pod annotation, exposed to network-init
// and swiftletd as a downward-API file. network-init applies it at boot;
// swiftletd re-applies it when kubelet refreshes the file, so a node that
// joins later is blocked in sandboxes that already run. The controller
// recomputes the list every EgressDenyInterval and patches the pods whose
// copy is stale.

const (
	// EgressDenyAnnotation carries the platform deny list on a restricted
	// launcher pod: IPv4 CIDRs, space-separated, sorted. Present (possibly
	// empty) on every restricted launcher pod this controller builds.
	EgressDenyAnnotation = "sandbox.kubeswift.io/egress-deny"
	egressDenyVolume     = "egress-deny"
	networkInitName      = "network-init" // swiftguest.NetworkInitContainer's name
	// EgressDenyDir is where network-init and swiftletd read the list, as the
	// file EgressDenyFile.
	EgressDenyDir  = "/etc/kubeswift/egress-deny"
	EgressDenyFile = "deny"
	// EgressDenyInterval is how often the controller recomputes the list.
	EgressDenyInterval = 30 * time.Second
)

// ExtraEgressDeny is the admin-set part of the list (chart
// sandboxNetwork.restrictedDeny), already normalized by ParseEgressDeny.
var ExtraEgressDeny []string

// currentEgressDeny is the last computed list; nil until the first compute.
var currentEgressDeny atomic.Pointer[string]

// currentEgressBase is the last computed list without the forwarded Service
// addresses: what a Service's endpoints must not be in. nil until the first
// compute.
var currentEgressBase atomic.Pointer[[]netip.Prefix]

// egressBase is currentEgressBase, or the admin-set part alone before the
// first compute.
func egressBase() []netip.Prefix {
	if v := currentEgressBase.Load(); v != nil {
		return *v
	}
	return denyPrefixes(strings.Join(ExtraEgressDeny, " "))
}

// egressDenyValue is the list a new restricted launcher pod gets: the last
// computed one, or the admin-set part alone before the first compute (the
// refresher patches the pod on its next pass).
func egressDenyValue() string {
	if v := currentEgressDeny.Load(); v != nil {
		return *v
	}
	return strings.Join(ExtraEgressDeny, " ")
}

// ParseEgressDeny parses a comma- or space-separated list of IPv4 addresses
// and CIDRs into normalized CIDRs. IPv6 is refused: restricted mode drops all
// IPv6 forwarding already, so an IPv6 entry would be a no-op that looks like a
// policy.
func ParseEgressDeny(s string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		p, err := parseIPv4Prefix(f)
		if err != nil {
			return nil, err
		}
		out = append(out, p.String())
	}
	return out, nil
}

func parseIPv4Prefix(s string) (netip.Prefix, error) {
	var p netip.Prefix
	if strings.Contains(s, "/") {
		var err error
		if p, err = netip.ParsePrefix(s); err != nil {
			return p, fmt.Errorf("egress deny entry %q is not an IPv4 address or CIDR", s)
		}
	} else {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return p, fmt.Errorf("egress deny entry %q is not an IPv4 address or CIDR", s)
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if !p.Addr().Is4() {
		return p, fmt.Errorf("egress deny entry %q is not IPv4 (restricted mode drops all IPv6 forwarding)", s)
	}
	return p.Masked(), nil
}

// computeEgressDeny renders the list: every node's InternalIP and
// ExternalIP, the API server endpoint addresses, and extra. IPv6 addresses
// are skipped (see ParseEgressDeny).
func computeEgressDeny(nodes []corev1.Node, apiserver *discoveryv1.EndpointSlice, extra []string) string {
	set := map[string]bool{}
	add := func(s string) {
		if p, err := parseIPv4Prefix(s); err == nil {
			set[p.String()] = true
		}
	}
	for _, n := range nodes {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
				add(a.Address)
			}
		}
	}
	if apiserver != nil {
		for _, e := range apiserver.Endpoints {
			for _, a := range e.Addresses {
				add(a)
			}
		}
	}
	for _, e := range extra {
		add(e)
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// denyPrefixes parses a computed list (space-separated, already normalized).
func denyPrefixes(list string) []netip.Prefix {
	var out []netip.Prefix
	for _, f := range strings.Fields(list) {
		if p, err := parseIPv4Prefix(f); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// deniedAddress returns the first IPv4 endpoint address of slices that falls
// in deny, or "".
func deniedAddress(slices []discoveryv1.EndpointSlice, deny []netip.Prefix) string {
	for _, sl := range slices {
		for _, e := range sl.Endpoints {
			for _, a := range e.Addresses {
				ip, err := netip.ParseAddr(a)
				if err != nil || !ip.Is4() {
					continue
				}
				for _, p := range deny {
					if p.Contains(ip) {
						return a
					}
				}
			}
		}
	}
	return ""
}

// serviceFrontends is every IPv4 address a pod can send to that kube-proxy
// forwards to the Service's endpoints: its ClusterIPs, spec.externalIPs and
// load-balancer ingress IPs.
func serviceFrontends(svc *corev1.Service) []string {
	out := ipv4ClusterIPs(svc)
	ips := append([]string{}, svc.Spec.ExternalIPs...)
	for _, in := range svc.Status.LoadBalancer.Ingress {
		ips = append(ips, in.IP)
	}
	for _, s := range ips {
		if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
			out = append(out, a.String())
		}
	}
	return out
}

// forwardedFrontends returns, as /32 CIDRs, the frontends of every Service
// with an endpoint in deny. The launcher's rules see a packet's destination
// before kube-proxy translates it on the node, so a Service in front of a
// node or the API server would otherwise reach them: through an allowed
// ClusterIP, or through an externalIP or load-balancer IP, which restricted
// mode treats as public.
func forwardedFrontends(svcs []corev1.Service, slices []discoveryv1.EndpointSlice, deny []netip.Prefix) []string {
	if len(deny) == 0 {
		return nil
	}
	byService := map[types.NamespacedName][]discoveryv1.EndpointSlice{}
	for _, sl := range slices {
		k := types.NamespacedName{Namespace: sl.Namespace, Name: sl.Labels[discoveryv1.LabelServiceName]}
		byService[k] = append(byService[k], sl)
	}
	var out []string
	for i := range svcs {
		svc := &svcs[i]
		if deniedAddress(byService[types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}], deny) == "" {
			continue
		}
		for _, ip := range serviceFrontends(svc) {
			out = append(out, ip+"/32")
		}
	}
	return out
}

// mergeDeny adds extra CIDRs to a computed list, keeping it sorted and unique.
func mergeDeny(list string, extra []string) string {
	set := map[string]bool{}
	for _, f := range strings.Fields(list) {
		set[f] = true
	}
	for _, e := range extra {
		if p, err := parseIPv4Prefix(e); err == nil {
			set[p.String()] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// restrictedLauncher is whether buildPod gives the sandbox the deny list:
// mode restricted (for the guest's forwarded traffic) and modes proxied and
// brokered (for the broker's own connections).
func restrictedLauncher(sb *sandboxv1alpha1.SwiftSandbox) bool {
	m := egressMode(sb)
	return networked(sb) && (m == sandboxv1alpha1.SandboxNetworkRestricted || m.EgressMediated())
}

// addEgressDeny stamps the list on a restricted launcher pod and mounts it,
// as a downward-API file, into network-init and the launcher.
func addEgressDeny(pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) {
	if !restrictedLauncher(sb) {
		return
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[EgressDenyAnnotation] = egressDenyValue()
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: egressDenyVolume,
		VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{
			Items: []corev1.DownwardAPIVolumeFile{{
				Path:     EgressDenyFile,
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.annotations['" + EgressDenyAnnotation + "']"},
			}},
		}},
	})
	m := corev1.VolumeMount{Name: egressDenyVolume, MountPath: EgressDenyDir, ReadOnly: true}
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == networkInitName {
			pod.Spec.InitContainers[i].VolumeMounts = append(pod.Spec.InitContainers[i].VolumeMounts, m)
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == launcherName {
			pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, m)
		}
	}
}

// EgressDenyRefresher recomputes the list and patches stale launcher pods.
type EgressDenyRefresher struct {
	// Client reads Nodes and Pods (cached) and patches Pods.
	Client client.Client
	// APIReader reads the API server's EndpointSlice (not cached: one object
	// in a namespace the controller does not otherwise watch).
	APIReader client.Reader
	Interval  time.Duration
}

// NeedLeaderElection: only the leader patches pods.
func (r *EgressDenyRefresher) NeedLeaderElection() bool { return true }

// Start runs until ctx ends.
func (r *EgressDenyRefresher) Start(ctx context.Context) error {
	iv := r.Interval
	if iv <= 0 {
		iv = EgressDenyInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if err := r.refresh(ctx); err != nil {
			klog.ErrorS(err, "egress deny list refresh failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (r *EgressDenyRefresher) refresh(ctx context.Context) error {
	var nodes corev1.NodeList
	if err := r.Client.List(ctx, &nodes); err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	var slice *discoveryv1.EndpointSlice
	es := &discoveryv1.EndpointSlice{}
	switch err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: "default", Name: "kubernetes"}, es); {
	case err == nil:
		slice = es
	case apierrors.IsNotFound(err):
	default:
		// Keep the previous list rather than drop the API server from it.
		return fmt.Errorf("get the API server EndpointSlice: %w", err)
	}
	base := computeEgressDeny(nodes.Items, slice, ExtraEgressDeny)
	basePrefixes := denyPrefixes(base)
	var svcs corev1.ServiceList
	if err := r.Client.List(ctx, &svcs); err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	var slices discoveryv1.EndpointSliceList
	if err := r.Client.List(ctx, &slices, client.HasLabels{discoveryv1.LabelServiceName}); err != nil {
		return fmt.Errorf("list endpointslices: %w", err)
	}
	want := mergeDeny(base, forwardedFrontends(svcs.Items, slices.Items, basePrefixes))
	if old := currentEgressDeny.Load(); old == nil || *old != want {
		klog.InfoS("egress deny list", "entries", len(strings.Fields(want)))
	}
	currentEgressBase.Store(&basePrefixes)
	currentEgressDeny.Store(&want)

	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.HasLabels{SandboxLabelKey}); err != nil {
		return fmt.Errorf("list sandbox launcher pods: %w", err)
	}
	var firstErr error
	for i := range pods.Items {
		p := &pods.Items[i]
		have, ok := p.Annotations[EgressDenyAnnotation]
		if !ok || have == want || p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		patch := client.MergeFrom(p.DeepCopy())
		p.Annotations[EgressDenyAnnotation] = want
		if err := r.Client.Patch(ctx, p, patch); err != nil && !apierrors.IsNotFound(err) && firstErr == nil {
			firstErr = fmt.Errorf("patch %s/%s: %w", p.Namespace, p.Name, err)
		}
	}
	return firstErr
}
