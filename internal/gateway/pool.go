package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/kubeswift-io/kubeswift/api/fleet/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/actions"
)

// swiftGuestGVR, podGVR, swiftMigrationGVR, guestPodLabel, and launcherContainer
// are sourced from internal/actions — the single definition shared with
// swiftctl and the write-action primitives. The read plane (ListGuests /
// WatchGuests), console, and telemetry reference these gateway-local names; the
// values live in one place.
var (
	swiftGuestGVR     = actions.SwiftGuestGVR
	podGVR            = actions.PodGVR
	swiftMigrationGVR = actions.SwiftMigrationGVR
)

// nodeGVR is the core node resource — ListNodes lists it for the migrate picker.
// It is gateway-only (no write action touches nodes), so it stays local.
var nodeGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// swiftSandboxGVR / swiftSandboxPoolGVR are the MicroVM resources the sandbox
// read plane lists. Gateway-local (no shared write action references them yet).
var (
	swiftSandboxGVR     = schema.GroupVersionResource{Group: "sandbox.kubeswift.io", Version: "v1alpha1", Resource: "swiftsandboxes"}
	swiftSandboxPoolGVR = schema.GroupVersionResource{Group: "sandbox.kubeswift.io", Version: "v1alpha1", Resource: "swiftsandboxpools"}
)

// guestPodLabel ties a launcher pod to its SwiftGuest. The label (not the pod
// name) is the stable handle — a live-migrated guest's pod is <guest>-mig-<uid>.
const guestPodLabel = actions.GuestLabel

// launcherContainer is the swiftletd container in the (multi-container) launcher
// pod — the one holding the serial socket. The console exec must name it.
const launcherContainer = actions.LauncherContainer

// ClientPool watches the hub's fleet.Cluster registry and maintains a base REST
// config per member cluster, built from the member's credential Secret. It
// hands out per-request dynamic clients that impersonate the end user (D1). On
// each member upsert it probes the member (server version) and writes the
// Cluster status (Ready / Reachable / KubernetesVersion) so the UI's cluster
// selector reflects real health. It is a manager.Runnable.
type ClientPool struct {
	cache     ctrlcache.Cache
	hub       client.Client // reads credential Secrets, writes Cluster status
	namespace string

	// discoveryNamespaces are scanned on each member for an in-cluster
	// Prometheus when the Cluster's spec.prometheusEndpoint is empty. Empty
	// slice = discovery disabled (only explicit endpoints resolve).
	discoveryNamespaces []string

	mu      sync.RWMutex
	members map[string]*member
}

type member struct {
	name       string
	config     *rest.Config
	prometheus string
}

// Prometheus endpoint resolution reasons — the reason on the
// PrometheusEndpointResolved condition and the discriminator the telemetry
// plane's degrade-vs-serve decision is derived from.
const (
	prometheusReasonExplicit       = "Explicit"       // operator-set spec.prometheusEndpoint
	prometheusReasonDiscovered     = "Discovered"     // gateway found an in-cluster Prometheus
	prometheusReasonNotFound       = "NotFound"       // none found / discovery disabled / member down
	prometheusReasonDiscoveryError = "DiscoveryError" // the member API errored during discovery
)

// prometheusDefaultPort is the operated Service's web port when no named
// web/http port is present.
const prometheusDefaultPort int32 = 9090

// NewClientPool builds the pool over the hub cache + a read/write hub client.
// discoveryNamespaces are scanned per member for an in-cluster Prometheus when
// the member's spec.prometheusEndpoint is empty (nil/empty disables discovery).
func NewClientPool(c ctrlcache.Cache, hub client.Client, namespace string, discoveryNamespaces []string) *ClientPool {
	return &ClientPool{cache: c, hub: hub, namespace: namespace, discoveryNamespaces: discoveryNamespaces, members: map[string]*member{}}
}

// NeedLeaderElection keeps the pool running on every replica.
func (p *ClientPool) NeedLeaderElection() bool { return false }

