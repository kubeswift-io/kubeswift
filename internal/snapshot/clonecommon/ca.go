package clonecommon

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/storagelocation"
)

// RegistryCAEnv is the env var a snapshot-oras Job reads PEM CA certificates
// from, trusted in addition to the system roots (internal/oci.RegistryCAEnv;
// a test keeps the two equal without the controller linking that package).
const RegistryCAEnv = "KUBESWIFT_REGISTRY_CA_BUNDLE"

// RegistryCAEnvVars passes bundle to a snapshot-oras container; none when it
// is empty.
func RegistryCAEnvVars(bundle string) []corev1.EnvVar {
	if bundle == "" {
		return nil
	}
	return []corev1.EnvVar{{Name: RegistryCAEnv, Value: bundle}}
}

// TransferCA is the CA bundle a Job that reads or deletes a snapshot's
// artifacts trusts: the one recorded at capture, plus the one its location
// holds now when it differs, so a registry whose CA was rotated after the push
// still restores. A location that is gone leaves the recorded bundle, which
// is why it is recorded at all.
func TransferCA(ctx context.Context, c client.Reader, snap *snapshotv1alpha1.SwiftSnapshot) (string, error) {
	conn, ok := SnapshotOCI(snap)
	if !ok || snap.Status.Location == nil {
		return conn.CABundle, nil
	}
	current, err := storagelocation.CurrentCABundle(ctx, c, snap.Namespace, snap.Status.Location.Source)
	if err != nil {
		return "", err
	}
	return combineCA(conn.CABundle, current), nil
}

func combineCA(recorded, current string) string {
	r, cur := strings.TrimSpace(recorded), strings.TrimSpace(current)
	switch {
	case cur == "" || cur == r:
		return recorded
	case r == "":
		return current
	}
	return r + "\n" + cur + "\n"
}
