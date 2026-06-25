package gateway

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KubeSwift roles are ordinary ClusterRoles marked + described by these
// metadata keys, so the editor can list them and reconstruct their capability
// set without a side database.
const (
	roleLabel          = "kubeswift.io/role" // "true" -> a bindable KubeSwift role
	roleDisplayAnno    = "kubeswift.io/role-display-name"
	roleCapsAnno       = "kubeswift.io/role-capabilities" // comma-joined capability keys
	rolePredefinedAnno = "kubeswift.io/role-predefined"   // "true" on the shipped roles
)

// capability is one granular permission the custom-role builder offers; each
// maps to a fixed set of RBAC rules (decision A2 — roles are real k8s RBAC).
type capability struct {
	key         string
	displayName string
	description string
	rules       []rbacv1.PolicyRule
}

func rule(groups, resources, verbs []string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{APIGroups: groups, Resources: resources, Verbs: verbs}
}

// capabilities is the curated catalog (confirmed granularity). Order is the
// display order in the UI.
var capabilities = []capability{
	{
		key: "view-vms", displayName: "View VMs",
		description: "Read SwiftGuests, pools, classes, migrations, and their Events (boot diagnostics).",
		rules: []rbacv1.PolicyRule{
			rule([]string{"swift.kubeswift.io"}, []string{"swiftguests", "swiftguestpools", "swiftguestclasses"}, []string{"get", "list", "watch"}),
			rule([]string{"migration.kubeswift.io"}, []string{"swiftmigrations"}, []string{"get", "list", "watch"}),
			rule([]string{""}, []string{"events"}, []string{"get", "list", "watch"}),
		},
	},
	{
		key: "manage-vms", displayName: "Manage VMs (start/stop)",
		description: "Start and stop SwiftGuests (patch runPolicy + delete the launcher pod).",
		rules: []rbacv1.PolicyRule{
			rule([]string{"swift.kubeswift.io"}, []string{"swiftguests"}, []string{"update", "patch"}),
			rule([]string{""}, []string{"pods"}, []string{"get", "list", "delete"}),
		},
	},
	{
		key: "console", displayName: "Console",
		description: "Open a VM serial console (exec into the launcher pod).",
		rules:       []rbacv1.PolicyRule{rule([]string{""}, []string{"pods/exec"}, []string{"create"})},
	},
	{
		key: "migrate", displayName: "Migrate",
		description: "Live/offline-migrate VMs between nodes.",
		rules: []rbacv1.PolicyRule{
			rule([]string{"migration.kubeswift.io"}, []string{"swiftmigrations"}, []string{"create"}),
			rule([]string{""}, []string{"nodes"}, []string{"list"}),
		},
	},
	{
		key: "manage-snapshots", displayName: "Manage snapshots",
		description: "Create and manage snapshots, restores, and schedules.",
		rules: []rbacv1.PolicyRule{
			rule([]string{"snapshot.kubeswift.io"}, []string{"swiftsnapshots", "swiftrestores", "swiftsnapshotschedules"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}),
		},
	},
	{
		key: "view-resources", displayName: "View cluster resources",
		description: "Browse nodes, namespaces, networking, storage, config, and KubeSwift CRDs in the Explorer (excludes Secret contents).",
		rules: []rbacv1.PolicyRule{
			rule([]string{""}, []string{"nodes", "namespaces", "pods", "services", "persistentvolumeclaims", "persistentvolumes", "configmaps"}, []string{"get", "list"}),
			rule([]string{"storage.k8s.io"}, []string{"storageclasses"}, []string{"get", "list"}),
			rule([]string{"networking.k8s.io"}, []string{"networkpolicies"}, []string{"get", "list"}),
			rule([]string{"k8s.cni.cncf.io"}, []string{"network-attachment-definitions"}, []string{"get", "list"}),
			rule([]string{"image.kubeswift.io"}, []string{"swiftimages"}, []string{"get", "list"}),
			rule([]string{"kernel.kubeswift.io"}, []string{"swiftkernels"}, []string{"get", "list"}),
			rule([]string{"seed.kubeswift.io"}, []string{"swiftseedprofiles"}, []string{"get", "list"}),
			rule([]string{"snapshot.kubeswift.io"}, []string{"swiftsnapshots", "swiftsnapshotschedules", "swiftrestores"}, []string{"get", "list"}),
			rule([]string{"gpu.kubeswift.io"}, []string{"swiftgpunodes", "swiftgpuprofiles"}, []string{"get", "list"}),
		},
	},
	{
		key: "view-secrets", displayName: "View secrets",
		description: "List Secret metadata in the Explorer (names + types; the gateway never exposes values).",
		rules:       []rbacv1.PolicyRule{rule([]string{""}, []string{"secrets"}, []string{"get", "list"})},
	},
	{
		key: "manage-rbac", displayName: "Manage access (RBAC)",
		description: "Create roles and assign them to users (manage Kubernetes RBAC).",
		rules: []rbacv1.PolicyRule{
			rule([]string{"rbac.authorization.k8s.io"}, []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"}, []string{"get", "list", "watch", "create", "update", "patch", "delete", "bind", "escalate"}),
		},
	},
}

// predefinedRole is a shipped role = a fixed capability composition.
type predefinedRole struct {
	name        string
	displayName string
	caps        []string
}

// predefinedRoles are the three roles the UI offers out of the box. The gateway
// ensures the matching ClusterRole exists (from the capability model) the first
// time one is assigned, so operators need not pre-apply anything.
var predefinedRoles = []predefinedRole{
	{"kubeswift-viewer", "View-only", []string{"view-vms", "view-resources"}},
	{"kubeswift-operator", "Operator", []string{"view-vms", "manage-vms", "console", "migrate", "manage-snapshots", "view-resources"}},
	{"kubeswift-admin", "Admin", []string{"view-vms", "manage-vms", "console", "migrate", "manage-snapshots", "view-resources", "view-secrets", "manage-rbac"}},
}

func capabilityByKey(key string) *capability {
	for i := range capabilities {
		if capabilities[i].key == key {
			return &capabilities[i]
		}
	}
	return nil
}

func predefinedByName(name string) *predefinedRole {
	for i := range predefinedRoles {
		if predefinedRoles[i].name == name {
			return &predefinedRoles[i]
		}
	}
	return nil
}

// validCapabilities keeps only known keys, preserving the catalog order (so the
// stored capability list is canonical regardless of UI input order).
func validCapabilities(keys []string) []string {
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	var out []string
	for _, c := range capabilities {
		if want[c.key] {
			out = append(out, c.key)
		}
	}
	return out
}

// rulesForCapabilities composes the RBAC rules for a capability set (in catalog
// order; unknown keys ignored).
func rulesForCapabilities(keys []string) []rbacv1.PolicyRule {
	var rules []rbacv1.PolicyRule
	for _, k := range validCapabilities(keys) {
		if c := capabilityByKey(k); c != nil {
			rules = append(rules, c.rules...)
		}
	}
	return rules
}

// clusterRoleFor builds the ClusterRole object for a role name + capability set,
// stamped with the KubeSwift role metadata so ListRoles can round-trip it.
func clusterRoleFor(name, displayName string, caps []string, predefined bool) *rbacv1.ClusterRole {
	caps = validCapabilities(caps)
	labels := map[string]string{roleLabel: "true"}
	annos := map[string]string{
		roleDisplayAnno: displayName,
		roleCapsAnno:    joinCSV(caps),
	}
	if predefined {
		annos[rolePredefinedAnno] = "true"
	}
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annos},
		Rules:      rulesForCapabilities(caps),
	}
}

func clusterRoleForPredefined(p *predefinedRole) *rbacv1.ClusterRole {
	return clusterRoleFor(p.name, p.displayName, p.caps, true)
}
