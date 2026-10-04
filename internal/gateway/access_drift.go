package gateway

import (
	"fmt"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
)

// A KubeSwift role is defined by its capabilities. Its ClusterRole holds the
// rules those capabilities granted when it was written, and the gateway never
// rewrites it on its own: every Access call runs as the signed-in user, and
// the gateway holds no RBAC-writing credential of its own (nor should it: to
// write a role, Kubernetes requires holding its rules). So when a release
// changes a capability's rules, a role created earlier keeps the old ones:
// v0.15.0 removed pods/exec from the console capability, and roles created
// before kept granting it. desiredRole and roleDrift let ListRoles report such
// a role and SyncRole or AssignRole bring it up to date.

// isKubeSwiftRole reports whether the ClusterRole is one the Access editor
// owns.
func isKubeSwiftRole(cr *rbacv1.ClusterRole) bool {
	return cr.Labels[roleLabel] == "true"
}

// desiredRole is the ClusterRole a KubeSwift role should be in this version: a
// predefined role's shipped composition, or a custom role's stored capability
// list composed afresh. It keeps nothing of cr but the name and, for a custom
// role, its display name and capabilities.
func desiredRole(cr *rbacv1.ClusterRole) *rbacv1.ClusterRole {
	if p := predefinedByName(cr.Name); p != nil {
		return clusterRoleForPredefined(p)
	}
	return clusterRoleFor(cr.Name, cr.Annotations[roleDisplayAnno], splitCSV(cr.Annotations[roleCapsAnno]), false)
}

// roleDrift describes how a role's ClusterRole differs from desired: the
// rules it grants that it should not, those it lacks, and a stale capability
// list. "" when it matches.
func roleDrift(actual, desired *rbacv1.ClusterRole) string {
	have, want := ruleAtoms(actual.Rules), ruleAtoms(desired.Rules)
	var parts []string
	if extra := minus(have, want); len(extra) > 0 {
		parts = append(parts, "grants rules its capabilities no longer include: "+describeAtoms(extra))
	}
	if missing := minus(want, have); len(missing) > 0 {
		parts = append(parts, "lacks rules its capabilities now include: "+describeAtoms(missing))
	}
	if len(parts) == 0 && actual.Annotations[roleCapsAnno] != desired.Annotations[roleCapsAnno] {
		parts = append(parts, fmt.Sprintf("its capability list is %q, not %q", actual.Annotations[roleCapsAnno], desired.Annotations[roleCapsAnno]))
	}
	return strings.Join(parts, "; ")
}

// syncedRole is actual with desired's rules and KubeSwift annotations; its
// other metadata (resourceVersion, any other labels or annotations) is kept
// so the update is a plain replace of what the editor owns.
func syncedRole(actual, desired *rbacv1.ClusterRole) *rbacv1.ClusterRole {
	out := actual.DeepCopy()
	out.Rules = desired.Rules
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	for k, v := range desired.Labels {
		out.Labels[k] = v
	}
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	for k, v := range desired.Annotations {
		out.Annotations[k] = v
	}
	return out
}

// ruleAtom is one permission: a verb on a resource (or non-resource URL).
type ruleAtom struct{ verb, group, resource, name string }

func ruleAtoms(rules []rbacv1.PolicyRule) map[ruleAtom]bool {
	out := map[ruleAtom]bool{}
	for _, r := range rules {
		names := r.ResourceNames
		if len(names) == 0 {
			names = []string{""}
		}
		for _, v := range r.Verbs {
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					for _, n := range names {
						out[ruleAtom{v, g, res, n}] = true
					}
				}
			}
			for _, u := range r.NonResourceURLs {
				out[ruleAtom{verb: v, resource: u}] = true
			}
		}
	}
	return out
}

func minus(a, b map[ruleAtom]bool) []ruleAtom {
	var out []ruleAtom
	for x := range a {
		if !b[x] {
			out = append(out, x)
		}
	}
	return out
}

// describeAtoms groups permissions per resource, e.g. "pods/exec (create),
// storage.kubeswift.io/swiftstoragelocations (get, list)".
func describeAtoms(atoms []ruleAtom) string {
	verbs := map[string][]string{}
	for _, a := range atoms {
		key := a.resource
		if a.group != "" {
			key = a.group + "/" + key
		}
		if a.name != "" {
			key += "/" + a.name
		}
		verbs[key] = append(verbs[key], a.verb)
	}
	keys := make([]string, 0, len(verbs))
	for k := range verbs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		sort.Strings(verbs[k])
		parts = append(parts, k+" ("+strings.Join(verbs[k], ", ")+")")
	}
	return strings.Join(parts, ", ")
}
