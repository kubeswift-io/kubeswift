package gateway

import (
	"context"
	"errors"
	"testing"

	connect "connectrpc.com/connect"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	rbacv1client "k8s.io/client-go/kubernetes/typed/rbac/v1"

	kubeswiftv1 "github.com/projectbeskar/kubeswift/gen/kubeswift/v1"
)

func newFakeAccess(objs ...runtime.Object) (*AccessService, *fake.Clientset) {
	cs := fake.NewSimpleClientset(objs...)
	svc := &AccessService{
		auth: NewInsecureAuthenticator(),
		rbacFor: func(cluster string, _ Identity) (rbacv1client.RbacV1Interface, error) {
			if cluster == "ghost" {
				return nil, errors.New("no client for ghost")
			}
			return cs.RbacV1(), nil
		},
	}
	return svc, cs
}

func TestAccess_ListCapabilities(t *testing.T) {
	svc, _ := newFakeAccess()
	resp, err := svc.ListCapabilities(context.Background(), connect.NewRequest(&kubeswiftv1.ListCapabilitiesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Capabilities) != len(capabilities) {
		t.Fatalf("got %d capabilities, want %d", len(resp.Msg.Capabilities), len(capabilities))
	}
	if resp.Msg.Capabilities[0].Key == "" || resp.Msg.Capabilities[0].DisplayName == "" {
		t.Errorf("capability missing key/displayName: %+v", resp.Msg.Capabilities[0])
	}
}

func TestAccess_ListRoles_PredefinedAlwaysPresent(t *testing.T) {
	// A custom KubeSwift role already on the cluster + an unrelated ClusterRole.
	custom := clusterRoleFor("team-readonly", "Team read-only", []string{"view-vms"}, false)
	unrelated := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "some-operator"}}
	svc, _ := newFakeAccess(custom, unrelated)

	resp, err := svc.ListRoles(context.Background(), connect.NewRequest(&kubeswiftv1.ListRolesRequest{Cluster: "boba"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Msg.Error)
	}
	byName := map[string]*kubeswiftv1.Role{}
	for _, r := range resp.Msg.Roles {
		byName[r.Name] = r
	}
	for _, p := range []string{"kubeswift-admin", "kubeswift-operator", "kubeswift-viewer"} {
		if byName[p] == nil || !byName[p].Predefined {
			t.Errorf("predefined role %q missing/not flagged predefined", p)
		}
	}
	if r := byName["team-readonly"]; r == nil || r.Predefined || len(r.Capabilities) != 1 || r.Capabilities[0] != "view-vms" {
		t.Errorf("custom role not surfaced correctly: %+v", r)
	}
	if byName["some-operator"] != nil {
		t.Errorf("an unlabelled ClusterRole must not be listed")
	}
}