// Start registers the fleet.Cluster informer handler and blocks until ctx done.
func (p *ClientPool) Start(ctx context.Context) error {
	inf, err := p.cache.GetInformer(ctx, &fleetv1alpha1.Cluster{})
	if err != nil {
		return err
	}
	if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { p.upsert(ctx, obj) },
		UpdateFunc: func(_, obj any) { p.upsert(ctx, obj) },
		DeleteFunc: func(obj any) { p.remove(obj) },
	}); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func (p *ClientPool) upsert(ctx context.Context, obj any) {
	c := extractCluster(obj)
	if c == nil || c.Namespace != p.namespace {
		return
	}
	cfg, err := p.buildConfig(ctx, c)
	if err != nil {
		p.mu.Lock()
		delete(p.members, c.Name)
		p.mu.Unlock()
		p.setStatus(ctx, c, false, "", err.Error(), "", prometheusReasonNotFound, "member credential invalid; telemetry endpoint unresolved")
		return
	}
	p.mu.Lock()
	p.members[c.Name] = &member{name: c.Name, config: cfg, prometheus: c.Spec.PrometheusEndpoint}
	p.mu.Unlock()

	// Probe reachability + version AND resolve the telemetry endpoint off the
	// informer goroutine so a slow or unreachable member never stalls the
	// shared handler.
	cl := c.DeepCopy()
	go func() {
		ver, verr := serverVersion(cfg)
		reachable := verr == nil
		reachMsg := ""
		if verr != nil {
			reachMsg = verr.Error()
		}
		endpoint, promReason, promMsg := p.resolvePrometheus(ctx, cfg, cl.Spec.PrometheusEndpoint, reachable)
		p.mu.Lock()
		if m := p.members[cl.Name]; m != nil {
			m.prometheus = endpoint
		}
		p.mu.Unlock()
		p.setStatus(ctx, cl, reachable, ver, reachMsg, endpoint, promReason, promMsg)
	}()
}

// resolvePrometheus determines the member's effective telemetry endpoint:
// an explicit spec.prometheusEndpoint always wins (reason Explicit); otherwise,
// when the member is reachable and discovery is enabled, it scans the member for
// an in-cluster Prometheus (reason Discovered); else empty (reason NotFound or
// DiscoveryError). It never guesses silently — the reason + message are surfaced
// on the Cluster's PrometheusEndpointResolved condition (Principle #6).
func (p *ClientPool) resolvePrometheus(ctx context.Context, cfg *rest.Config, specEndpoint string, reachable bool) (endpoint, reason, msg string) {
	if specEndpoint != "" {
		return specEndpoint, prometheusReasonExplicit, "operator-set spec.prometheusEndpoint"
	}
	if !reachable {
		return "", prometheusReasonNotFound, "member unreachable; Prometheus discovery skipped"
	}
	if len(p.discoveryNamespaces) == 0 {
		return "", prometheusReasonNotFound, "spec.prometheusEndpoint empty and Prometheus discovery is disabled"
	}
	cs, cerr := kubernetes.NewForConfig(cfg)
	if cerr != nil {
		return "", prometheusReasonDiscoveryError, fmt.Sprintf("Prometheus discovery client: %v", cerr)
	}
	ep, detail, derr := discoverPrometheus(ctx, cs, p.discoveryNamespaces)
	if derr != nil {
		return "", prometheusReasonDiscoveryError, fmt.Sprintf("Prometheus discovery failed: %v", derr)
	}
	if ep == "" {
		return "", prometheusReasonNotFound, fmt.Sprintf("no Prometheus Service found in namespaces %v", p.discoveryNamespaces)
	}
	return ep, prometheusReasonDiscovered, detail
}

