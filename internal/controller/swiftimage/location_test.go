package swiftimage

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
	"github.com/kubeswift-io/kubeswift/internal/storagelocation"
)

// testCA and testKey are a real self-signed CA certificate and a cosign-style
// public key: the location resolver parses both.
var testCA, testKey = genPEMs()

func genPEMs() (string, string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))
}

func nsDefaultLocation(repo string) *storagev1alpha1.SwiftStorageLocation {
	return &storagev1alpha1.SwiftStorageLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "default"},
		Spec: storagev1alpha1.StorageLocationSpec{Default: true, OCI: &storagev1alpha1.OCILocation{
			Repository: repo, CABundle: testCA, CredentialsSecretName: "reg", VerifyKey: testKey,
		}},
	}
}

func regSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "reg", Namespace: "default"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte("{}")},
	}
}

func importWith(t *testing.T, img *imagev1alpha1.SwiftImage, objs ...client.Object) (*ImportResult, client.Client) {
	t.Helper()
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r := &SwiftImageReconciler{Client: c, Scheme: scheme, SnapshotORASImage: "oras:test"}
	res, err := r.StartImport(context.Background(), img)
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	return res, c
}

func puller(t *testing.T, c client.Client) corev1.Container {
	t.Helper()
	var job batchv1.Job
	if err := c.Get(context.Background(), types.NamespacedName{Name: importJobNamePrefix + "gold", Namespace: "default"}, &job); err != nil {
		t.Fatalf("import Job: %v", err)
	}
	return job.Spec.Template.Spec.InitContainers[0]
}

func env(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// A source that sets no connection field takes them from the namespace's
// default location when the registry matches, and records them.
func TestImageSource_LocationApplies(t *testing.T) {
	img := ociImageResource(imagev1alpha1.OCIImageSource{Repository: "reg.example.com/golden/ubuntu", Tag: "v1"})
	res, c := importWith(t, img, nsDefaultLocation("reg.example.com/kubeswift"), regSecret())
	if res.Phase != imagev1alpha1.SwiftImagePhaseImporting {
		t.Fatalf("phase %s (%s: %s)", res.Phase, res.WaitReason, res.WaitMessage)
	}
	want := imagev1alpha1.ImageSourceStatus{Location: "SwiftStorageLocation/team", CABundle: testCA, CredentialsSecretName: "reg", VerifyKey: testKey}
	if res.Source == nil || *res.Source != want {
		t.Fatalf("source = %+v, want %+v", res.Source, want)
	}
	p := puller(t, c)
	if env(p, clonecommon.RegistryCAEnv) != testCA || env(p, clonecommon.VerifyKeyEnvName) != testKey || env(p, "DOCKER_CONFIG") != "/oras-auth" {
		t.Errorf("puller env = %+v", p.Env)
	}
}

// A default for another registry applies nothing: an anonymous pull, as before.
func TestImageSource_OtherRegistryDefaultIsIgnored(t *testing.T) {
	img := ociImageResource(imagev1alpha1.OCIImageSource{Repository: "ghcr.io/org/golden", Tag: "v1"})
	res, c := importWith(t, img, nsDefaultLocation("reg.example.com/kubeswift"), regSecret())
	if res.Source == nil || res.Source.Location != imagev1alpha1.ImageSourceLocationNone {
		t.Fatalf("source = %+v, want None", res.Source)
	}
	if p := puller(t, c); len(p.Env) != 0 {
		t.Errorf("anonymous pull got env %+v", p.Env)
	}
}

// A locationRef for another registry is a mistake worth saying.
func TestImageSource_LocationRefForOtherRegistryWaits(t *testing.T) {
	img := ociImageResource(imagev1alpha1.OCIImageSource{Repository: "ghcr.io/org/golden", Tag: "v1",
		LocationRef: &storagev1alpha1.StorageLocationRef{Kind: storagev1alpha1.KindSwiftStorageLocation, Name: "team"}})
	res, _ := importWith(t, img, nsDefaultLocation("reg.example.com/kubeswift"), regSecret())
	if res.WaitReason != storagelocation.ReasonInvalid {
		t.Fatalf("wait = %q (%s), want %s", res.WaitReason, res.WaitMessage, storagelocation.ReasonInvalid)
	}
}

func TestImageSource_MissingCredentialsWaits(t *testing.T) {
	img := ociImageResource(imagev1alpha1.OCIImageSource{Repository: "reg.example.com/golden/ubuntu", Tag: "v1"})
	res, c := importWith(t, img, nsDefaultLocation("reg.example.com/kubeswift"))
	if res.WaitReason != storagelocation.ReasonCredentialsMissing {
		t.Fatalf("wait = %q, want %s", res.WaitReason, storagelocation.ReasonCredentialsMissing)
	}
	var job batchv1.Job
	if err := c.Get(context.Background(), types.NamespacedName{Name: importJobNamePrefix + "gold", Namespace: "default"}, &job); err == nil {
		t.Error("import Job created while waiting for credentials")
	}
}

// Any connection field set: used as written, the location is not consulted.
func TestImageSource_ExplicitWins(t *testing.T) {
	img := ociImageResource(imagev1alpha1.OCIImageSource{Repository: "reg.example.com/golden/ubuntu", Tag: "v1", CABundle: testCA})
	res, c := importWith(t, img, nsDefaultLocation("reg.example.com/kubeswift"), regSecret())
	want := imagev1alpha1.ImageSourceStatus{Location: imagev1alpha1.ImageSourceLocationExplicit, CABundle: testCA}
	if res.Source == nil || *res.Source != want {
		t.Fatalf("source = %+v, want %+v", res.Source, want)
	}
	p := puller(t, c)
	if env(p, clonecommon.RegistryCAEnv) != testCA || env(p, "DOCKER_CONFIG") != "" {
		t.Errorf("explicit CA only: puller env %+v", p.Env)
	}
}

// Once recorded, status.source is what the import uses; a changed default does
// not move it.
func TestImageSource_RecordedIsReused(t *testing.T) {
	img := ociImageResource(imagev1alpha1.OCIImageSource{Repository: "reg.example.com/golden/ubuntu", Tag: "v1"})
	img.Status.Source = &imagev1alpha1.ImageSourceStatus{Location: imagev1alpha1.ImageSourceLocationNone}
	res, c := importWith(t, img, nsDefaultLocation("reg.example.com/kubeswift"), regSecret())
	if res.Source != nil {
		t.Errorf("re-recorded source %+v", res.Source)
	}
	if p := puller(t, c); len(p.Env) != 0 {
		t.Errorf("recorded None, but the puller got %+v", p.Env)
	}
}
