package cli

import (
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubeConfig holds kubeconfig-related options for swiftctl.
type KubeConfig struct {
	Kubeconfig string
	Context    string
}

// ToRESTConfig returns a rest.Config from the given options.
// Uses standard precedence: explicit path > KUBECONFIG env > default locations.
func (k *KubeConfig) ToRESTConfig() (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if k.Kubeconfig != "" {
		loadingRules.ExplicitPath = k.Kubeconfig
	}

	overrides := &clientcmd.ConfigOverrides{}
	if k.Context != "" {
		overrides.CurrentContext = k.Context
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		overrides,
	).ClientConfig()
	if err != nil {
		return nil, err
	}

	return config, nil
}

// ResolveKubeconfig returns the effective kubeconfig path for display.
func (k *KubeConfig) ResolveKubeconfig() string {
	if k.Kubeconfig != "" {
		return k.Kubeconfig
	}
	return clientcmd.RecommendedHomeFile
}