// discoverPrometheus scans the given namespaces on the member (via the member
// credential in cfg) for a kube-prometheus-stack Prometheus Service — the
// governing "prometheus-operated" Service or any Service labeled
// app.kubernetes.io/name=prometheus — and returns its base URL. The pick is
// deterministic (namespace order, then Service name); when more than one is
// found the detail names the others so an operator can override via
// spec.prometheusEndpoint. Empty endpoint + nil error means none found.
func discoverPrometheus(ctx context.Context, cs kubernetes.Interface, namespaces []string) (endpoint, detail string, err error) {
	type cand struct {
		ns, name string
		port     int32
	}
	var cands []cand
	seen := map[string]bool{}
	add := func(s *corev1.Service) {
		key := s.Namespace + "/" + s.Name
		if seen[key] {
			return
		}
		seen[key] = true
		cands = append(cands, cand{ns: s.Namespace, name: s.Name, port: prometheusServicePort(s)})
	}
	for _, ns := range namespaces {
		// The operator's governing Service, stable across kps releases.
		if s, gerr := cs.CoreV1().Services(ns).Get(ctx, "prometheus-operated", metav1.GetOptions{}); gerr == nil {
			add(s)
		} else if !apierrors.IsNotFound(gerr) {
			return "", "", gerr
		}
		// Any Service the operator labels as a Prometheus (the *-kube-prometheus-
		// prometheus ClusterIP Service and the operated Service both carry it).
		list, lerr := cs.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=prometheus"})
		if lerr != nil {
			return "", "", lerr
		}
		for i := range list.Items {
			add(&list.Items[i])
		}
	}
	if len(cands) == 0 {
		return "", "", nil
	}
	nsIndex := map[string]int{}
	for i, ns := range namespaces {
		nsIndex[ns] = i
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if nsIndex[cands[i].ns] != nsIndex[cands[j].ns] {
			return nsIndex[cands[i].ns] < nsIndex[cands[j].ns]
		}
		return cands[i].name < cands[j].name
	})
	pick := cands[0]
	endpoint = fmt.Sprintf("http://%s.%s.svc:%d", pick.name, pick.ns, pick.port)
	detail = fmt.Sprintf("discovered %s.%s:%d", pick.name, pick.ns, pick.port)
	if len(cands) > 1 {
		others := make([]string, 0, len(cands)-1)
		for _, c := range cands[1:] {
			others = append(others, fmt.Sprintf("%s.%s", c.name, c.ns))
		}
		detail += fmt.Sprintf(" (also saw %s; set spec.prometheusEndpoint to override)", strings.Join(others, ", "))
	}
	return endpoint, detail, nil
}

// prometheusServicePort returns the Service's web/http port, preferring a named
// "web"/"http" port, then a 9090 port, else the 9090 default.
func prometheusServicePort(s *corev1.Service) int32 {
	for _, p := range s.Spec.Ports {
		if p.Name == "web" || p.Name == "http" {
			return p.Port
		}
	}
	for _, p := range s.Spec.Ports {
		if p.Port == prometheusDefaultPort {
			return p.Port
		}
	}
	return prometheusDefaultPort
}

func (p *ClientPool) remove(obj any) {
	if c := extractCluster(obj); c != nil {
		p.mu.Lock()
		delete(p.members, c.Name)
		p.mu.Unlock()
	}
}

// buildConfig builds the member's base REST config from its credential Secret:
// a `kubeconfig` key (preferred) or a `token` (+ optional `ca.crt`) paired with
// spec.server.
func (p *ClientPool) buildConfig(ctx context.Context, c *fleetv1alpha1.Cluster) (*rest.Config, error) {
	// local:true federates the hub's OWN cluster via the gateway pod's
	// in-cluster ServiceAccount — no credential Secret is read or persisted.
	if c.Spec.Local {
		if c.Spec.CredentialSecretRef != nil {
			return nil, errors.New("spec.local and spec.credentialSecretRef are mutually exclusive")
		}
		return rest.InClusterConfig()
	}
	if c.Spec.CredentialSecretRef == nil || c.Spec.CredentialSecretRef.Name == "" {
		return nil, errors.New("spec.credentialSecretRef.name is required (or set spec.local: true for the hub's own cluster)")
	}
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: c.Namespace, Name: c.Spec.CredentialSecretRef.Name}
	if err := p.hub.Get(ctx, key, &sec); err != nil {
		return nil, fmt.Errorf("read credential secret: %w", err)
	}
	if kc := sec.Data["kubeconfig"]; len(kc) > 0 {
		cfg, err := clientcmd.RESTConfigFromKubeConfig(kc)
		if err != nil {
			return nil, fmt.Errorf("parse kubeconfig: %w", err)
		}
		return cfg, nil
	}
	tok := sec.Data["token"]
	if len(tok) == 0 {
		return nil, errors.New("credential secret has neither a kubeconfig nor a token key")
	}
	if c.Spec.Server == "" {
		return nil, errors.New("spec.server is required when using a token credential")
	}
	cfg := &rest.Config{Host: c.Spec.Server, BearerToken: string(tok)}
	cfg.TLSClientConfig.Insecure = c.Spec.InsecureSkipTLSVerify
	if ca := sec.Data["ca.crt"]; len(ca) > 0 {
		cfg.TLSClientConfig.CAData = ca
	}
	return cfg, nil
}

