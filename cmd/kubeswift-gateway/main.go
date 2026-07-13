// Command kubeswift-gateway is the browser-facing Connect hub for the KubeSwift
// UI. It runs in a hub cluster, watches fleet.kubeswift.io Cluster objects, and
// (PR C2) fans the read/write/telemetry/console planes out across the fleet.
// This binary (PR C1) serves the ClusterService — the UI's cluster selector.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
	ctrl "sigs.k8s.io/controller-runtime"
	cacheopts "sigs.k8s.io/controller-runtime/pkg/cache"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kubeswift-io/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
	"github.com/kubeswift-io/kubeswift/internal/gateway"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
	"github.com/kubeswift-io/kubeswift/internal/version"
)

const defaultClustersNamespace = "kubeswift-system"

func main() {
	showVersion := flag.Bool("version", false, "Print version and exit")
	listen := flag.String("listen", ":8080", "Connect / gRPC-Web listen address")
	corsOrigin := flag.String("cors-allow-origin", "*", "Access-Control-Allow-Origin for browser clients")
	clustersNS := flag.String("clusters-namespace", clustersNamespace(), "Namespace the hub watches for fleet.kubeswift.io Cluster objects")
	metricsAddr := flag.String("metrics-bind-address", "0", `controller-runtime metrics address ("0" disables)`)
	authMode := flag.String("auth-mode", "insecure", `end-user auth: "oidc" (verify an IdP-issued OIDC token, impersonate the user), "token" (impersonate via TokenReview), or "insecure" (SA-trusted dev; no impersonation)`)
	oidcIssuer := flag.String("oidc-issuer-url", "", "OIDC issuer URL (auth-mode=oidc), e.g. https://keycloak.example.com/realms/kubeswift")
	oidcClientID := flag.String("oidc-client-id", "", "OIDC client ID / audience the ID token must carry (auth-mode=oidc)")
	oidcUsernameClaim := flag.String("oidc-username-claim", "email", "OIDC claim to use as the impersonated username")
	oidcGroupsClaim := flag.String("oidc-groups-claim", "groups", "OIDC claim to use as the impersonated groups")
	oidcUsernamePrefix := flag.String("oidc-username-prefix", "", "prefix prepended to the OIDC username (mirrors apiserver --oidc-username-prefix)")
	oidcGroupsPrefix := flag.String("oidc-groups-prefix", "", "prefix prepended to each OIDC group")
	oidcCAFile := flag.String("oidc-ca-file", "", "PEM CA bundle to trust when fetching the OIDC issuer's discovery/JWKS (for a private-CA IdP, e.g. a self-signed Keycloak); empty = system roots")
	promDiscoveryNS := flag.String("prometheus-discovery-namespaces", "monitoring,kube-prometheus-stack,observability,prometheus", "comma-separated namespaces scanned on each member for an in-cluster Prometheus when spec.prometheusEndpoint is empty; empty disables discovery")
	klog.InitFlags(nil)
	flag.Parse()

	crlog.SetLogger(klogr.New())
	log := crlog.Log.WithName("kubeswift-gateway")

	if *showVersion {
		fmt.Printf("kubeswift-gateway %s (git %s)\n", version.Version, version.GitCommit)
		os.Exit(0)
	}

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:         scheme.Scheme,
		LeaderElection: false,
		Metrics:        metricsserver.Options{BindAddress: *metricsAddr},
		// Scope the hub cache (and thus the cached client + informers) to the
		// namespace that holds Cluster objects and their credential Secrets.
		Cache: cacheopts.Options{
			DefaultNamespaces: map[string]cacheopts.Config{*clustersNS: {}},
		},
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// End-user authentication for member impersonation (decision D1).
	auth, err := buildAuthenticator(*authMode, cfg, log, oidcOptions{
		issuer:         *oidcIssuer,
		clientID:       *oidcClientID,
		usernameClaim:  *oidcUsernameClaim,
		groupsClaim:    *oidcGroupsClaim,
		usernamePrefix: *oidcUsernamePrefix,
		groupsPrefix:   *oidcGroupsPrefix,
		caFile:         *oidcCAFile,
	})
	if err != nil {
		log.Error(err, "unable to build authenticator")
		os.Exit(1)
	}

	// The shared informer broadcaster backing ClusterService.Watch.
	watcher := gateway.NewClusterWatcher(mgr.GetCache())
	if err := mgr.Add(watcher); err != nil {
		log.Error(err, "unable to add cluster watcher")
		os.Exit(1)
	}

	// The per-member client pool backing the guest read plane. It also resolves
	// each member's telemetry endpoint, discovering an in-cluster Prometheus in
	// the given namespaces when spec.prometheusEndpoint is empty.
	pool := gateway.NewClientPool(mgr.GetCache(), mgr.GetClient(), *clustersNS, splitCSV(*promDiscoveryNS))
	if err := mgr.Add(pool); err != nil {
		log.Error(err, "unable to add client pool")
		os.Exit(1)
	}

	clusterSvc := gateway.NewClusterService(mgr.GetClient(), *clustersNS, watcher, pool, auth)
	clusterPath, clusterHandler := kubeswiftv1connect.NewClusterServiceHandler(clusterSvc)

	guestSvc := gateway.NewGuestService(pool, auth)
	guestPath, guestHandler := kubeswiftv1connect.NewGuestServiceHandler(guestSvc)

	// Migrations (P2): read plane over SwiftMigrations (the UI polls it live).
	migSvc := gateway.NewMigrationService(pool, auth)
	migPath, migHandler := kubeswiftv1connect.NewMigrationServiceHandler(migSvc)

	// Sandboxes (v0.11.0 A2): read plane over SwiftSandbox + SwiftSandboxPool
	// (the MicroVM inventory). Write actions (create/delete) land in A3.
	sandboxSvc := gateway.NewSandboxService(pool, auth)
	sandboxPath, sandboxHandler := kubeswiftv1connect.NewSandboxServiceHandler(sandboxSvc)

	// Telemetry (P1): per-VM range metrics from each member's Prometheus.
	telSvc := gateway.NewTelemetryService(pool, auth)
	telPath, telHandler := kubeswiftv1connect.NewTelemetryServiceHandler(telSvc)

	// Explorer (P2): read-only generic resource browser (nodes, namespaces,
	// networking, storage, secrets — metadata only — and the KubeSwift CRDs).
	resSvc := gateway.NewResourceService(pool, auth)
	resPath, resHandler := kubeswiftv1connect.NewResourceServiceHandler(resSvc)

	// Access (B3): the RBAC editor backend — list/create KubeSwift roles + assign
	// them to OIDC users/groups (cluster-wide or per-namespace), as the user.
	accessSvc := gateway.NewAccessService(pool, auth)
	accessPath, accessHandler := kubeswiftv1connect.NewAccessServiceHandler(accessSvc)
	// Console stays a P0 stub (CodeUnimplemented) until the console plane lands.
	conPath, conHandler := kubeswiftv1connect.NewConsoleServiceHandler(kubeswiftv1connect.UnimplementedConsoleServiceHandler{})

	// Console plane (D5 bootstrap): a raw WebSocket at /console that exec-bridges
	// the guest's serial socket. Not a Connect RPC — browsers can't do bidi
	// Connect — so it rides RawHandlers, not the generated ConsoleService stub.
	consoleWS := gateway.NewConsoleHandler(pool, auth)

	srv := &gateway.Server{
		Addr:          *listen,
		AllowedOrigin: *corsOrigin,
		Handlers: []gateway.ConnectHandler{
			{Path: clusterPath, Handler: clusterHandler},
			{Path: guestPath, Handler: guestHandler},
			{Path: migPath, Handler: migHandler},
			{Path: sandboxPath, Handler: sandboxHandler},
			{Path: telPath, Handler: telHandler},
			{Path: resPath, Handler: resHandler},
			{Path: accessPath, Handler: accessHandler},
			{Path: conPath, Handler: conHandler},
		},
		RawHandlers: []gateway.ConnectHandler{
			{Path: "/console", Handler: consoleWS},
		},
		Log: log.WithName("server"),
	}
	if err := mgr.Add(srv); err != nil {
		log.Error(err, "unable to add server")
		os.Exit(1)
	}

	log.Info("starting kubeswift-gateway",
		"listen", *listen, "clustersNamespace", *clustersNS, "version", version.Version)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited non-zero")
		os.Exit(1)
	}
}

func clustersNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return defaultClustersNamespace
}

// splitCSV parses a comma-separated flag into a trimmed, empty-dropped slice.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// oidcOptions carries the auth-mode=oidc flags into buildAuthenticator.
type oidcOptions struct {
	issuer, clientID             string
	usernameClaim, groupsClaim   string
	usernamePrefix, groupsPrefix string
	caFile                       string
}

// buildAuthenticator selects the end-user auth strategy (decision D1).
// "oidc" verifies an IdP-issued OIDC ID token (gateway-side OIDC, A1) and
// impersonates the claim-derived user; "token" impersonates the bearer-token
// user via the hub's TokenReview; "insecure" is the SA-trusted dev stub with no
// impersonation.
func buildAuthenticator(mode string, cfg *rest.Config, log logr.Logger, oidc oidcOptions) (gateway.Authenticator, error) {
	switch mode {
	case "oidc":
		if oidc.issuer == "" || oidc.clientID == "" {
			return nil, fmt.Errorf("auth-mode=oidc requires --oidc-issuer-url and --oidc-client-id")
		}
		log.Info("auth-mode=oidc: impersonate the OIDC-token user", "issuer", oidc.issuer, "clientID", oidc.clientID, "usernameClaim", oidc.usernameClaim, "caFile", oidc.caFile)
		return gateway.NewOIDCAuthenticator(oidc.issuer, oidc.clientID, oidc.caFile, gateway.OIDCClaimConfig{
			UsernameClaim:  oidc.usernameClaim,
			GroupsClaim:    oidc.groupsClaim,
			UsernamePrefix: oidc.usernamePrefix,
			GroupsPrefix:   oidc.groupsPrefix,
		}), nil
	case "token":
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, err
		}
		return gateway.NewTokenReviewAuthenticator(cs.AuthenticationV1().TokenReviews()), nil
	case "insecure":
		log.Info("auth-mode=insecure: member queries run as the gateway credential (no per-user impersonation)")
		return gateway.NewInsecureAuthenticator(), nil
	default:
		return nil, fmt.Errorf("unknown auth-mode %q (want oidc|token|insecure)", mode)
	}
}
