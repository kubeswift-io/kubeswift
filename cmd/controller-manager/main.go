package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	cacheopts "sigs.k8s.io/controller-runtime/pkg/cache"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/controller/migrationcert"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftgpu"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftguest"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftguestpool"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftimage"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftkernel"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftmigration"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftrestore"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftsnapshot"
	"github.com/projectbeskar/kubeswift/internal/scheme"
	"github.com/projectbeskar/kubeswift/internal/version"
	swiftguestwebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftguest"
	swiftimagewebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftimage"
	swiftmigrationwebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftmigration"
	swiftrestorewebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftrestore"
	swiftseedprofilewebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftseedprofile"
	swiftsnapshotwebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftsnapshot"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

const (
	defaultWebhookPort      = 9443
	defaultWebhookHost      = "0.0.0.0"
	defaultCertDir          = "/tmp/k8s-webhook-server/serving-certs"
	webhookCertDirEnv       = "WEBHOOK_CERT_DIR"
	leaderElectionID        = "kubeswift-controller-manager"
	leaderElectionNSEnv     = "POD_NAMESPACE"
	defaultLeaderElectionNS = "kubeswift-system"
)

func main() {
	showVersion := flag.Bool("version", false, "Print version and exit")
	webhookEnabled := flag.Bool("webhook-enabled", false, "Enable admission webhooks (requires TLS certs)")
	migrationMTLSEnabled := flag.Bool("migration-mtls-enabled", false, "Enable the live-migration mTLS cert provisioner (Phase 3c; requires cert-manager)")
	leaderElect := flag.Bool("leader-elect", false, "Enable leader election for controller manager")
	webhookPort := flag.Int("webhook-port", defaultWebhookPort, "Port for webhook server")
	webhookHost := flag.String("webhook-host", defaultWebhookHost, "Host for webhook server")
	webhookCertDir := flag.String("webhook-cert-dir", defaultCertDir, "Directory containing webhook TLS certs (tls.crt, tls.key)")
	metricsAddr := flag.String("metrics-bind-address", ":8080", "Address for metrics endpoint")
	klog.InitFlags(nil)
	flag.Parse()

	crlog.SetLogger(klogr.New())

	if *showVersion {
		fmt.Printf("controller-manager %s (git %s)\n", version.Version, version.GitCommit)
		os.Exit(0)
	}

	certDir := *webhookCertDir
	if envCertDir := os.Getenv(webhookCertDirEnv); envCertDir != "" {
		certDir = envCertDir
	}

	leaderElectionNS := defaultLeaderElectionNS
	if ns := os.Getenv(leaderElectionNSEnv); ns != "" {
		leaderElectionNS = ns
	}

	ctx := ctrl.SetupSignalHandler()

	mgrOpts := ctrl.Options{
		Scheme:                  scheme.Scheme,
		Metrics:                 metricsserver.Options{BindAddress: *metricsAddr},
		LeaderElection:          *leaderElect,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: leaderElectionNS,
		// Cache.SyncPeriod=30s: defense-in-depth for missed informer
		// events on labeled launcher pods, NOT the primary observation
		// mechanism (Phase 3a live migration design §5.5; architect F-3).
		// Phase 3a's controller observes src/dst pod migration-status
		// transitions exclusively via informer events; the labeled-pod
		// watch (kubeswift.io/migration label, set on dst at creation
		// and on src at StopAndCopy entry) drives all state advances
		// in the typical case. The 30s resync catches the rare missed
		// event (apiserver bookmark gap, controller restart mid-flight,
		// etc.) within an acceptable bound while keeping apiserver list-
		// load bounded. Default controller-runtime SyncPeriod is 10h —
		// far too coarse for live migration's seconds-scale cadence.
		// Phase 1 controllers tolerate 30s without behavior change
		// (their Reconcile is idempotent and their primary trigger is
		// also informer-driven).
		Cache: cacheopts.Options{SyncPeriod: ptr.To(30 * time.Second)},
	}
	if *webhookEnabled {
		mgrOpts.WebhookServer = webhook.NewServer(webhook.Options{
			Port:    *webhookPort,
			Host:    *webhookHost,
			CertDir: certDir,
		})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		klog.ErrorS(err, "unable to create manager")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		klog.ErrorS(err, "unable to create kubernetes clientset")
		os.Exit(1)
	}

	if err = (&swiftimage.SwiftImageReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Converter: swiftimage.StubConverter{},
		Clientset: clientset,
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftImage controller")
		os.Exit(1)
	}

	if err = (&swiftguest.SwiftGuestReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftGuest controller")
		os.Exit(1)
	}

	if err = (&swiftkernel.SwiftKernelReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftKernel controller")
		os.Exit(1)
	}

	if err = (&swiftgpu.SwiftGPUReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftGPU controller")
		os.Exit(1)
	}

	if err = (&swiftguestpool.SwiftGuestPoolReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftGuestPool controller")
		os.Exit(1)
	}

	if err = (&swiftsnapshot.SwiftSnapshotReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftSnapshot controller")
		os.Exit(1)
	}

	if err = (&swiftrestore.SwiftRestoreReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftRestore controller")
		os.Exit(1)
	}

	if err = (&swiftmigration.SwiftMigrationReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("swiftmigration-controller"),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftMigration controller")
		os.Exit(1)
	}

	// Live-migration mTLS cert provisioner (Phase 3c, Option B per-node
	// identity). Registered ONLY when --migration-mtls-enabled=true. The
	// reconciler issues one cert-manager Certificate per worker node
	// (SAN=nodeName) into the system namespace; it is dormant (not
	// registered) by default so clusters without cert-manager are
	// unaffected. SystemNamespace is the controller's own namespace
	// (POD_NAMESPACE), where the Helm/overlay-managed CA Issuer lives.
	if *migrationMTLSEnabled {
		if err = (&migrationcert.MigrationCertReconciler{
			Client:          mgr.GetClient(),
			Scheme:          mgr.GetScheme(),
			SystemNamespace: leaderElectionNS,
		}).SetupWithManager(mgr); err != nil {
			klog.ErrorS(err, "unable to create migrationcert controller")
			os.Exit(1)
		}
	}

	if *webhookEnabled {
		if err = ctrl.NewWebhookManagedBy(mgr, &swiftv1alpha1.SwiftGuest{}).
			WithCustomValidator(&swiftguestwebhook.Validator{}).
			WithCustomDefaulter(&swiftguestwebhook.Defaulter{}).
			Complete(); err != nil {
			klog.ErrorS(err, "unable to create SwiftGuest webhook")
			os.Exit(1)
		}
		if err = ctrl.NewWebhookManagedBy(mgr, &imagev1alpha1.SwiftImage{}).
			WithCustomValidator(&swiftimagewebhook.Validator{}).
			WithCustomDefaulter(&swiftimagewebhook.Defaulter{}).
			Complete(); err != nil {
			klog.ErrorS(err, "unable to create SwiftImage webhook")
			os.Exit(1)
		}
		if err = ctrl.NewWebhookManagedBy(mgr, &seedv1alpha1.SwiftSeedProfile{}).
			WithCustomValidator(&swiftseedprofilewebhook.Validator{}).
			WithCustomDefaulter(&swiftseedprofilewebhook.Defaulter{}).
			Complete(); err != nil {
			klog.ErrorS(err, "unable to create SwiftSeedProfile webhook")
			os.Exit(1)
		}
		if err = ctrl.NewWebhookManagedBy(mgr, &snapshotv1alpha1.SwiftSnapshot{}).
			WithCustomValidator(&swiftsnapshotwebhook.Validator{Client: mgr.GetClient()}).
			Complete(); err != nil {
			klog.ErrorS(err, "unable to create SwiftSnapshot webhook")
			os.Exit(1)
		}
		if err = ctrl.NewWebhookManagedBy(mgr, &snapshotv1alpha1.SwiftRestore{}).
			WithCustomValidator(&swiftrestorewebhook.Validator{Client: mgr.GetClient()}).
			Complete(); err != nil {
			klog.ErrorS(err, "unable to create SwiftRestore webhook")
			os.Exit(1)
		}
		if err = ctrl.NewWebhookManagedBy(mgr, &migrationv1alpha1.SwiftMigration{}).
			WithCustomValidator(&swiftmigrationwebhook.Validator{Client: mgr.GetClient()}).
			Complete(); err != nil {
			klog.ErrorS(err, "unable to create SwiftMigration webhook")
			os.Exit(1)
		}
	}

	klog.InfoS("starting manager", "version", version.Version, "git", version.GitCommit)
	if err := mgr.Start(ctx); err != nil {
		klog.ErrorS(err, "manager exited with error")
		os.Exit(1)
	}
}