func TestAccess_AssignPredefined_ClusterWide_EnsuresRole(t *testing.T) {
	svc, cs := newFakeAccess()
	ctx := context.Background()

	resp, err := svc.AssignRole(ctx, connect.NewRequest(&kubeswiftv1.AssignRoleRequest{
		Cluster: "boba", Role: "kubeswift-operator",
		Subject: &kubeswiftv1.Subject{Kind: "Group", Name: "team-a"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Assignment.Scope != "Cluster" {
		t.Errorf("scope = %q, want Cluster", resp.Msg.Assignment.Scope)
	}
	// The predefined ClusterRole was created on demand, from the model.
	cr, err := cs.RbacV1().ClusterRoles().Get(ctx, "kubeswift-operator", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("kubeswift-operator ClusterRole was not ensured: %v", err)
	}
	if cr.Labels[roleLabel] != "true" || cr.Annotations[rolePredefinedAnno] != "true" {
		t.Errorf("ensured role missing KubeSwift metadata: %+v", cr.ObjectMeta)
	}
	if len(cr.Rules) == 0 {
		t.Errorf("ensured role has no rules")
	}
	// A labelled ClusterRoleBinding to the right role + subject exists.
	crbs, _ := cs.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{LabelSelector: rbacBindingLabel + "=true"})
	if len(crbs.Items) != 1 {
		t.Fatalf("want 1 labelled ClusterRoleBinding, got %d", len(crbs.Items))
	}
	b := crbs.Items[0]
	if b.RoleRef.Name != "kubeswift-operator" || b.RoleRef.Kind != "ClusterRole" {
		t.Errorf("roleRef = %+v", b.RoleRef)
	}
	if len(b.Subjects) != 1 || b.Subjects[0].Kind != "Group" || b.Subjects[0].Name != "team-a" {
		t.Errorf("subjects = %+v", b.Subjects)
	}
}

func TestAccess_AssignNamespaced(t *testing.T) {
	svc, cs := newFakeAccess()
	ctx := context.Background()

	resp, err := svc.AssignRole(ctx, connect.NewRequest(&kubeswiftv1.AssignRoleRequest{
		Cluster: "boba", Role: "kubeswift-viewer", Namespace: "team-a",
		Subject: &kubeswiftv1.Subject{Kind: "User", Name: "alice@example.com"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Assignment.Scope != "Namespace" || resp.Msg.Assignment.Namespace != "team-a" {
		t.Errorf("assignment = %+v", resp.Msg.Assignment)
	}
	rbs, _ := cs.RbacV1().RoleBindings("team-a").List(ctx, metav1.ListOptions{LabelSelector: rbacBindingLabel + "=true"})
	if len(rbs.Items) != 1 || rbs.Items[0].RoleRef.Name != "kubeswift-viewer" {
		t.Fatalf("want 1 RoleBinding to kubeswift-viewer in team-a, got %+v", rbs.Items)
	}
}

func TestAccess_CreateRole_ComposesRules(t *testing.T) {
	svc, cs := newFakeAccess()
	ctx := context.Background()

	resp, err := svc.CreateRole(ctx, connect.NewRequest(&kubeswiftv1.CreateRoleRequest{
		Cluster: "boba", Name: "vm-console-only", DisplayName: "VM + console",
		Capabilities: []string{"view-vms", "console", "bogus-cap"}, // bogus dropped
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Role.Capabilities) != 2 {
		t.Errorf("capabilities = %v, want view-vms+console (bogus dropped)", resp.Msg.Role.Capabilities)
	}
	cr, err := cs.RbacV1().ClusterRoles().Get(ctx, "vm-console-only", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cr.Labels[roleLabel] != "true" || cr.Annotations[roleCapsAnno] != "view-vms,console" {
		t.Errorf("metadata = %+v", cr.ObjectMeta)
	}
	// Rules include pods/exec (console) — proof the capability rules were composed.
	found := false
	for _, r := range cr.Rules {
		for _, res := range r.Resources {
			if res == "pods/exec" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("composed role missing the console (pods/exec) rule: %+v", cr.Rules)
	}
}

func TestAccess_CreateRole_Validation(t *testing.T) {
	svc, _ := newFakeAccess()
	ctx := context.Background()
	// A predefined name is rejected.
	_, err := svc.CreateRole(ctx, connect.NewRequest(&kubeswiftv1.CreateRoleRequest{Cluster: "boba", Name: "kubeswift-admin", Capabilities: []string{"view-vms"}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("predefined name: want InvalidArgument, got %v", err)
	}
	// No valid capabilities is rejected.
	_, err = svc.CreateRole(ctx, connect.NewRequest(&kubeswiftv1.CreateRoleRequest{Cluster: "boba", Name: "empty", Capabilities: []string{"bogus"}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("no valid caps: want InvalidArgument, got %v", err)
	}
}

func TestAccess_ListAssignments_Roundtrip(t *testing.T) {
	svc, _ := newFakeAccess()
	ctx := context.Background()
	_, err := svc.AssignRole(ctx, connect.NewRequest(&kubeswiftv1.AssignRoleRequest{
		Cluster: "boba", Role: "kubeswift-admin",
		Subject: &kubeswiftv1.Subject{Kind: "User", Name: "root@example.com"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := svc.ListAssignments(ctx, connect.NewRequest(&kubeswiftv1.ListAssignmentsRequest{Cluster: "boba"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Assignments) != 1 {
		t.Fatalf("want 1 assignment, got %d", len(resp.Msg.Assignments))
	}
	a := resp.Msg.Assignments[0]
	if a.Role != "kubeswift-admin" || a.Subject.Name != "root@example.com" || a.Scope != "Cluster" {
		t.Errorf("assignment = %+v", a)
	}
}

func TestMapAccessErr(t *testing.T) {
	gr := schema.GroupResource{Group: "swift.kubeswift.io", Resource: "swiftguests"}
	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"forbidden->permission_denied", apierrors.NewForbidden(gr, "vm", errors.New("rbac")), connect.CodePermissionDenied},
		{"unauthorized->unauthenticated", apierrors.NewUnauthorized("token expired"), connect.CodeUnauthenticated},
		{"notfound->not_found", apierrors.NewNotFound(gr, "vm"), connect.CodeNotFound},
		{"alreadyexists->already_exists", apierrors.NewAlreadyExists(gr, "vm"), connect.CodeAlreadyExists},
		{"other->internal", errors.New("boom"), connect.CodeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := connect.CodeOf(mapAccessErr(c.err)); got != c.want {
				t.Errorf("mapAccessErr(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}
