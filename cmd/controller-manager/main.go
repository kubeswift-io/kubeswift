package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/klogr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	cacheopts "sigs.k8s.io/controller-runtime/pkg/cache"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	seedv1alpha1 "github.com/kubeswift-io/kubeswift/api/seed/v1alpha1"
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/migrationcert"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftdrain"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftgpu"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguestpool"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftimage"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftkernel"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftmigration"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftrestore"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftsandbox"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftsnapshot"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftsnapshotschedule"
	kubeswiftmetrics "github.com/kubeswift-io/kubeswift/internal/metrics"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
	"github.com/kubeswift-io/kubeswift/internal/version"
	evictionwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/eviction"
	swiftguestwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftguest"
	swiftimagewebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftimage"
	swiftkernelwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftkernel"
	swiftmigrationwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftmigration"
	swiftrestorewebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftrestore"
	swiftsandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
	swiftseedprofilewebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftseedprofile"
	swiftsnapshotwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsnapshot"
	swiftsnapshotschedulewebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsnapshotschedule"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
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

// stringSliceFlag collects a repeatable string flag.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	showVersion := flag.Bool("version", false, "Print version and exit")
	webhookEnabled := flag.Bool("webhook-enabled", false, "Enable admission webhooks (requires TLS certs)")
	migrationMTLSEnabled := flag.Bool("migration-mtls-enabled", false, "Enable the live-migration mTLS cert provisioner (Phase 3c; requires cert-manager)")
	leaderElect := flag.Bool("leader-elect", false, "Enable leader election for controller manager")
	scopedLauncherRBAC := flag.Bool("scoped-launcher-rbac", false,
		"Retire the namespace-wide launcher RoleBindings and rely solely on the per-pod scoped Roles (#515). "+
			"Covers guests, migration targets, sandboxes and warm pool slots. "+
			"Off by default: enabling it DELETES a live grant.")
	webhookPort := flag.Int("webhook-port", defaultWebhookPort, "Port for webhook server")
	webhookHost := flag.String("webhook-host", defaultWebhookHost, "Host for webhook server")
	webhookCertDir := flag.String("webhook-cert-dir", defaultCertDir, "Directory containing webhook TLS certs (tls.crt, tls.key)")
	metricsAddr := flag.String("metrics-bind-address", ":8080", "Address for metrics endpoint")
	var allowedHostPaths stringSliceFlag
	flag.Var(&allowedHostPaths, "allowed-hostpath-prefix",
		"Host-path prefix a SwiftGuest may mount into the (privileged) launcher pod "+
			"-- spec.filesystems[].source.hostPath and vhost-user socket dirs. Repeatable. "+
			"EMPTY DENIES ALL, which is the default: opt in per cluster.")
	klog.InitFlags(nil)
	flag.Parse()

	crlog.SetLogger(klogr.New())

	if *showVersion {
		fmt.Printf("controller-manager %s (git %s)\n", version.Version, version.GitCommit)
		os.Exit(0)
	}

	// Published as a package var rather than threaded through every reconciler:
	// the switch is read at two points in two controllers (swiftguest and
	// swiftmigration), and passing it through both constructors would widen
	// their signatures for a value that never changes after startup.
	swiftguest.ScopedOnly = *scopedLauncherRBAC

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

	// CSI VolumeSnapshot CRDs are optional. The snapshot controllers
	// Owns(VolumeSnapshot); if the external-snapshotter CRDs are absent that watch
	// can never sync its cache and the manager fatally exits. Gate those watches on
	// a one-time discovery check so a cluster without the snapshot CRDs still runs
	// the core VM runtime (only CSI snapshots / cloneStrategy=snapshot degrade).
	volumeSnapshotEnabled := volumeSnapshotCRDsInstalled(clientset)
	if !volumeSnapshotEnabled {
		klog.Warning("CSI VolumeSnapshot CRDs (snapshot.storage.k8s.io/v1) are not installed; " +
			"the csi-volume-snapshot snapshot backend and SwiftImage cloneStrategy=snapshot are disabled. " +
			"Core VM runtime, local/s3 snapshots, and cloneStrategy=copy are unaffected. " +
			"Install the external-snapshotter CRDs to enable CSI snapshots.")
	}

	// CR-state gauges (kubeswift_guests, kubeswift_pool_replicas, ...) are
	// computed from the informer cache at scrape time — every listed type
	// already has a controller-driven informer, so scrapes are in-memory.
	kubeswiftmetrics.RegisterStateCollector(mgr.GetClient())

	if err = (&swiftimage.SwiftImageReconciler{
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		Converter:             swiftimage.StubConverter{},
		Clientset:             clientset,
		VolumeSnapshotEnabled: volumeSnapshotEnabled,
		SnapshotORASImage:     swiftsnapshot.SnapshotORASImage(),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftImage controller")
		os.Exit(1)
	}

	if err = (&swiftguest.SwiftGuestReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// Phase 3c (Option B): when mTLS is enabled, migration-eligible
		// launcher pods carry an idle source-side stunnel client sidecar so
		// a later live migration has its TLS client already in the
		// immutable source pod. SystemNamespace is where the stunnel
		// ConfigMap + per-node identity Secrets live.
		MigrationMTLSEnabled: *migrationMTLSEnabled,
		SystemNamespace:      leaderElectionNS,
		SnapshotS3Image:      swiftsnapshot.SnapshotS3Image(),
		SnapshotORASImage:    swiftsnapshot.SnapshotORASImage(),

		AllowedHostPathPrefixes: allowedHostPaths}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftGuest controller")
		os.Exit(1)
	}

	// Retires the legacy `default` subject from launcher RoleBindings once a
	// namespace stops running legacy launchers. Convergence otherwise only runs
	// from a guest/sandbox reconcile, so a fully drained namespace would keep
	// the namespace-wide pods:patch grant forever (#443).
	if err = (&swiftguest.LauncherRBACReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create launcher-rbac controller")
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
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		SnapshotS3Image:       swiftsnapshot.SnapshotS3Image(),
		SnapshotORASImage:     swiftsnapshot.SnapshotORASImage(),
		VolumeSnapshotEnabled: volumeSnapshotEnabled,
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftSnapshot controller")
		os.Exit(1)
	}

	if err = (&swiftrestore.SwiftRestoreReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		SnapshotS3Image:   swiftsnapshot.SnapshotS3Image(),
		SnapshotORASImage: swiftsnapshot.SnapshotORASImage(),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftRestore controller")
		os.Exit(1)
	}

	if err = (&swiftsnapshotschedule.SwiftSnapshotScheduleReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftSnapshotSchedule controller")
		os.Exit(1)
	}

	if err = (&swiftmigration.SwiftMigrationReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("swiftmigration-controller"),
		// Phase 3c (Option B): destination-side mTLS wiring is gated on
		// the same --migration-mtls-enabled flag that registers the
		// migrationcert provisioner below. SystemNamespace is where the
		// per-node identity Secrets live (cert-manager writes them there).
		MigrationMTLSEnabled: *migrationMTLSEnabled,
		SystemNamespace:      leaderElectionNS,
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftMigration controller")
		os.Exit(1)
	}

	if err = (&swiftsandbox.SwiftSandboxReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("swiftsandbox-controller"),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftSandbox controller")
		os.Exit(1)
	}

	if err = (&swiftsandbox.SwiftSandboxPoolReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("swiftsandboxpool-controller"),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftSandboxPool controller")
		os.Exit(1)
	}

	// Phase 4 drain controller: the "controller creates" half of drain
	// integration. Watches SwiftGuests for the kubeswift.io/drain-requested
	// marker (stamped by the eviction webhook) and creates a SwiftMigration to
	// evacuate the guest. Always registered — it is a no-op until a marker
	// appears (which only the eviction webhook stamps).
	if err = (&swiftdrain.Reconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("swiftdrain-controller"),
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create SwiftDrain controller")
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
			WithCustomValidator(&swiftguestwebhook.Validator{AllowedHostPathPrefixes: allowedHostPaths}).
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
		if err = ctrl.NewWebhookManagedBy(mgr, &kernelv1alpha1.SwiftKernel{}).
			WithCustomValidator(&swiftkernelwebhook.Validator{}).
			Complete(); err != nil {
			klog.ErrorS(err, "unable to create SwiftKernel webhook")
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
		if err = ctrl.NewWebhookManagedBy(mgr, &snapshotv1alpha1.SwiftSnapshotSchedule{}).
			WithCustomValidator(&swiftsnapshotschedulewebhook.Validator{}).
			Complete(); err != nil {
			klog.ErrorS(err, "unable to create SwiftSnapshotSchedule webhook")
			os.Exit(1)
		}
		if err = ctrl.NewWebhookManagedBy(mgr, &migrationv1alpha1.SwiftMigration{}).
			WithCustomValidator(&swiftmigrationwebhook.Validator{Client: mgr.GetClient()}).
			Complete(); err != nil {
			klog.ErrorS(err, "unable to create SwiftMigration webhook")
			os.Exit(1)
		}
		if err = ctrl.NewWebhookManagedBy(mgr, &sandboxv1alpha1.SwiftSandbox{}).
			WithCustomValidator(&swiftsandboxwebhook.Validator{}).
			Complete(); err != nil {
			klog.ErrorS(err, "unable to create SwiftSandbox webhook")
			os.Exit(1)
		}
		// Phase 4 drain integration: raw admission handler on pods/eviction
		// (not a CRD validator, so registered directly on the webhook-server
		// path). The VWC entry uses failurePolicy: Ignore — a webhook outage
		// must never break cluster-wide evictions; the per-guest PDB is the
		// hard floor that protects VMs when the webhook is down.
		mgr.GetWebhookServer().Register("/validate-pods-eviction",
			&webhook.Admission{Handler: &evictionwebhook.Handler{Client: mgr.GetClient()}})
		klog.InfoS("registered pods/eviction webhook at /validate-pods-eviction")
	}

	klog.InfoS("starting manager", "version", version.Version, "git", version.GitCommit)
	go watchdogCacheSync(ctx, mgr)
	if err := mgr.Start(ctx); err != nil {
		klog.ErrorS(err, "manager exited with error")
		os.Exit(1)
	}
}

