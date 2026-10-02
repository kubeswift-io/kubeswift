package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// crdScope is what the catalog needs from a CRD: its scope and served versions.
type crdScope struct {
	namespaced bool
	served     map[string]bool
}

// chartCRDScopes reads the CRDs the chart installs, keyed by group/plural.
func chartCRDScopes(t *testing.T) map[string]crdScope {
	t.Helper()
	paths, err := filepath.Glob("../../charts/kubeswift/crds/*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no chart CRDs found: %v", err)
	}
	out := map[string]crdScope{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		var crd struct {
			Spec struct {
				Group string `json:"group"`
				Scope string `json:"scope"`
				Names struct {
					Plural string `json:"plural"`
				} `json:"names"`
				Versions []struct {
					Name   string `json:"name"`
					Served bool   `json:"served"`
				} `json:"versions"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		s := crdScope{namespaced: crd.Spec.Scope == "Namespaced", served: map[string]bool{}}
		for _, v := range crd.Spec.Versions {
			s.served[v.Name] = v.Served
		}
		out[crd.Spec.Group+"/"+crd.Spec.Names.Plural] = s
	}
	return out
}

// A KubeSwift kind whose catalog scope disagrees with its CRD makes the gateway
// build request paths the apiserver does not serve: SwiftGuestClass was marked
// namespaced, so creating, editing or deleting one from the UI failed (#704).
// Every KubeSwift entry must match the CRD the chart ships.
func TestResourceCatalog_KubeSwiftScopesMatchCRDs(t *testing.T) {
	crds := chartCRDScopes(t)
	for _, k := range resourceCatalog {
		if !strings.HasSuffix(k.gvr.Group, ".kubeswift.io") {
			continue
		}
		crd, ok := crds[k.gvr.Group+"/"+k.gvr.Resource]
		if !ok {
			t.Errorf("catalog entry %q (%s/%s): the chart ships no such CRD", k.key, k.gvr.Group, k.gvr.Resource)
			continue
		}
		if !crd.served[k.gvr.Version] {
			t.Errorf("catalog entry %q uses version %s, which its CRD does not serve", k.key, k.gvr.Version)
		}
		if k.namespaced != crd.namespaced {
			t.Errorf("catalog entry %q has namespaced=%v, but its CRD is namespaced=%v", k.key, k.namespaced, crd.namespaced)
		}
	}
}
