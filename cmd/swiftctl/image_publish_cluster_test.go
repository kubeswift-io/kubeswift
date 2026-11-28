package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// publishCmdWith returns the publish command with only these flags set.
func publishCmdWith(t *testing.T, flags map[string]string) *cobra.Command {
	t.Helper()
	publishTo, publishTag, publishOSType, publishInsecure = "", "latest", "linux", false
	publishFromSnapshot, publishFromGuest, publishFromImage = "", "", ""
	publishImageName, publishLocation, publishRequestName, publishCreateImage = "", "", "", ""
	publishOverwriteTag, publishCredsSecret, publishSigningSecret, publishVerifyKeySecret = false, "", "", ""
	cmd := &cobra.Command{}
	cmd.Flags().AddFlagSet(imagePublishCmd.Flags())
	cmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = false })
	for k, v := range flags {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	return cmd
}

func TestBuildPublishRequest(t *testing.T) {
	cmd := publishCmdWith(t, map[string]string{"from-snapshot": "web-snap", "image": "web", "tag": "2026.10",
		"location": "cluster/registry", "create-image": "web-img", "os-type": "windows"})
	p, err := buildPublishRequest(cmd, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	d := p.Spec.Destination
	if p.Name != "web-snap-2026-10" || p.Spec.Source.SnapshotRef.Name != "web-snap" || d.Image != "web" || d.Tag != "2026.10" ||
		d.LocationRef == nil || d.LocationRef.Kind != storagev1alpha1.KindSwiftClusterStorageLocation || p.Spec.CreateImage.Name != "web-img" ||
		p.Spec.OSType != imagev1alpha1.OSTypeWindows {
		t.Fatalf("request %+v", p)
	}

	cmd = publishCmdWith(t, map[string]string{"from-guest": "tmpl", "to": "reg.local:5000/golden/web", "tag": "v1", "insecure": "true",
		"credentials-secret": "creds", "signing-key-secret": "sign", "overwrite-tag": "true"})
	p, err = buildPublishRequest(cmd, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	d = p.Spec.Destination
	if d.Repository != "reg.local:5000/golden/web" || !d.Insecure || d.CredentialsSecretRef.Name != "creds" || d.SigningKeySecretRef.Name != "sign" ||
		!d.OverwriteTag || p.Spec.Source.GuestRef.Name != "tmpl" || p.Spec.OSType != "" {
		t.Fatalf("explicit request %+v", p)
	}

	for name, flags := range map[string]map[string]string{
		"no tag":           {"from-image": "noble", "image": "web"},
		"two sources":      {"from-image": "noble", "from-guest": "g", "image": "web", "tag": "v1"},
		"no image":         {"from-image": "noble", "tag": "v1"},
		"to with image":    {"from-image": "noble", "to": "r/x", "image": "web", "tag": "v1"},
		"secret, location": {"from-image": "noble", "image": "web", "tag": "v1", "credentials-secret": "c"},
		"local sign key":   {"from-image": "noble", "image": "web", "tag": "v1", "sign-key": "k"},
	} {
		if _, err := buildPublishRequest(publishCmdWith(t, flags), "ns"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWaitPublish(t *testing.T) {
	p := &imagev1alpha1.SwiftImagePublish{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"},
		Status: imagev1alpha1.SwiftImagePublishStatus{Phase: imagev1alpha1.SwiftImagePublishReady, Reference: "r/x:v1", Digest: "sha256:d",
			SizeBytes: 1 << 30, Signed: true, Image: "img"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(p).Build()
	var out bytes.Buffer
	if err := waitPublish(context.Background(), c, &out, client.ObjectKeyFromObject(p), time.Second, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sha256:d") || !strings.Contains(out.String(), "SwiftImage ns/img") {
		t.Fatalf("output %q", out.String())
	}
	p.Status = imagev1alpha1.SwiftImagePublishStatus{Phase: imagev1alpha1.SwiftImagePublishFailed,
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "TagInUse", Message: "tag in use"}}}
	c = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(p).Build()
	if err := waitPublish(context.Background(), c, &out, client.ObjectKeyFromObject(p), time.Second, time.Millisecond); err == nil || !strings.Contains(err.Error(), "TagInUse") {
		t.Fatalf("failed publish: %v", err)
	}
}
