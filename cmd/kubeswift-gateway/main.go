// Command kubeswift-gateway is the browser-facing Connect hub for the KubeSwift
// UI. It runs in a hub cluster, watches fleet.kubeswift.io Cluster objects, and
// (PR C2) fans the read/write/telemetry/console planes out across the fleet.
// This binary (PR C1) serves the ClusterService — the UI's cluster selector.
// See docs/design/ui-backend-enablement.md.
package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
	ctrl "sigs.k8s.io/controller-runtime"
	cacheopts "sigs.k8s.io/controller-runtime/pkg/cache"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/projectbeskar/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
	"github.com/projectbeskar/kubeswift/internal/gateway"
	"github.com/projectbeskar/kubeswift/internal/scheme"
	"github.com/projectbeskar/kubeswift/internal/version"
)

const defaultClustersNamespace = "kubeswift-system"

func main() {
	showVersion := flag.Bool("version", false, "Print version and exit")
	listen := flag.String("listen", ":8080", "Connect / gRPC-Web listen address")
	corsOrigin := flag.String("cors-allow-origin", "*", "Access-Control-Allow-Origin for browser clients")
	clustersNS := flag.String("clusters-namespace", clustersNamespace(), "Namespace the hub watches for fleet.kubeswift.io Cluster objects")
	metricsAddr := flag.String("metrics-bind-address", "0", `controller-runtime metrics address ("0" disables)`)
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

	// The shared informer broadcaster backing ClusterService.Watch.
	watcher := gateway.NewClusterWatcher(mgr.GetCache())
	if err := mgr.Add(watcher); err != nil {
		log.Error(err, "unable to add cluster watcher")
		os.Exit(1)
	}

	clusterSvc := gateway.NewClusterService(mgr.GetClient(), *clustersNS, watcher)
	clusterPath, clusterHandler := kubeswiftv1connect.NewClusterServiceHandler(clusterSvc)

	srv := &gateway.Server{
		Addr:          *listen,
		AllowedOrigin: *corsOrigin,
		Handlers: []gateway.ConnectHandler{
			{Path: clusterPath, Handler: clusterHandler},
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