// DynamicFor returns a dynamic client for the named member, impersonating the
// given identity (an empty identity uses the member's own credential).
func (p *ClientPool) DynamicFor(cluster string, id Identity) (dynamic.Interface, error) {
	cfg, err := p.RestConfigFor(cluster, id)
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(cfg)
}

// RestConfigFor returns the member's REST config, impersonating the identity
// (unless empty). The console plane needs the raw config for client-go's
// remotecommand exec; DynamicFor builds on it.
func (p *ClientPool) RestConfigFor(cluster string, id Identity) (*rest.Config, error) {
	p.mu.RLock()
	m, ok := p.members[cluster]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("cluster %q is not registered or not reachable", cluster)
	}
	cfg := rest.CopyConfig(m.config)
	if !id.empty() {
		cfg.Impersonate = rest.ImpersonationConfig{UserName: id.User, Groups: id.Groups}
	}
	return cfg, nil
}

// Members returns the names of all currently registered member clusters.
func (p *ClientPool) Members() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	names := make([]string, 0, len(p.members))
	for n := range p.members {
		names = append(names, n)
	}
	return names
}

// PrometheusEndpoint returns the member's registered Prometheus base URL, or ""
// if the cluster is unknown or has no prometheusEndpoint configured.
func (p *ClientPool) PrometheusEndpoint(cluster string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if m, ok := p.members[cluster]; ok {
		return m.prometheus
	}
	return ""
}

func serverVersion(cfg *rest.Config) (string, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return "", err
	}
	v, err := dc.ServerVersion()
	if err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

// setStatus best-effort patches the Cluster's Ready/Reachable conditions, the
// resolved telemetry endpoint (spec/discovered/none) + its
// PrometheusEndpointResolved condition, version, and lastConnected. It re-reads
// the object to avoid clobbering a concurrent writer; failures are swallowed
// (status is advisory).
func (p *ClientPool) setStatus(ctx context.Context, c *fleetv1alpha1.Cluster, reachable bool, version, msg, promEndpoint, promReason, promMsg string) {
	var cur fleetv1alpha1.Cluster
	if err := p.hub.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.Name}, &cur); err != nil {
		return
	}
	patch := client.MergeFrom(cur.DeepCopy())
	now := metav1.Now()
	setCondition(&cur.Status.Conditions, fleetv1alpha1.ClusterConditionReachable, reachable, "Reachable", "Unreachable", msg, now)
	setCondition(&cur.Status.Conditions, fleetv1alpha1.ClusterConditionReady, reachable, "Ready", "NotReady", msg, now)
	promOK := promReason == prometheusReasonExplicit || promReason == prometheusReasonDiscovered
	setCondition(&cur.Status.Conditions, fleetv1alpha1.ClusterConditionPrometheusEndpointResolved, promOK, promReason, promReason, promMsg, now)
	cur.Status.PrometheusEndpoint = promEndpoint
	if version != "" {
		cur.Status.KubernetesVersion = version
	}
	if reachable {
		cur.Status.LastConnected = &now
	}
	cur.Status.ObservedGeneration = cur.Generation
	_ = p.hub.Status().Patch(ctx, &cur, patch)
}

func setCondition(conds *[]metav1.Condition, condType string, ok bool, okReason, failReason, msg string, now metav1.Time) {
	status := metav1.ConditionFalse
	reason := failReason
	if ok {
		status = metav1.ConditionTrue
		reason = okReason
	}
	apimeta.SetStatusCondition(conds, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: now,
	})
}