// cacheSyncDeadline bounds how long the manager may run with an unsynced cache
// before it gives up. Generous enough for a cold apiserver and a large fleet;
// far short of "forever", which is the default and the bug (#460).
var cacheSyncDeadline = 3 * time.Minute

// watchdogCacheSync turns an un-syncable informer into a crash instead of a
// silent, permanent no-op.
//
// controller-runtime's cached client starts an informer per watched type and
// retries a failing LIST forever. The manager keeps running, the pod stays
// Ready, and NOTHING reconciles — not just the affected type, all of it,
// because no controller starts until the cache syncs. Observed for real when a
// controller built after the launcher-ServiceAccount work ran against an older
// chart's RBAC: `serviceaccounts is forbidden` every 15s, and freshly created
// SwiftGuests sat with a completely empty .status, no pod, no event, no phase.
// The operator has nothing to go on; `kubectl get deploy` says healthy.
//
// That is the most complete violation of "no silent failures" the system can
// manage, so make it loud: wait a bounded time for the cache, and if it has not
// synced, log the likely cause and exit non-zero. CrashLoopBackOff plus the
// reflector's own "is forbidden" lines is a diagnosis; an idle Running pod is
// not.
//
// The deadline is only armed while the process should be starting up — a normal
// ctx cancellation (SIGTERM, lost leader election) exits quietly and is not a
// sync failure.
// A NOTE ON WHY THIS WAS NEEDED, given the comment above about VolumeSnapshot:
// a MISSING CRD and a FORBIDDEN LIST fail differently. No such type at all fails
// when the informer is constructed, and the manager does exit — which is what
// that comment describes and why the CRD check exists. A type that exists but
// which RBAC denies fails inside the reflector's retry loop instead, forever,
// with the manager perfectly healthy. Only the second case is silent.
func watchdogCacheSync(ctx context.Context, mgr manager.Manager) {
	if err := cacheSyncOutcome(ctx, cacheSyncDeadline, mgr.GetCache().WaitForCacheSync); err != nil {
		klog.ErrorS(err, "REFUSING TO RUN with an unsynced informer cache")
		os.Exit(1)
	}
}

