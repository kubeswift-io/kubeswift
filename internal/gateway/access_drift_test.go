package gateway

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"

	connect "connectrpc.com/connect"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
)

// oldConsoleRole is a custom role created before v0.15.0: its console
// capability then granted pods/exec, which it no longer does.
func oldConsoleRole() *rbacv1.ClusterRole {
	cr := clusterRoleFor("team-console", "Team console", []string{"view-vms", "console"}, false)
	cr.Rules = append(clusterRoleFor("x", "", []string{"view-vms"}, false).Rules,
		rule([]string{""}, []string{"pods/exec"}, []string{"create"}))
	cr.Labels["owner"] = "platform" // someone else's metadata, to be kept
	return cr
}

// oldAdminRole is the predefined Admin role as a release before
// manage-storage-locations wrote it.
func oldAdminRole() *rbacv1.ClusterRole {
	p := *predefinedByName("kubeswift-admin")
	var caps []string
	for _, c := range p.caps {
		if c != "manage-storage-locations" {
			caps = append(caps, c)
		}
	}
	p.caps = caps
	return clusterRoleForPredefined(&p)
}

func rolesByName(t *testing.T, svc *AccessService) map[string]*kubeswiftv1.Role {
	t.Helper()
	resp, err := svc.ListRoles(context.Background(), connect.NewRequest(&kubeswiftv1.ListRolesRequest{Cluster: "edge-1"}))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*kubeswiftv1.Role{}
	for _, r := range resp.Msg.Roles {
		out[r.Name] = r
	}
	return out
}

// ListRoles tells a role that grants what its capabilities grant from one
// that was written by an earlier release, and says how they differ.
func TestAccess_ListRoles_ReportsOutdated(t *testing.T) {
	current := clusterRoleFor("team-view", "Team view", []string{"view-vms"}, false)
	svc, _ := newFakeAccess(oldConsoleRole(), oldAdminRole(), current, clusterRoleForPredefined(predefinedByName("kubeswift-viewer")))
	roles := rolesByName(t, svc)

	if r := roles["team-console"]; !r.Outdated || !strings.Contains(r.OutdatedReason, "grants rules its capabilities no longer include: pods/exec (create)") ||
		!strings.Contains(r.OutdatedReason, "lacks rules its capabilities now include") || !strings.Contains(r.OutdatedReason, "swiftguests/console (create)") {
		t.Errorf("old console role: outdated=%v reason=%q", r.Outdated, r.OutdatedReason)
	}
	if r := roles["kubeswift-admin"]; !r.Outdated || !strings.Contains(r.OutdatedReason, "storage.kubeswift.io/swiftstoragelocations") {
		t.Errorf("old admin role: outdated=%v reason=%q", r.Outdated, r.OutdatedReason)
	}
	for _, name := range []string{"team-view", "kubeswift-viewer", "kubeswift-operator"} {
		if roles[name].Outdated {
			t.Errorf("%s is current (or not created yet), but reported outdated: %q", name, roles[name].OutdatedReason)
		}
	}
}

