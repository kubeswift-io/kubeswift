// Package oci holds the KubeSwift golden-image (P3) OCI transfer core: the
// sparse, zero-skipping, content-addressed disk chunking used by both the
// in-cluster snapshot-oras transfer Job and the client-side `swiftctl image
// publish` command. It is an importable package precisely so both `package
// main` binaries can share one implementation (Go forbids importing one main
// from another). See docs/design/oras-golden-image.md.
package oci

import (
	"fmt"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// NewRepository builds an authenticated ORAS remote for repoRef. Credentials
// come from the Docker config (DOCKER_CONFIG / ~/.docker/config.json — the
// dockerconfigjson pull-secret the controller mounts in-cluster, or the
// operator's `docker login` client-side); anonymous when absent. insecure
// switches to plaintext HTTP for an in-cluster / test registry — UNSAFE, and
// cosign VERIFY is unsupported over plaintext (see Sign / the design note).
func NewRepository(repoRef string, insecure bool) (*remote.Repository, error) {
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("repository %q: %w", repoRef, err)
	}
	repo.PlainHTTP = insecure
	if credStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		repo.Client = &auth.Client{
			Client:     retry.DefaultClient,
			Cache:      auth.NewCache(),
			Credential: credentials.Credential(credStore),
		}
	}
	return repo, nil
}
