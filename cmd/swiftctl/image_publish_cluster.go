package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
)

// In-cluster sources for `image publish`: they create a SwiftImagePublish.
var (
	publishFromSnapshot    string
	publishFromGuest       string
	publishFromImage       string
	publishImageName       string
	publishLocation        string
	publishRequestName     string
	publishCreateImage     string
	publishOverwriteTag    bool
	publishCredsSecret     string
	publishSigningSecret   string
	publishVerifyKeySecret string
	publishWait            bool
	publishTimeout         time.Duration
)

func init() {
	f := imagePublishCmd.Flags()
	f.StringVar(&publishFromSnapshot, "from-snapshot", "", "Publish a SwiftSnapshot's root disk from the cluster (csi-volume-snapshot, or a full-state oci snapshot) instead of a local disk")
	f.StringVar(&publishFromGuest, "from-guest", "", "Publish a stopped SwiftGuest's root disk from the cluster; the guest is held stopped until its disk has been read")
	f.StringVar(&publishFromImage, "from-image", "", "Publish a Ready SwiftImage from the cluster")
	f.StringVar(&publishImageName, "image", "", "With --from-*: the image's name under the storage location (<repository>/<namespace>/images/<image>)")
	f.StringVar(&publishLocation, "location", "", "With --from-*: storage location, NAME or cluster/NAME; omit for the namespace's default, else the cluster's")
	f.StringVar(&publishRequestName, "name", "", "With --from-*: name of the SwiftImagePublish (default: <source>-<tag>)")
	f.StringVar(&publishCreateImage, "create-image", "", "With --from-*: also create a SwiftImage of this name, pinned to the pushed digest")
	f.BoolVar(&publishOverwriteTag, "overwrite-tag", false, "With --from-*: move --tag if it names another image (by default the publish fails with TagInUse)")
	f.StringVar(&publishCredsSecret, "credentials-secret", "", "With --from-* and --to: dockerconfigjson Secret in the namespace for the registry")
	f.StringVar(&publishSigningSecret, "signing-key-secret", "", "With --from-* and --to: Secret in the namespace with cosign.key (and cosign.password); the image is signed")
	f.StringVar(&publishVerifyKeySecret, "verify-key-secret", "", "With --from-* and --to: Secret in the namespace with cosign.pub, for the SwiftImage --create-image makes")
	f.BoolVar(&publishWait, "wait", false, "With --from-*: wait until the publish is Ready or Failed")
	f.DurationVar(&publishTimeout, "timeout", 30*time.Minute, "With --wait: how long to wait")
}

// clusterSource reports whether a --from-* flag was given, and the source.
func clusterSource() (imagev1alpha1.ImagePublishSource, string, int) {
	var src imagev1alpha1.ImagePublishSource
	n, label := 0, ""
	if publishFromSnapshot != "" {
		src.SnapshotRef = &corev1.LocalObjectReference{Name: publishFromSnapshot}
		n, label = n+1, publishFromSnapshot
	}
	if publishFromGuest != "" {
		src.GuestRef = &corev1.LocalObjectReference{Name: publishFromGuest}
		n, label = n+1, publishFromGuest
	}
	if publishFromImage != "" {
		src.ImageRef = &corev1.LocalObjectReference{Name: publishFromImage}
		n, label = n+1, publishFromImage
	}
	return src, label, n
}

var nonName = regexp.MustCompile(`[^a-z0-9-]+`)

// requestName is the default SwiftImagePublish name: <source>-<tag>, as a
// DNS label.
func requestName(source, tag string) string {
	n := nonName.ReplaceAllString(strings.ToLower(source+"-"+tag), "-")
	n = strings.Trim(n, "-")
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	return n
}

