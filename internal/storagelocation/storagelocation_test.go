package storagelocation

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
)

// testCA returns a self-signed CA certificate as PEM.
func testCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func testPublicKey(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func oci(mut func(*storagev1alpha1.OCILocation)) *storagev1alpha1.StorageLocationSpec {
	o := &storagev1alpha1.OCILocation{Repository: "registry.example.com/kubeswift"}
	if mut != nil {
		mut(o)
	}
	return &storagev1alpha1.StorageLocationSpec{OCI: o}
}

func TestValidate(t *testing.T) {
	ca, pub := testCA(t), testPublicKey(t)
	ok := map[string]*storagev1alpha1.StorageLocationSpec{
		"minimal oci":         oci(nil),
		"registry with port":  oci(func(o *storagev1alpha1.OCILocation) { o.Repository = "registry.example.com:5000/team/vm" }),
		"anonymous":           oci(func(o *storagev1alpha1.OCILocation) { o.Anonymous = true }),
		"named credentials":   oci(func(o *storagev1alpha1.OCILocation) { o.CredentialsSecretName = "regcreds" }),
		"ca bundle":           oci(func(o *storagev1alpha1.OCILocation) { o.CABundle = ca }),
		"two certs in bundle": oci(func(o *storagev1alpha1.OCILocation) { o.CABundle = ca + ca }),
		"verify key":          oci(func(o *storagev1alpha1.OCILocation) { o.VerifyKey = pub }),
		"csi only":            {CSI: &storagev1alpha1.CSILocation{VolumeSnapshotClassName: "longhorn-snap"}},
	}
	for name, spec := range ok {
		if err := Validate(spec); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]struct {
		spec *storagev1alpha1.StorageLocationSpec
		want string
	}{
		"neither oci nor csi":  {&storagev1alpha1.StorageLocationSpec{}, "oci, csi, or both"},
		"no registry":          {oci(func(o *storagev1alpha1.OCILocation) { o.Repository = "kubeswift" }), "not a registry/repository reference"},
		"tag":                  {oci(func(o *storagev1alpha1.OCILocation) { o.Repository = "registry.example.com/kubeswift:v1" }), "carries a tag or digest"},
		"anonymous and secret": {oci(func(o *storagev1alpha1.OCILocation) { o.Anonymous = true; o.CredentialsSecretName = "regcreds" }), "mutually exclusive"},
		"bad secret name":      {oci(func(o *storagev1alpha1.OCILocation) { o.CredentialsSecretName = "Reg_Creds" }), "not a valid Secret name"},
		"garbage ca":           {oci(func(o *storagev1alpha1.OCILocation) { o.CABundle = "not pem" }), "caBundle"},
		"key in ca bundle":     {oci(func(o *storagev1alpha1.OCILocation) { o.CABundle = pub }), "only CERTIFICATE blocks"},
		"garbage verify key":   {oci(func(o *storagev1alpha1.OCILocation) { o.VerifyKey = "not pem" }), "verifyKey"},
		"cert as verify key":   {oci(func(o *storagev1alpha1.OCILocation) { o.VerifyKey = ca }), "verifyKey"},
		"bad snapshot class":   {&storagev1alpha1.StorageLocationSpec{CSI: &storagev1alpha1.CSILocation{VolumeSnapshotClassName: "Bad_Class"}}, "volumeSnapshotClassName"},
	}
	for name, c := range bad {
		err := Validate(c.spec)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, c.want)
		}
	}
}

func TestRegistry(t *testing.T) {
	for repo, want := range map[string]string{
		"registry.example.com/kubeswift":    "registry.example.com",
		"registry.example.com:5000/a/b":     "registry.example.com:5000",
		"val-reg.ns.svc:5000/vm-snapshots":  "val-reg.ns.svc:5000",
		"ghcr.io/kubeswift-io/vm-snapshots": "ghcr.io",
	} {
		got, err := Registry(repo)
		if err != nil || got != want {
			t.Errorf("Registry(%q) = %q, %v; want %q", repo, got, err, want)
		}
	}
}

func TestOthers(t *testing.T) {
	if got := Others([]string{"a", "b", "c"}, "b"); strings.Join(got, ",") != "a,c" {
		t.Errorf("Others = %v", got)
	}
	if got := Others([]string{"a"}, "a"); len(got) != 0 {
		t.Errorf("Others of only self = %v", got)
	}
}
