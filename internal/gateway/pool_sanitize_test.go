package gateway

import (
	"testing"

	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// A member kubeconfig must not resolve credentials against the gateway's own
// filesystem or an external command; those let a registered Cluster exfiltrate
// the gateway's ServiceAccount token or run code as the gateway. Inline data is
// fine.
func TestSanitizeMemberRESTConfig(t *testing.T) {
	reject := map[string]*rest.Config{
		"tokenFile":        {BearerTokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token"},
		"exec plugin":      {ExecProvider: &clientcmdapi.ExecConfig{Command: "/bin/sh"}},
		"auth provider":    {AuthProvider: &clientcmdapi.AuthProviderConfig{Name: "gcp"}},
		"client cert file": {TLSClientConfig: rest.TLSClientConfig{CertFile: "/etc/x.crt", KeyFile: "/etc/x.key"}},
		"ca file":          {TLSClientConfig: rest.TLSClientConfig{CAFile: "/etc/ca.crt"}},
	}
	for name, cfg := range reject {
		if err := sanitizeMemberRESTConfig(cfg); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}

	allow := map[string]*rest.Config{
		"inline token": {Host: "https://member:6443", BearerToken: "abc"},
		"inline certs": {Host: "https://member:6443", TLSClientConfig: rest.TLSClientConfig{
			CertData: []byte("cert"), KeyData: []byte("key"), CAData: []byte("ca"),
		}},
	}
	for name, cfg := range allow {
		if err := sanitizeMemberRESTConfig(cfg); err != nil {
			t.Errorf("%s: expected accept, got %v", name, err)
		}
	}
}
