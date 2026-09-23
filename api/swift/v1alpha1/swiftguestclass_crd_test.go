package v1alpha1

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"
)

// crdField is the part of a generated CRD this test reads.
type crdField struct {
	Default     *bool `json:"default"`
	Validations []struct {
		Rule    string `json:"rule"`
		Message string `json:"message"`
	} `json:"x-kubernetes-validations"`
}

type guestClassCRD struct {
	Spec struct {
		Versions []struct {
			Name   string `json:"name"`
			Schema struct {
				OpenAPIV3Schema struct {
					Properties struct {
						Spec struct {
							Properties struct {
								SharedBaseDisk crdField `json:"sharedBaseDisk"`
							} `json:"properties"`
						} `json:"spec"`
					} `json:"properties"`
				} `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

// sharedBaseDisk is immutable at the apiserver, through a CEL rule in the
// generated CRD. There is no webhook behind it, so a regeneration that dropped
// the marker would take the guarantee with it and say nothing — and flipping the
// field on a class in use moves every guest's root disk at its next restart.
//
// Both copies are checked: the chart ships its own, and that is the one an
// operator installs.
func TestSwiftGuestClassCRD_SharedBaseDiskIsImmutable(t *testing.T) {
	for _, path := range []string{
		"../../../config/crd/bases/swift.kubeswift.io_swiftguestclasses.yaml",
		"../../../charts/kubeswift/crds/swift.kubeswift.io_swiftguestclasses.yaml",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var crd guestClassCRD
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(crd.Spec.Versions) == 0 {
			t.Fatalf("%s: no versions", path)
		}
		for _, v := range crd.Spec.Versions {
			f := v.Schema.OpenAPIV3Schema.Properties.Spec.Properties.SharedBaseDisk
			// The rule is a transition rule, and a transition rule is skipped
			// when the old value is absent. The default is what makes the field
			// always present, so it is part of the guarantee, not decoration.
			if f.Default == nil || *f.Default {
				t.Errorf("%s (%s): sharedBaseDisk default = %v, want false; without it the "+
					"immutability rule is skipped on a class that never set the field", path, v.Name, f.Default)
			}
			found := false
			for _, val := range f.Validations {
				if val.Rule == "self == oldSelf" {
					found = true
					if val.Message == "" {
						t.Errorf("%s (%s): the immutability rule has no message, so a rejected "+
							"change would not say why", path, v.Name)
					}
				}
			}
			if !found {
				t.Errorf("%s (%s): sharedBaseDisk has no `self == oldSelf` rule (validations: %+v); "+
					"nothing stops a class in use being flipped", path, v.Name, f.Validations)
			}
		}
	}
}
