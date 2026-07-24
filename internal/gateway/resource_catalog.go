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
	{key: "deployments", displayName: "Deployments", gvr: gvr("apps", "v1", "deployments"), namespaced: true, category: "Workloads", columns: []string{"ready", "uptodate", "available"}, project: deploymentProject},
	{key: "statefulsets", displayName: "Stateful Sets", gvr: gvr("apps", "v1", "statefulsets"), namespaced: true, category: "Workloads", columns: []string{"ready"}, project: replicaReadyProject},
	{key: "daemonsets", displayName: "Daemon Sets", gvr: gvr("apps", "v1", "daemonsets"), namespaced: true, category: "Workloads", columns: []string{"desired", "ready"}, project: daemonSetProject},
	{key: "replicasets", displayName: "Replica Sets", gvr: gvr("apps", "v1", "replicasets"), namespaced: true, category: "Workloads", columns: []string{"ready"}, project: replicaReadyProject},
	{key: "jobs", displayName: "Jobs", gvr: gvr("batch", "v1", "jobs"), namespaced: true, category: "Workloads", columns: []string{"completions", "status"}, project: jobProject},
	{key: "cronjobs", displayName: "Cron Jobs", gvr: gvr("batch", "v1", "cronjobs"), namespaced: true, category: "Workloads", columns: []string{"schedule", "suspend", "lastRun"}, project: scheduleProject},
	{key: "pods", displayName: "Pods", gvr: gvr("", "v1", "pods"), namespaced: true, category: "Workloads", columns: []string{"status", "ready", "node", "ip"}, project: podProject},

	// Networking.
	{key: "services", displayName: "Services", gvr: gvr("", "v1", "services"), namespaced: true, category: "Networking", columns: []string{"type", "clusterIP", "ports"}, project: serviceProject},
	{key: "ingresses", displayName: "Ingresses", gvr: gvr("networking.k8s.io", "v1", "ingresses"), namespaced: true, category: "Networking", columns: []string{"class", "hosts"}, project: ingressProject},
	{key: "network-attachment-definitions", displayName: "Network Attachments", gvr: gvr("k8s.cni.cncf.io", "v1", "network-attachment-definitions"), namespaced: true, category: "Networking", columns: nil, project: nilProject},
	{key: "networkpolicies", displayName: "Network Policies", gvr: gvr("networking.k8s.io", "v1", "networkpolicies"), namespaced: true, category: "Networking", columns: []string{"podSelector"}, project: netpolProject},

	// Storage.
	{key: "persistentvolumeclaims", displayName: "Persistent Volume Claims", gvr: gvr("", "v1", "persistentvolumeclaims"), namespaced: true, category: "Storage", columns: []string{"status", "capacity", "storageClass", "volumeMode"}, project: pvcProject},

	// Config.
	{key: "secrets", displayName: "Secrets", gvr: gvr("", "v1", "secrets"), namespaced: true, category: "Config", columns: []string{"type", "keys"}, project: secretProject},
	{key: "configmaps", displayName: "Config Maps", gvr: gvr("", "v1", "configmaps"), namespaced: true, category: "Config", columns: []string{"keys"}, project: configMapProject},

	// Access control (RBAC + identities).
	{key: "serviceaccounts", displayName: "Service Accounts", gvr: gvr("", "v1", "serviceaccounts"), namespaced: true, category: "Access", columns: []string{"secrets"}, project: serviceAccountProject},
	{key: "roles", displayName: "Roles", gvr: gvr("rbac.authorization.k8s.io", "v1", "roles"), namespaced: true, category: "Access", columns: []string{"rules"}, project: ruleCountProject},
	{key: "rolebindings", displayName: "Role Bindings", gvr: gvr("rbac.authorization.k8s.io", "v1", "rolebindings"), namespaced: true, category: "Access", columns: []string{"role", "subjects"}, project: roleBindingProject},
	{key: "clusterroles", displayName: "Cluster Roles", gvr: gvr("rbac.authorization.k8s.io", "v1", "clusterroles"), namespaced: false, category: "Access", columns: []string{"rules"}, project: ruleCountProject},
	{key: "clusterrolebindings", displayName: "Cluster Role Bindings", gvr: gvr("rbac.authorization.k8s.io", "v1", "clusterrolebindings"), namespaced: false, category: "Access", columns: []string{"role", "subjects"}, project: roleBindingProject},

	// KubeSwift CRDs without a dedicated view.
	{key: "swiftimages", displayName: "Images", gvr: gvr("image.kubeswift.io", "v1alpha1", "swiftimages"), namespaced: true, category: "KubeSwift", columns: []string{"phase"}, project: phaseStatusProject},
	{key: "swiftkernels", displayName: "Kernels", gvr: gvr("kernel.kubeswift.io", "v1alpha1", "swiftkernels"), namespaced: true, category: "KubeSwift", columns: []string{"phase"}, project: phaseStatusProject},
	{key: "swiftguestclasses", displayName: "Guest Classes", gvr: gvr("swift.kubeswift.io", "v1alpha1", "swiftguestclasses"), namespaced: true, category: "KubeSwift", columns: nil, project: nilProject},
	{key: "swiftguestpools", displayName: "Guest Pools", gvr: gvr("swift.kubeswift.io", "v1alpha1", "swiftguestpools"), namespaced: true, category: "KubeSwift", columns: []string{"phase", "replicas"}, project: poolProject},
	{key: "swiftsandboxes", displayName: "Sandboxes", gvr: gvr("sandbox.kubeswift.io", "v1alpha1", "swiftsandboxes"), namespaced: true, category: "KubeSwift", columns: []string{"phase", "image", "node", "ip"}, project: sandboxProject},
	{key: "swiftsandboxpools", displayName: "Sandbox Pools", gvr: gvr("sandbox.kubeswift.io", "v1alpha1", "swiftsandboxpools"), namespaced: true, category: "KubeSwift", columns: []string{"phase", "warm", "claimed", "min"}, project: sandboxPoolProject},
	{key: "swiftseedprofiles", displayName: "Seed Profiles", gvr: gvr("seed.kubeswift.io", "v1alpha1", "swiftseedprofiles"), namespaced: true, category: "KubeSwift", columns: nil, project: nilProject},
	{key: "swiftsnapshots", displayName: "Snapshots", gvr: gvr("snapshot.kubeswift.io", "v1alpha1", "swiftsnapshots"), namespaced: true, category: "KubeSwift", columns: []string{"guest", "backend", "phase"}, project: snapshotProject},
	{key: "swiftsnapshotschedules", displayName: "Snapshot Schedules", gvr: gvr("snapshot.kubeswift.io", "v1alpha1", "swiftsnapshotschedules"), namespaced: true, category: "KubeSwift", columns: []string{"schedule", "suspend", "lastRun"}, project: scheduleProject},
	{key: "swiftrestores", displayName: "Restores", gvr: gvr("snapshot.kubeswift.io", "v1alpha1", "swiftrestores"), namespaced: true, category: "KubeSwift", columns: []string{"snapshot", "target", "phase"}, project: restoreProject},
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

