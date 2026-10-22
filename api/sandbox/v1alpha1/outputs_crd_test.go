package v1alpha1

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"
)

// spec.outputs is immutable without the webhook (off by default): the rule
// must be in the generated CRD the chart ships, and Exporting must be a
// phase the API server accepts.
func TestSwiftSandboxCRD_OutputsImmutableAndExportingPhase(t *testing.T) {
	raw, err := os.ReadFile("../../../charts/kubeswift/crds/sandbox.kubeswift.io_swiftsandboxes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	schema := crd["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	props := schema["properties"].(map[string]any)
	outputs := props["spec"].(map[string]any)["properties"].(map[string]any)["outputs"].(map[string]any)
	found := false
	for _, v := range outputs["x-kubernetes-validations"].([]any) {
		if v.(map[string]any)["rule"] == "self == oldSelf" {
			found = true
		}
	}
	if !found {
		t.Error("spec.outputs has no immutability rule in the chart CRD")
	}
	phases := props["status"].(map[string]any)["properties"].(map[string]any)["phase"].(map[string]any)["enum"].([]any)
	has := false
	for _, p := range phases {
		if p == string(SwiftSandboxExporting) {
			has = true
		}
	}
	if !has {
		t.Errorf("phase enum %v lacks Exporting", phases)
	}
}
