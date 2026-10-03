// Package oci holds the KubeSwift golden-image (P3) OCI transfer core: the
// sparse, zero-skipping, content-addressed disk chunking used by both the
// in-cluster snapshot-oras transfer Job and the client-side `swiftctl image
// publish` command. It is an importable package precisely so both `package
// main` binaries can share one implementation (Go forbids importing one main
// from another).
package oci

import (
	"fmt"
	"net/http"

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
	httpClient := retry.DefaultClient
	if tr := configuredTransport(); tr != nil {
		// A private CA (SetRegistryCA): the system roots plus the bundle.
		httpClient = &http.Client{Transport: retry.NewTransport(tr)}
	}
	if credStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		repo.Client = &auth.Client{
			Client:     httpClient,
			Cache:      auth.NewCache(),
			Credential: credentials.Credential(credStore),
		}
	} else if httpClient != retry.DefaultClient {
		repo.Client = &auth.Client{Client: httpClient, Cache: auth.NewCache()}
	}
	return repo, nil
}
