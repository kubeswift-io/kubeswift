package gateway

import (
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// resourceKind is one cluster-explorer catalog entry (decision E2): its GVR,
// where it sits in the nav, the ordered column keys it emits, and the projector
// that fills them from an object.
type resourceKind struct {
	key         string
	displayName string
	gvr         schema.GroupVersionResource
	namespaced  bool
	category    string
	columns     []string
	project     func(*unstructured.Unstructured) map[string]string
}

func gvr(group, version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

// resourceCatalog is the static, server-owned set of browsable kinds. SwiftGuests
// and SwiftMigrations are intentionally absent — they have dedicated UI views.
// Adding a kind here is all it takes for the UI nav to surface it (E2).
var resourceCatalog = []resourceKind{
	// Cluster-scoped.
	{key: "nodes", displayName: "Nodes", gvr: gvr("", "v1", "nodes"), namespaced: false, category: "Cluster", columns: []string{"status", "roles", "version", "internalIP"}, project: nodeProject},
	{key: "namespaces", displayName: "Namespaces", gvr: gvr("", "v1", "namespaces"), namespaced: false, category: "Cluster", columns: []string{"status"}, project: phaseStatusProject},
	{key: "storageclasses", displayName: "Storage Classes", gvr: gvr("storage.k8s.io", "v1", "storageclasses"), namespaced: false, category: "Cluster", columns: []string{"provisioner", "reclaimPolicy", "volumeBindingMode"}, project: storageClassProject},
	{key: "persistentvolumes", displayName: "Persistent Volumes", gvr: gvr("", "v1", "persistentvolumes"), namespaced: false, category: "Cluster", columns: []string{"status", "capacity", "storageClass", "claim"}, project: pvProject},

	// Workloads.
	{key: "pods", displayName: "Pods", gvr: gvr("", "v1", "pods"), namespaced: true, category: "Workloads", columns: []string{"status", "ready", "node", "ip"}, project: podProject},

	// Networking.
	{key: "services", displayName: "Services", gvr: gvr("", "v1", "services"), namespaced: true, category: "Networking", columns: []string{"type", "clusterIP", "ports"}, project: serviceProject},
	{key: "network-attachment-definitions", displayName: "Network Attachments", gvr: gvr("k8s.cni.cncf.io", "v1", "network-attachment-definitions"), namespaced: true, category: "Networking", columns: nil, project: nilProject},
	{key: "networkpolicies", displayName: "Network Policies", gvr: gvr("networking.k8s.io", "v1", "networkpolicies"), namespaced: true, category: "Networking", columns: []string{"podSelector"}, project: netpolProject},

	// Storage.
	{key: "persistentvolumeclaims", displayName: "Persistent Volume Claims", gvr: gvr("", "v1", "persistentvolumeclaims"), namespaced: true, category: "Storage", columns: []string{"status", "capacity", "storageClass", "volumeMode"}, project: pvcProject},

	// Config.
	{key: "secrets", displayName: "Secrets", gvr: gvr("", "v1", "secrets"), namespaced: true, category: "Config", columns: []string{"type", "keys"}, project: secretProject},
	{key: "configmaps", displayName: "Config Maps", gvr: gvr("", "v1", "configmaps"), namespaced: true, category: "Config", columns: []string{"keys"}, project: configMapProject},

	// KubeSwift CRDs without a dedicated view.
	{key: "swiftimages", displayName: "Images", gvr: gvr("image.kubeswift.io", "v1alpha1", "swiftimages"), namespaced: true, category: "KubeSwift", columns: []string{"phase"}, project: phaseStatusProject},
	{key: "swiftkernels", displayName: "Kernels", gvr: gvr("kernel.kubeswift.io", "v1alpha1", "swiftkernels"), namespaced: true, category: "KubeSwift", columns: []string{"phase"}, project: phaseStatusProject},
	{key: "swiftguestclasses", displayName: "Guest Classes", gvr: gvr("swift.kubeswift.io", "v1alpha1", "swiftguestclasses"), namespaced: true, category: "KubeSwift", columns: nil, project: nilProject},
	{key: "swiftguestpools", displayName: "Guest Pools", gvr: gvr("swift.kubeswift.io", "v1alpha1", "swiftguestpools"), namespaced: true, category: "KubeSwift", columns: []string{"phase", "replicas"}, project: poolProject},
	{key: "swiftseedprofiles", displayName: "Seed Profiles", gvr: gvr("seed.kubeswift.io", "v1alpha1", "swiftseedprofiles"), namespaced: true, category: "KubeSwift", columns: nil, project: nilProject},
	{key: "swiftsnapshots", displayName: "Snapshots", gvr: gvr("snapshot.kubeswift.io", "v1alpha1", "swiftsnapshots"), namespaced: true, category: "KubeSwift", columns: []string{"phase"}, project: phaseStatusProject},
	{key: "swiftsnapshotschedules", displayName: "Snapshot Schedules", gvr: gvr("snapshot.kubeswift.io", "v1alpha1", "swiftsnapshotschedules"), namespaced: true, category: "KubeSwift", columns: nil, project: nilProject},
	{key: "swiftrestores", displayName: "Restores", gvr: gvr("snapshot.kubeswift.io", "v1alpha1", "swiftrestores"), namespaced: true, category: "KubeSwift", columns: []string{"phase"}, project: phaseStatusProject},
	{key: "swiftgpunodes", displayName: "GPU Nodes", gvr: gvr("gpu.kubeswift.io", "v1alpha1", "swiftgpunodes"), namespaced: false, category: "KubeSwift", columns: []string{"phase", "gpus", "free"}, project: gpuNodeProject},
	{key: "swiftgpuprofiles", displayName: "GPU Profiles", gvr: gvr("gpu.kubeswift.io", "v1alpha1", "swiftgpuprofiles"), namespaced: true, category: "KubeSwift", columns: nil, project: nilProject},
}

func lookupKind(key string) *resourceKind {
	for i := range resourceCatalog {
		if resourceCatalog[i].key == key {
			return &resourceCatalog[i]
		}
	}
	return nil
}

// --- projectors -----------------------------------------------------------
//
// Each returns the kind-specific display columns. Keep them small and total:
// missing fields project to "" rather than erroring.

func nilProject(*unstructured.Unstructured) map[string]string { return map[string]string{} }

// phaseStatusProject reports status.phase under both the "phase" and "status"
// keys, so it serves Namespaces (column "status") and the phase-bearing CRDs
// (column "phase") from one function.
func phaseStatusProject(u *unstructured.Unstructured) map[string]string {
	p := nestedStr(u, "status", "phase")
	return map[string]string{"phase": p, "status": p}
}

func nodeProject(u *unstructured.Unstructured) map[string]string {
	m := map[string]string{}
	status := "NotReady"
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if ok && cm["type"] == "Ready" && cm["status"] == "True" {
			status = "Ready"
		}
	}
	if nestedBool(u, "spec", "unschedulable") {
		status += ",SchedulingDisabled"
	}
	m["status"] = status

	var roles []string
	for k := range u.GetLabels() {
		if r := strings.TrimPrefix(k, "node-role.kubernetes.io/"); r != k && r != "" {
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	if len(roles) == 0 {
		m["roles"] = "<none>"
	} else {
		m["roles"] = strings.Join(roles, ",")
	}

	m["version"] = nestedStr(u, "status", "nodeInfo", "kubeletVersion")
	addrs, _, _ := unstructured.NestedSlice(u.Object, "status", "addresses")
	for _, a := range addrs {
		am, ok := a.(map[string]interface{})
		if ok && am["type"] == "InternalIP" {
			m["internalIP"], _ = am["address"].(string)
			break
		}
	}
	return m
}

func podProject(u *unstructured.Unstructured) map[string]string {
	cs, _, _ := unstructured.NestedSlice(u.Object, "status", "containerStatuses")
	ready := 0
	for _, c := range cs {
		if cm, ok := c.(map[string]interface{}); ok {
			if r, _ := cm["ready"].(bool); r {
				ready++
			}
		}
	}
	return map[string]string{
		"status": nestedStr(u, "status", "phase"),
		"ready":  strconv.Itoa(ready) + "/" + strconv.Itoa(len(cs)),
		"node":   nestedStr(u, "spec", "nodeName"),
		"ip":     nestedStr(u, "status", "podIP"),
	}
}

func serviceProject(u *unstructured.Unstructured) map[string]string {
	ports, _, _ := unstructured.NestedSlice(u.Object, "spec", "ports")
	var ps []string
	for _, p := range ports {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		proto, _ := pm["protocol"].(string)
		if proto == "" {
			proto = "TCP"
		}
		ps = append(ps, numToStr(pm["port"])+"/"+proto)
	}
	return map[string]string{
		"type":      nestedStr(u, "spec", "type"),
		"clusterIP": nestedStr(u, "spec", "clusterIP"),
		"ports":     strings.Join(ps, ","),
	}
}

func netpolProject(u *unstructured.Unstructured) map[string]string {
	sel, _, _ := unstructured.NestedStringMap(u.Object, "spec", "podSelector", "matchLabels")
	if len(sel) == 0 {
		return map[string]string{"podSelector": "<all pods>"}
	}
	parts := make([]string, 0, len(sel))
	for k, v := range sel {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return map[string]string{"podSelector": strings.Join(parts, ",")}
}

func pvcProject(u *unstructured.Unstructured) map[string]string {
	capacity := nestedStr(u, "status", "capacity", "storage")
	if capacity == "" {
		capacity = nestedStr(u, "spec", "resources", "requests", "storage")
	}
	return map[string]string{
		"status":       nestedStr(u, "status", "phase"),
		"capacity":     capacity,
		"storageClass": nestedStr(u, "spec", "storageClassName"),
		"volumeMode":   nestedStr(u, "spec", "volumeMode"),
	}
}

func pvProject(u *unstructured.Unstructured) map[string]string {
	claim := ""
	if name := nestedStr(u, "spec", "claimRef", "name"); name != "" {
		if ns := nestedStr(u, "spec", "claimRef", "namespace"); ns != "" {
			claim = ns + "/" + name
		} else {
			claim = name
		}
	}
	return map[string]string{
		"status":       nestedStr(u, "status", "phase"),
		"capacity":     nestedStr(u, "spec", "capacity", "storage"),
		"storageClass": nestedStr(u, "spec", "storageClassName"),
		"claim":        claim,
	}
}

func storageClassProject(u *unstructured.Unstructured) map[string]string {
	return map[string]string{
		"provisioner":       nestedStr(u, "provisioner"),
		"reclaimPolicy":     nestedStr(u, "reclaimPolicy"),
		"volumeBindingMode": nestedStr(u, "volumeBindingMode"),
	}
}

// secretProject emits metadata ONLY — type + the data key names — and never
// reads a value (decision E4). The data map's values stay untouched.
func secretProject(u *unstructured.Unstructured) map[string]string {
	return map[string]string{
		"type": nestedStr(u, "type"),
		"keys": dataKeyNames(u, "data"),
	}
}

func configMapProject(u *unstructured.Unstructured) map[string]string {
	keys := dataKeyNames(u, "data")
	if bin := dataKeyNames(u, "binaryData"); bin != "" {
		if keys == "" {
			keys = bin
		} else {
			keys += "," + bin
		}
	}
	return map[string]string{"keys": keys}
}

func poolProject(u *unstructured.Unstructured) map[string]string {
	replicas := ""
	if r, ok, _ := unstructured.NestedInt64(u.Object, "spec", "replicas"); ok {
		replicas = strconv.FormatInt(r, 10)
	}
	return map[string]string{"phase": nestedStr(u, "status", "phase"), "replicas": replicas}
}

func gpuNodeProject(u *unstructured.Unstructured) map[string]string {
	return map[string]string{
		"phase": nestedStr(u, "status", "phase"),
		"gpus":  nestedIntStr(u, "status", "gpuCount"),
		"free":  nestedIntStr(u, "status", "freeGPUs"),
	}
}

// --- helpers --------------------------------------------------------------

func nestedStr(u *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, fields...)
	return s
}

func nestedIntStr(u *unstructured.Unstructured, fields ...string) string {
	if n, ok, _ := unstructured.NestedInt64(u.Object, fields...); ok {
		return strconv.FormatInt(n, 10)
	}
	return ""
}

// dataKeyNames returns the sorted key names of a map field (e.g. a Secret's or
// ConfigMap's "data"). It reads only the keys, never the values.
func dataKeyNames(u *unstructured.Unstructured, field string) string {
	data, _, _ := unstructured.NestedMap(u.Object, field)
	if len(data) == 0 {
		return ""
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func numToStr(v interface{}) string {
	switch n := v.(type) {
	case int64:
		return strconv.FormatInt(n, 10)
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case string:
		return n
	}
	return ""
}
