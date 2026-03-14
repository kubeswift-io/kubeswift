package main

import (
	"flag"
	"os"

	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftguest"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftimage"
	"github.com/projectbeskar/kubeswift/internal/scheme"
	swiftguestwebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftguest"
	swiftimagewebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftimage"
	swiftseedprofilewebhook "github.com/projectbeskar/kubeswift/internal/webhook/swiftseedprofile"
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	ctx := ctrl.SetupSignalHandler()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme.Scheme,
	})
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

	klog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		klog.ErrorS(err, "manager exited with error")
		os.Exit(1)
	}
}