// SyncRole rewrites the rules and the capability list, keeps everything else
// on the object, and is a no-op the second time.
func TestAccess_SyncRole(t *testing.T) {
	svc, cs := newFakeAccess(oldConsoleRole(), oldAdminRole(),
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses"}})
	ctx := context.Background()
	sync := func(name string) error {
		_, err := svc.SyncRole(ctx, connect.NewRequest(&kubeswiftv1.SyncRoleRequest{Cluster: "edge-1", Name: name}))
		return err
	}
	for _, name := range []string{"team-console", "kubeswift-admin"} {
		if err := sync(name); err != nil {
			t.Fatalf("sync %s: %v", name, err)
		}
		cr, _ := cs.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
		if d := roleDrift(cr, desiredRole(cr)); d != "" {
			t.Errorf("%s still differs after sync: %s", name, d)
		}
	}
	cr, _ := cs.RbacV1().ClusterRoles().Get(ctx, "team-console", metav1.GetOptions{})
	if cr.Labels["owner"] != "platform" || cr.Annotations[roleDisplayAnno] != "Team console" {
		t.Errorf("sync dropped metadata it does not own: %+v", cr.ObjectMeta)
	}
	for _, r := range cr.Rules {
		for _, res := range r.Resources {
			if res == "pods/exec" {
				t.Error("the synced role still grants pods/exec")
			}
		}
	}
	if roles := rolesByName(t, svc); roles["team-console"].Outdated || roles["kubeswift-admin"].Outdated {
		t.Error("synced roles still listed as outdated")
	}
	updates := 0
	cs.PrependReactor("update", "clusterroles", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		return false, nil, nil
	})
	if err := sync("team-console"); err != nil || updates != 0 {
		t.Errorf("a current role must not be rewritten: err=%v updates=%d", err, updates)
	}
	if err := sync("someone-elses"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a ClusterRole that is not a KubeSwift role: err=%v", err)
	}
	if err := sync("kubeswift-operator"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a predefined role not created yet: err=%v", err)
	}
}

// Assigning a role brings it up to date first, so a binding never grants
// rules a capability has dropped.
func TestAccess_AssignRole_UpdatesAnOutdatedRole(t *testing.T) {
	svc, cs := newFakeAccess(oldConsoleRole())
	ctx := context.Background()
	if _, err := svc.AssignRole(ctx, connect.NewRequest(&kubeswiftv1.AssignRoleRequest{
		Cluster: "edge-1", Role: "team-console", Subject: &kubeswiftv1.Subject{Kind: "User", Name: "alice"},
	})); err != nil {
		t.Fatal(err)
	}
	cr, _ := cs.RbacV1().ClusterRoles().Get(ctx, "team-console", metav1.GetOptions{})
	if d := roleDrift(cr, desiredRole(cr)); d != "" {
		t.Errorf("the assigned role is still outdated: %s", d)
	}
}

// If the assigner may not update the outdated role, nothing is bound.
func TestAccess_AssignRole_RefusedWhenTheUpdateIsForbidden(t *testing.T) {
	svc, cs := newFakeAccess(oldConsoleRole())
	cs.PrependReactor("update", "clusterroles", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "clusterroles"}, "team-console",
			stderrors.New("user alice is attempting to grant RBAC permissions not currently held"))
	})
	ctx := context.Background()
	_, err := svc.AssignRole(ctx, connect.NewRequest(&kubeswiftv1.AssignRoleRequest{
		Cluster: "edge-1", Role: "team-console", Subject: &kubeswiftv1.Subject{Kind: "User", Name: "bob"},
	}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "out of date") || !strings.Contains(err.Error(), "pods/exec") {
		t.Fatalf("err = %v, want PermissionDenied naming the drift", err)
	}
	crbs, _ := cs.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if len(crbs.Items) != 0 {
		t.Errorf("a binding was created to the outdated role: %+v", crbs.Items)
	}
}

// A ClusterRole that merely has a predefined role's name, without the
// KubeSwift label, is someone else's: it is never bound.
func TestAccess_AssignRole_RefusesAForeignClusterRole(t *testing.T) {
	svc, cs := newFakeAccess(&rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "kubeswift-admin"},
		Rules:      []rbacv1.PolicyRule{rule([]string{"*"}, []string{"*"}, []string{"*"})},
	})
	ctx := context.Background()
	_, err := svc.AssignRole(ctx, connect.NewRequest(&kubeswiftv1.AssignRoleRequest{
		Cluster: "edge-1", Role: "kubeswift-admin", Subject: &kubeswiftv1.Subject{Kind: "User", Name: "bob"},
	}))
	if err == nil || !strings.Contains(err.Error(), "not a KubeSwift role") {
		t.Fatalf("err = %v", err)
	}
	if crbs, _ := cs.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{}); len(crbs.Items) != 0 {
		t.Error("the foreign ClusterRole was bound")
	}
}
