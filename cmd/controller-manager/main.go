package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftguest"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftimage"
	"github.com/projectbeskar/kubeswift/internal/scheme"
	"github.com/projectbeskar/kubeswift/internal/version"
	swiftguestwebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftguest"
	swiftimagewebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftimage"
	swiftseedprofilewebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftseedprofile"
)

const (
	defaultWebhookPort = 9443
	defaultWebhookHost = "0.0.0.0"
	defaultCertDir     = "/tmp/k8s-webhook-server/serving-certs"
	webhookCertDirEnv  = "WEBHOOK_CERT_DIR"
)

func main() {
	showVersion := flag.Bool("version", false, "Print version and exit")
	webhookEnabled := flag.Bool("webhook-enabled", false, "Enable admission webhooks (requires TLS certs)")
	webhookPort := flag.Int("webhook-port", defaultWebhookPort, "Port for webhook server")
	webhookHost := flag.String("webhook-host", defaultWebhookHost, "Host for webhook server")
	webhookCertDir := flag.String("webhook-cert-dir", defaultCertDir, "Directory containing webhook TLS certs (tls.crt, tls.key)")
	klog.InitFlags(nil)
	flag.Parse()

	if *showVersion {
		fmt.Printf("controller-manager %s (git %s)\n", version.Version, version.GitCommit)
		os.Exit(0)
	}

	certDir := *webhookCertDir
	if envCertDir := os.Getenv(webhookCertDirEnv); envCertDir != "" {
		certDir = envCertDir
	}

	ctx := ctrl.SetupSignalHandler()

	mgrOpts := ctrl.Options{
		Scheme: scheme.Scheme,
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

	if err = (&swiftimage.SwiftImageReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Converter: swiftimage.StubConverter{},
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
	}

	klog.InfoS("starting manager", "version", version.Version, "git", version.GitCommit)
	if err := mgr.Start(ctx); err != nil {
		klog.ErrorS(err, "manager exited with error")
		os.Exit(1)
	}
}