// cacheSyncOutcome holds the decision, separate from the process exit, so the
// three outcomes are testable without a real manager.
//
// Returns nil when the cache syncs, and nil when the parent context is already
// done — a SIGTERM or a lost leader election during start-up is a shutdown, not
// a fault, and must not be reported as one. Returns an error only when the
// deadline passes with the process still expected to be running.
func cacheSyncOutcome(ctx context.Context, deadline time.Duration, waitForCacheSync func(context.Context) bool) error {
	deadlined, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	if waitForCacheSync(deadlined) {
		klog.InfoS("informer cache synced")
		return nil
	}
	if ctx.Err() != nil {
		return nil // shutting down, not a sync failure
	}
	return fmt.Errorf("informer cache did not sync within %s. No controller reconciles until "+
		"EVERY watched type syncs, so this process would otherwise sit idle and Ready while doing "+
		"nothing at all — no pods created, no status written, for any resource. The usual cause is "+
		"missing RBAC on a watched type: look for \"is forbidden\" from the reflector above and grant "+
		"the controller-manager ClusterRole list+watch on that resource. Running a controller image "+
		"newer than its chart's RBAC produces exactly this", deadline)
}

// volumeSnapshotCRDsInstalled reports whether the CSI external-snapshotter
// VolumeSnapshot CRD (snapshot.storage.k8s.io/v1) is present, via a discovery
// lookup. Used to gate the snapshot controllers' Owns(VolumeSnapshot) watch: that
// watch cannot sync its cache when the CRD is absent and would fatally exit the
// manager, so on a cluster without the snapshot CRDs we skip it and run the core
// VM runtime regardless.
func volumeSnapshotCRDsInstalled(cs kubernetes.Interface) bool {
	list, err := cs.Discovery().ServerResourcesForGroupVersion("snapshot.storage.k8s.io/v1")
	if err != nil {
		return false
	}
	for _, r := range list.APIResources {
		if r.Kind == "VolumeSnapshot" {
			return true
		}
	}
	return false
}