// snapshotProject reports a SwiftSnapshot's source guest, backend type, and phase.
func snapshotProject(u *unstructured.Unstructured) map[string]string {
	return map[string]string{
		"guest":   nestedStr(u, "spec", "guestRef", "name"),
		"backend": nestedStr(u, "spec", "backend", "type"),
		"phase":   nestedStr(u, "status", "phase"),
	}
}

// restoreProject reports a SwiftRestore's source snapshot, target guest, and phase.
func restoreProject(u *unstructured.Unstructured) map[string]string {
	return map[string]string{
		"snapshot": nestedStr(u, "spec", "snapshotRef", "name"),
		"target":   nestedStr(u, "spec", "targetGuest", "name"),
		"phase":    nestedStr(u, "status", "phase"),
	}
}

// scheduleProject reports a SwiftSnapshotSchedule's cron, suspend flag, and last run.
func scheduleProject(u *unstructured.Unstructured) map[string]string {
	suspend := "false"
	if b, ok, _ := unstructured.NestedBool(u.Object, "spec", "suspend"); ok && b {
		suspend = "true"
	}
	return map[string]string{
		"schedule": nestedStr(u, "spec", "schedule"),
		"suspend":  suspend,
		"lastRun":  nestedStr(u, "status", "lastScheduleTime"),
	}
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

// sandboxProject reports a SwiftSandbox's phase, image, node, and guest IP.
func sandboxProject(u *unstructured.Unstructured) map[string]string {
	return map[string]string{
		"phase": nestedStr(u, "status", "phase"),
		"image": nestedStr(u, "spec", "image"),
		"node":  nestedStr(u, "status", "nodeName"),
		"ip":    nestedStr(u, "status", "network", "primaryIP"),
	}
}

// sandboxPoolProject reports a SwiftSandboxPool's phase, warm/claimed slot counts,
// and the configured minimum warm buffer.
func sandboxPoolProject(u *unstructured.Unstructured) map[string]string {
	return map[string]string{
		"phase":   nestedStr(u, "status", "phase"),
		"warm":    nestedIntStr(u, "status", "warmReplicas"),
		"claimed": nestedIntStr(u, "status", "claimedReplicas"),
		"min":     nestedIntStr(u, "spec", "minWarm"),
	}
}

// deploymentProject reports a Deployment's ready/desired, up-to-date, and
// available replica counts.
func deploymentProject(u *unstructured.Unstructured) map[string]string {
	return map[string]string{
		"ready":     readyOverDesired(u),
		"uptodate":  nestedIntStr(u, "status", "updatedReplicas"),
		"available": nestedIntStr(u, "status", "availableReplicas"),
	}
}

// replicaReadyProject reports ready/desired for StatefulSets and ReplicaSets.
func replicaReadyProject(u *unstructured.Unstructured) map[string]string {
	return map[string]string{"ready": readyOverDesired(u)}
}

// daemonSetProject reports a DaemonSet's desired and ready scheduled counts.
func daemonSetProject(u *unstructured.Unstructured) map[string]string {
	return map[string]string{
		"desired": nestedIntStr(u, "status", "desiredNumberScheduled"),
		"ready":   nestedIntStr(u, "status", "numberReady"),
	}
}

// jobProject reports a Job's succeeded/desired completions and a coarse status.
func jobProject(u *unstructured.Unstructured) map[string]string {
	succeeded := nestedIntStr(u, "status", "succeeded")
	if succeeded == "" {
		succeeded = "0"
	}
	completions := nestedIntStr(u, "spec", "completions")
	if completions == "" {
		completions = "1"
	}
	status := "Running"
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]interface{})
		if !ok || cm["status"] != "True" {
			continue
		}
		switch cm["type"] {
		case "Complete":
			status = "Complete"
		case "Failed":
			status = "Failed"
		}
	}
	return map[string]string{"completions": succeeded + "/" + completions, "status": status}
}