// buildPublishRequest turns the flags into a SwiftImagePublish.
func buildPublishRequest(cmd *cobra.Command, ns string) (*imagev1alpha1.SwiftImagePublish, error) {
	src, label, n := clusterSource()
	if n != 1 {
		return nil, fmt.Errorf("give exactly one of --from-snapshot, --from-guest and --from-image")
	}
	if !cmd.Flags().Changed("tag") {
		return nil, fmt.Errorf("--tag is required with --from-*: tags do not move, so name the version")
	}
	if cmd.Flags().Changed("sign-key") || cmd.Flags().Changed("chunk-size-mib") || cmd.Flags().Changed("keep-converted") {
		return nil, fmt.Errorf("--sign-key, --chunk-size-mib and --keep-converted are for a local disk; in the cluster the destination's signing key signs (--signing-key-secret with --to)")
	}
	d := imagev1alpha1.ImagePublishDestination{Tag: publishTag, OverwriteTag: publishOverwriteTag}
	if publishTo != "" {
		if publishImageName != "" || publishLocation != "" {
			return nil, fmt.Errorf("--to names the whole repository; --image and --location are for a storage location")
		}
		d.Repository = publishTo
		d.Insecure = publishInsecure
		if publishCredsSecret != "" {
			d.CredentialsSecretRef = &imagev1alpha1.SecretObjectReference{Name: publishCredsSecret}
		}
		if publishSigningSecret != "" {
			d.SigningKeySecretRef = &imagev1alpha1.SecretObjectReference{Name: publishSigningSecret}
		}
		if publishVerifyKeySecret != "" {
			d.VerifyKeySecretRef = &imagev1alpha1.SecretObjectReference{Name: publishVerifyKeySecret}
		}
	} else {
		if publishImageName == "" {
			return nil, fmt.Errorf("--image is required with --from-* unless --to names the repository")
		}
		if publishInsecure || publishCredsSecret != "" || publishSigningSecret != "" || publishVerifyKeySecret != "" {
			return nil, fmt.Errorf("--insecure and the --*-secret flags go with --to; a storage location carries its own")
		}
		d.Image = publishImageName
		ref, err := parseLocationFlag(publishLocation)
		if err != nil {
			return nil, err
		}
		d.LocationRef = ref
	}
	name := publishRequestName
	if name == "" {
		name = requestName(label, publishTag)
	}
	pub := &imagev1alpha1.SwiftImagePublish{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       imagev1alpha1.SwiftImagePublishSpec{Source: src, Destination: d},
	}
	if cmd.Flags().Changed("os-type") {
		pub.Spec.OSType = imagev1alpha1.OSType(publishOSType)
	}
	if publishCreateImage != "" {
		pub.Spec.CreateImage = &imagev1alpha1.ImagePublishCreateImage{Name: publishCreateImage}
	}
	return pub, nil
}

func runClusterPublish(cmd *cobra.Command) error {
	ns := getNamespace()
	pub, err := buildPublishRequest(cmd, ns)
	if err != nil {
		return err
	}
	c, err := newSnapshotClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := c.Create(ctx, pub); err != nil {
		return fmt.Errorf("create SwiftImagePublish: %w", err)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Created SwiftImagePublish %s/%s\n", ns, pub.Name)
	if !publishWait {
		fmt.Fprintf(out, "Follow it with: kubectl -n %s get sip %s\n", ns, pub.Name)
		return nil
	}
	return waitPublish(ctx, c, out, client.ObjectKeyFromObject(pub), publishTimeout, 2*time.Second)
}

// waitPublish polls the request until it is Ready or Failed, printing each
// change of phase or reason.
func waitPublish(ctx context.Context, c client.Reader, out io.Writer, key client.ObjectKey, timeout, every time.Duration) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		var p imagev1alpha1.SwiftImagePublish
		if err := c.Get(ctx, key, &p); err != nil {
			return err
		}
		reason, msg := "", ""
		if cond := meta.FindStatusCondition(p.Status.Conditions, "Ready"); cond != nil {
			reason, msg = cond.Reason, cond.Message
		}
		if now := string(p.Status.Phase) + "/" + reason; now != last && p.Status.Phase != "" {
			fmt.Fprintf(out, "  %s: %s\n", p.Status.Phase, msg)
			last = now
		}
		switch p.Status.Phase {
		case imagev1alpha1.SwiftImagePublishReady:
			fmt.Fprintf(out, "Pushed %s\n  digest:      %s\n  disk size:   %s\n  transferred: %s\n  signed:      %t\n",
				p.Status.Reference, p.Status.Digest, humanizeBytes(p.Status.SizeBytes), humanizeBytes(p.Status.TransferredBytes), p.Status.Signed)
			if p.Status.Image != "" {
				fmt.Fprintf(out, "  image:       SwiftImage %s/%s\n", p.Namespace, p.Status.Image)
			}
			return nil
		case imagev1alpha1.SwiftImagePublishFailed:
			return fmt.Errorf("publish failed (%s): %s", reason, msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s; the publish is %s (%s)", timeout, p.Status.Phase, reason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}
