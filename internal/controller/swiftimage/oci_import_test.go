package swiftimage

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
)

func ociImageResource(oci imagev1alpha1.OCIImageSource) *imagev1alpha1.SwiftImage {
	return &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "gold", Namespace: "default"},
		Spec: imagev1alpha1.SwiftImageSpec{
			Format: imagev1alpha1.DiskFormatRaw,
			Source: imagev1alpha1.ImageSource{OCI: &oci},
		},
	}
}

func TestStartImport_OCISourceCreatesJobWithPuller(t *testing.T) {
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftImageReconciler{Client: client, Scheme: scheme, SnapshotORASImage: "ghcr.io/k/snapshot-oras:test"}
	img := ociImageResource(imagev1alpha1.OCIImageSource{
		Repository: "zot.svc:5000/golden-ubuntu", Digest: "sha256:deadbeef",
	})

	result, err := r.StartImport(context.Background(), img)
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	if result.Phase != imagev1alpha1.SwiftImagePhaseImporting {
		t.Errorf("phase = %s, want Importing", result.Phase)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := client.Get(context.Background(), types.NamespacedName{Name: importPVCNamePrefix + "gold", Namespace: "default"}, &pvc); err != nil {
		t.Fatalf("PVC not created: %v", err)
	}
	var job batchv1.Job
	if err := client.Get(context.Background(), types.NamespacedName{Name: importJobNamePrefix + "gold", Namespace: "default"}, &job); err != nil {
		t.Fatalf("Job not created: %v", err)
	}
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 1 {
		t.Fatalf("want 1 init container (the puller); got %d", len(spec.InitContainers))
	}
	pull := spec.InitContainers[0]
	if pull.Image != "ghcr.io/k/snapshot-oras:test" {
		t.Errorf("puller image = %q, want the configured snapshot-oras image", pull.Image)
	}
	args := strings.Join(pull.Args, " ")
	for _, want := range []string{
		"--mode=download-image",
		"--repository=zot.svc:5000/golden-ubuntu",
		"--digest=sha256:deadbeef",
		"--file=/data/image.raw",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("puller args missing %q; got %q", want, args)
		}
	}
	// The main container reuses the ubuntu importer for the resize/patch tail.
	if len(spec.Containers) != 1 || spec.Containers[0].Image != "ubuntu:22.04" {
		t.Errorf("want a single ubuntu:22.04 main container; got %+v", spec.Containers)
	}
	// Both containers share the import PVC at /data.
	if len(spec.Volumes) < 1 || spec.Volumes[0].PersistentVolumeClaim == nil ||
		spec.Volumes[0].PersistentVolumeClaim.ClaimName != importPVCNamePrefix+"gold" {
		t.Errorf("data volume not backed by the import PVC: %+v", spec.Volumes)
	}
}

func TestStartImport_OCISourceInsecureTagAndCreds(t *testing.T) {
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftImageReconciler{Client: client, Scheme: scheme, SnapshotORASImage: "img:test"}
	img := ociImageResource(imagev1alpha1.OCIImageSource{
		Repository:           "zot.svc:5000/golden",
		Tag:                  "noble",
		Insecure:             true,
		CredentialsSecretRef: &imagev1alpha1.SecretObjectReference{Name: "reg-creds"},
	})
	if _, err := r.StartImport(context.Background(), img); err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	var job batchv1.Job
	if err := client.Get(context.Background(), types.NamespacedName{Name: importJobNamePrefix + "gold", Namespace: "default"}, &job); err != nil {
		t.Fatalf("Job not created: %v", err)
	}
	pull := job.Spec.Template.Spec.InitContainers[0]
	args := strings.Join(pull.Args, " ")
	if !strings.Contains(args, "--tag=noble") || !strings.Contains(args, "--insecure") {
		t.Errorf("expected --tag=noble + --insecure; got %q", args)
	}
	var dockerCfg bool
	for _, e := range pull.Env {
		if e.Name == "DOCKER_CONFIG" && e.Value == "/oras-auth" {
			dockerCfg = true
		}
	}
	if !dockerCfg {
		t.Error("DOCKER_CONFIG env missing on the puller")
	}
	var authVol bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "oras-auth" && v.Secret != nil && v.Secret.SecretName == "reg-creds" {
			authVol = true
		}
	}
	if !authVol {
		t.Error("oras-auth credential volume missing")
	}
}

func TestStartImport_OCIVerifyKeyMountsKeyAndFlag(t *testing.T) {
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftImageReconciler{Client: client, Scheme: scheme, SnapshotORASImage: "img:test"}
	img := ociImageResource(imagev1alpha1.OCIImageSource{
		Repository:         "ghcr.io/org/golden",
		Tag:                "noble",
		VerifyKeySecretRef: &imagev1alpha1.SecretObjectReference{Name: "cosign-pub"},
	})
	if _, err := r.StartImport(context.Background(), img); err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	var job batchv1.Job
	if err := client.Get(context.Background(), types.NamespacedName{Name: importJobNamePrefix + "gold", Namespace: "default"}, &job); err != nil {
		t.Fatalf("Job not created: %v", err)
	}
	pull := job.Spec.Template.Spec.InitContainers[0]

	if !strings.Contains(strings.Join(pull.Args, " "), "--verify-key=/verify-key/cosign.pub") {
		t.Errorf("puller must carry --verify-key; got %q", strings.Join(pull.Args, " "))
	}
	// cosign needs a writable HOME/TMPDIR (the puller has a read-only rootfs).
	var home, tmp bool
	for _, e := range pull.Env {
		if e.Name == "HOME" && e.Value == "/cosign-home" {
			home = true
		}
		if e.Name == "TMPDIR" && e.Value == "/cosign-home" {
			tmp = true
		}
	}
	if !home || !tmp {
		t.Errorf("puller must set HOME+TMPDIR to a writable dir for cosign; env=%+v", pull.Env)
	}
	// verify-key mounted read-only; cosign-home writable emptyDir mounted.
	var keyMount, homeMount bool
	for _, m := range pull.VolumeMounts {
		if m.Name == "verify-key" && m.MountPath == "/verify-key" && m.ReadOnly {
			keyMount = true
		}
		if m.Name == "cosign-home" && m.MountPath == "/cosign-home" {
			homeMount = true
		}
	}
	if !keyMount || !homeMount {
		t.Errorf("puller must mount verify-key (ro) + cosign-home (rw); mounts=%+v", pull.VolumeMounts)
	}
	// The verify-key volume projects the Secret's cosign.pub; cosign-home is an emptyDir.
	var keyVol, homeVol bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "verify-key" && v.Secret != nil && v.Secret.SecretName == "cosign-pub" {
			for _, it := range v.Secret.Items {
				if it.Key == "cosign.pub" && it.Path == "cosign.pub" {
					keyVol = true
				}
			}
		}
		if v.Name == "cosign-home" && v.EmptyDir != nil {
			homeVol = true
		}
	}
	if !keyVol {
		t.Error("verify-key volume must project the Secret's cosign.pub")
	}
	if !homeVol {
		t.Error("cosign-home emptyDir volume missing")
	}
}

func TestStartImport_OCINoImageConfigured_Failed(t *testing.T) {
	scheme := testScheme()
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftImageReconciler{Client: client, Scheme: scheme} // SnapshotORASImage empty
	img := ociImageResource(imagev1alpha1.OCIImageSource{Repository: "r", Tag: "t"})
	result, err := r.StartImport(context.Background(), img)
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	if result.Phase != imagev1alpha1.SwiftImagePhaseFailed {
		t.Errorf("phase = %s, want Failed when snapshot-oras image is unconfigured", result.Phase)
	}
}