// ingressProject reports an Ingress's class and the hosts it routes.
func ingressProject(u *unstructured.Unstructured) map[string]string {
	rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "rules")
	var hosts []string
	for _, r := range rules {
		if rm, ok := r.(map[string]interface{}); ok {
			if h, _ := rm["host"].(string); h != "" {
				hosts = append(hosts, h)
			}
		}
	}
	return map[string]string{
		"class": nestedStr(u, "spec", "ingressClassName"),
		"hosts": strings.Join(hosts, ","),
	}
}

// serviceAccountProject reports how many secrets a ServiceAccount references.
func serviceAccountProject(u *unstructured.Unstructured) map[string]string {
	secrets, _, _ := unstructured.NestedSlice(u.Object, "secrets")
	return map[string]string{"secrets": strconv.Itoa(len(secrets))}
}

// ruleCountProject reports the number of policy rules on a (Cluster)Role.
func ruleCountProject(u *unstructured.Unstructured) map[string]string {
	rules, _, _ := unstructured.NestedSlice(u.Object, "rules")
	return map[string]string{"rules": strconv.Itoa(len(rules))}
}

// roleBindingProject reports a (Cluster)RoleBinding's target role + subject count.
func roleBindingProject(u *unstructured.Unstructured) map[string]string {
	subjects, _, _ := unstructured.NestedSlice(u.Object, "subjects")
	return map[string]string{
		"role":     nestedStr(u, "roleRef", "name"),
		"subjects": strconv.Itoa(len(subjects)),
	}
}

// readyOverDesired renders "<status.readyReplicas>/<spec.replicas>" with sane
// defaults (ready→0, desired→1) — shared by Deployments/StatefulSets/ReplicaSets.
func readyOverDesired(u *unstructured.Unstructured) string {
	ready := nestedIntStr(u, "status", "readyReplicas")
	if ready == "" {
		ready = "0"
	}
	desired := nestedIntStr(u, "spec", "replicas")
	if desired == "" {
		desired = "1"
	}
	return ready + "/" + desired
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
