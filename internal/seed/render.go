package seed

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// Render resolves userData, metaData, networkData from ResolvedGuest.Seed.
// Resolves Secret/ConfigMap refs when present; otherwise passes through inline strings.
func Render(ctx context.Context, c client.Client, namespace string, seed *resolved.Seed) (userData, metaData, networkData string, err error) {
	if seed == nil {
		return "", "", "", nil
	}
	userData, err = Resolve(ctx, c, namespace, seed.UserData, seed.UserDataFrom)
	if err != nil {
		return "", "", "", fmt.Errorf("userData: %w", err)
	}
	metaData, err = Resolve(ctx, c, namespace, seed.MetaData, seed.MetaDataFrom)
	if err != nil {
		return "", "", "", fmt.Errorf("metaData: %w", err)
	}
	networkData, err = Resolve(ctx, c, namespace, seed.NetworkData, seed.NetworkDataFrom)
	if err != nil {
		return "", "", "", fmt.Errorf("networkData: %w", err)
	}
	return userData, metaData, networkData, nil
}
