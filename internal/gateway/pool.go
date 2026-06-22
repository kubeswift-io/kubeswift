package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1alpha1 "github.com/projectbeskar/kubeswift/api/fleet/v1alpha1"
)

// swiftGuestGVR is the dynamic resource the read plane lists/watches per member.
// Using the dynamic client keyed by a known GVR avoids per-request discovery /
// RESTMapper construction; member clients are therefore cheap to create.
var swiftGuestGVR = schema.GroupVersionResource{
	Group:    "swift.kubeswift.io",
	Version:  "v1alpha1",
	Resource: "swiftguests",
}

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

	mu      sync.RWMutex
	members map[string]*member
}

type member struct {
	name       string
	config     *rest.Config
	prometheus string
}

// NewClientPool builds the pool over the hub cache + a read/write hub client.
func NewClientPool(c ctrlcache.Cache, hub client.Client, namespace string) *ClientPool {
	return &ClientPool{cache: c, hub: hub, namespace: namespace, members: map[string]*member{}}
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
		p.setStatus(ctx, c, false, "", err.Error())
		return
	}
	p.mu.Lock()
	p.members[c.Name] = &member{name: c.Name, config: cfg, prometheus: c.Spec.PrometheusEndpoint}
	p.mu.Unlock()

	// Probe reachability + version off the informer goroutine so a slow or
	// unreachable member never stalls the shared handler.
	cl := c.DeepCopy()
	go func() {
		if ver, perr := serverVersion(cfg); perr != nil {
			p.setStatus(ctx, cl, false, "", perr.Error())
		} else {
			p.setStatus(ctx, cl, true, ver, "")
		}
	}()
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
	if c.Spec.CredentialSecretRef.Name == "" {
		return nil, errors.New("spec.credentialSecretRef.name is empty")
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
	return dynamic.NewForConfig(cfg)
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

// setStatus best-effort patches the Cluster's Ready/Reachable conditions,
// version, and lastConnected. It re-reads the object to avoid clobbering a
// concurrent writer; failures are swallowed (status is advisory).
func (p *ClientPool) setStatus(ctx context.Context, c *fleetv1alpha1.Cluster, reachable bool, version, msg string) {
	var cur fleetv1alpha1.Cluster
	if err := p.hub.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.Name}, &cur); err != nil {
		return
	}
	patch := client.MergeFrom(cur.DeepCopy())
	now := metav1.Now()
	setCondition(&cur.Status.Conditions, fleetv1alpha1.ClusterConditionReachable, reachable, "Reachable", "Unreachable", msg, now)
	setCondition(&cur.Status.Conditions, fleetv1alpha1.ClusterConditionReady, reachable, "Ready", "NotReady", msg, now)
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
