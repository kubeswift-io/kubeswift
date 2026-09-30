package v1alpha1

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// storageRuleMessage identifies StorageSpec's CEL rule in a generated CRD.
const storageRuleMessage = "accessMode=ReadWriteMany requires volumeMode=Block; Filesystem RWX is not live-migration-capable"

// storageRules collects the rule of every x-kubernetes-validations entry that
// carries StorageSpec's message, wherever in the schema StorageSpec appears.
func storageRules(node interface{}, found *[]string) {
	switch n := node.(type) {
	case map[string]interface{}:
		if vals, ok := n["x-kubernetes-validations"].([]interface{}); ok {
			for _, v := range vals {
				if m, ok := v.(map[string]interface{}); ok && m["message"] == storageRuleMessage {
					rule, _ := m["rule"].(string)
					*found = append(*found, rule)
				}
			}
		}
		for _, child := range n {
			storageRules(child, found)
		}
	case []interface{}:
		for _, child := range n {
			storageRules(child, found)
		}
	}
}

// Lab validation of v0.15.0: a SwiftGuestClass whose storage set only
// storageClassName was refused with "no such key: accessMode". CEL fails on
// an absent optional field rather than reading it as unset, so the rule must
// test has(self.accessMode) before comparing it. Its other operand already
// guards volumeMode the same way.
//
// StorageSpec is in the guest, its class and a pool's template; each CRD has
// its copy, and the chart ships its own set, which is what an operator installs.
func TestStorageSpecCRDRule_GuardsAbsentAccessMode(t *testing.T) {
	for _, dir := range []string{"../../../config/crd/bases", "../../../charts/kubeswift/crds"} {
		for _, crd := range []string{"swiftguests", "swiftguestclasses", "swiftguestpools"} {
			path := dir + "/swift.kubeswift.io_" + crd + ".yaml"
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			var doc map[string]interface{}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			var rules []string
			storageRules(doc, &rules)
			if len(rules) == 0 {
				t.Errorf("%s: no StorageSpec rule found; RWX Filesystem would be accepted", path)
			}
			for _, rule := range rules {
				if !strings.HasPrefix(rule, "!(has(self.accessMode) && ") {
					t.Errorf("%s: StorageSpec rule %q compares accessMode without has(self.accessMode); "+
						"a storage block without accessMode is refused with \"no such key\"", path, rule)
				}
			}
		}
	}
}
