package v1alpha1

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const specImmutableMessage = "spec is immutable except deletionPolicy and ttl"

// specRules returns the x-kubernetes-validations rules on a CRD's .spec, in
// its first version, keyed by message.
func specRules(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Validations []struct {
									Rule    string `json:"rule"`
									Message string `json:"message"`
								} `json:"x-kubernetes-validations"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := map[string]string{}
	for _, v := range crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Validations {
		out[v.Message] = v.Rule
	}
	return out
}

// Restore and clone read the backend from a snapshot's spec when they run, so
// the apiserver must refuse a spec edit even with the webhook off (#707). The
// rule names fields, so a field added to SwiftSnapshotSpec must be added to it
// (or deliberately left mutable here).
func TestSwiftSnapshotCRD_SpecImmutableRule(t *testing.T) {
	mutable := map[string]bool{"deletionPolicy": true, "ttl": true}
	for _, dir := range []string{"../../../config/crd/bases", "../../../charts/kubeswift/crds"} {
		path := dir + "/snapshot.kubeswift.io_swiftsnapshots.yaml"
		var rule string
		for msg, r := range specRules(t, path) {
			if strings.HasPrefix(msg, specImmutableMessage) {
				rule = r
			}
		}
		if rule == "" {
			t.Errorf("%s: no spec immutability rule", path)
			continue
		}
		typ := reflect.TypeOf(SwiftSnapshotSpec{})
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if mutable[name] {
				if strings.Contains(rule, "self."+name) {
					t.Errorf("%s: the rule freezes %s, which is meant to stay mutable", path, name)
				}
				continue
			}
			if !strings.Contains(rule, "self."+name) || !strings.Contains(rule, "oldSelf."+name) {
				t.Errorf("%s: spec.%s is not covered by the immutability rule", path, name)
			}
		}
	}
}

// A schedule's template is a SwiftSnapshotSpec too, and must stay editable:
// the rule belongs on SwiftSnapshot's spec field, not on the type.
func TestSwiftSnapshotScheduleCRD_TemplateStaysMutable(t *testing.T) {
	for _, dir := range []string{"../../../config/crd/bases", "../../../charts/kubeswift/crds"} {
		path := dir + "/snapshot.kubeswift.io_swiftsnapshotschedules.yaml"
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if strings.Contains(string(raw), "oldSelf") {
			t.Errorf("%s: carries a transition rule; a schedule's template must stay editable", path)
		}
	}
}
