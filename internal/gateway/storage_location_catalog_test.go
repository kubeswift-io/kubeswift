package gateway

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestStorageLocationProject(t *testing.T) {
	cluster := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"default": true, "oci": map[string]interface{}{"repository": "registry.example.com/kubeswift"}},
		"status": map[string]interface{}{"conditions": []interface{}{
			map[string]interface{}{"type": "Valid", "status": "True"},
			map[string]interface{}{"type": "Ready", "status": "False"},
			map[string]interface{}{"type": "Reachable", "status": "True"},
		}},
	}}
	got := storageLocationProject(cluster)
	want := map[string]string{"default": "true", "repository": "registry.example.com/kubeswift", "ready": "False", "reachable": "True"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	// A csi-only namespace location: no registry, no probe.
	ns := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"csi": map[string]interface{}{"volumeSnapshotClassName": "fast"}},
	}}
	got = storageLocationProject(ns)
	if got["default"] != "false" || got["repository"] != "class fast" || got["ready"] != "" || got["reachable"] != "" {
		t.Errorf("csi-only location: %v", got)
	}
}

// Reading the locations is part of browsing the cluster; writing them is its
// own capability, which the shipped Admin role includes.
func TestStorageLocationCapabilities(t *testing.T) {
	has := func(capKey, verb string) bool {
		c := capabilityByKey(capKey)
		if c == nil {
			return false
		}
		for _, r := range c.rules {
			if len(r.APIGroups) == 1 && r.APIGroups[0] == "storage.kubeswift.io" && len(r.Resources) == 2 {
				for _, v := range r.Verbs {
					if v == verb {
						return true
					}
				}
			}
		}
		return false
	}
	if !has("view-resources", "list") || has("view-resources", "create") {
		t.Error("view-resources must read storage locations and not write them")
	}
	if !has("manage-storage-locations", "create") {
		t.Error("manage-storage-locations must write them")
	}
	admin := predefinedByName("kubeswift-admin")
	found := false
	for _, c := range admin.caps {
		found = found || c == "manage-storage-locations"
	}
	if !found {
		t.Error("the Admin role must manage storage locations")
	}
}
