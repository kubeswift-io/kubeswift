package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	connect "connectrpc.com/connect"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	rbacv1client "k8s.io/client-go/kubernetes/typed/rbac/v1"

	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
	"github.com/kubeswift-io/kubeswift/gen/kubeswift/v1/kubeswiftv1connect"
)

// rbacBindingLabel marks the (Cluster)RoleBindings the editor creates, so
// ListAssignments can find them without scanning every binding in the cluster.
const rbacBindingLabel = "kubeswift.io/access-binding"

// rbacClientFunc builds a typed RBAC client for a member, impersonating the
// end user. Injected so the service is unit-testable with a fake clientset.
type rbacClientFunc func(cluster string, id Identity) (rbacv1client.RbacV1Interface, error)

// AccessService is the UI RBAC editor's backend (decision A2): list/create the
// bindable KubeSwift roles (the three predefined + custom capability-composed
// ClusterRoles) and assign them to OIDC users/groups, cluster-wide or per
// namespace. Every call runs as the impersonated user, so Kubernetes RBAC gates
// who may edit access.
type AccessService struct {
	kubeswiftv1connect.UnimplementedAccessServiceHandler

	rbacFor rbacClientFunc
	auth    Authenticator
}

var _ kubeswiftv1connect.AccessServiceHandler = (*AccessService)(nil)

// NewAccessService wires the editor to the client pool + the authenticator.
func NewAccessService(pool *ClientPool, auth Authenticator) *AccessService {
	return &AccessService{
		auth: auth,
		rbacFor: func(cluster string, id Identity) (rbacv1client.RbacV1Interface, error) {
			cfg, err := pool.RestConfigFor(cluster, id)
			if err != nil {
				return nil, err
			}
			cs, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return nil, err
			}
			return cs.RbacV1(), nil
		},
	}
}

func (s *AccessService) ListCapabilities(ctx context.Context, req *connect.Request[kubeswiftv1.ListCapabilitiesRequest]) (*connect.Response[kubeswiftv1.ListCapabilitiesResponse], error) {
	if _, err := s.auth.Authenticate(ctx, req.Header()); err != nil {
		return nil, err
	}
	out := &kubeswiftv1.ListCapabilitiesResponse{}
	for _, c := range capabilities {
		out.Capabilities = append(out.Capabilities, &kubeswiftv1.Capability{Key: c.key, DisplayName: c.displayName, Description: c.description})
	}
	return connect.NewResponse(out), nil
}

// ListRoles returns the three predefined roles (always, synthesized from the
// capability model even before they exist on the cluster) plus any custom
// KubeSwift roles present. A failure to list custom roles surfaces as a
// ClusterError but never hides the predefined set.
func (s *AccessService) ListRoles(ctx context.Context, req *connect.Request[kubeswiftv1.ListRolesRequest]) (*connect.Response[kubeswiftv1.ListRolesResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster := req.Msg.GetCluster()
	if cluster == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster is required"))
	}
	out := &kubeswiftv1.ListRolesResponse{}
	for _, p := range predefinedRoles {
		out.Roles = append(out.Roles, &kubeswiftv1.Role{Name: p.name, DisplayName: p.displayName, Predefined: true, Capabilities: p.caps})
	}
	if rc, err := s.rbacFor(cluster, id); err != nil {
		out.Error = &kubeswiftv1.ClusterError{Cluster: cluster, Message: err.Error()}
	} else if list, err := rc.ClusterRoles().List(ctx, metav1.ListOptions{LabelSelector: roleLabel + "=true"}); err != nil {
		out.Error = &kubeswiftv1.ClusterError{Cluster: cluster, Message: err.Error()}
	} else {
		for i := range list.Items {
			cr := &list.Items[i]
			if predefinedByName(cr.Name) != nil {
				continue // already emitted (canonical synthesized form)
			}
			out.Roles = append(out.Roles, &kubeswiftv1.Role{
				Name:         cr.Name,
				DisplayName:  cr.Annotations[roleDisplayAnno],
				Predefined:   false,
				Capabilities: splitCSV(cr.Annotations[roleCapsAnno]),
			})
		}
	}
	return connect.NewResponse(out), nil
}

func (s *AccessService) CreateRole(ctx context.Context, req *connect.Request[kubeswiftv1.CreateRoleRequest]) (*connect.Response[kubeswiftv1.CreateRoleResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster, name := req.Msg.GetCluster(), req.Msg.GetName()
	if cluster == "" || name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster and name are required"))
	}
	if predefinedByName(name) != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%q is a predefined role name", name))
	}
	caps := validCapabilities(req.Msg.GetCapabilities())
	if len(caps) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("at least one valid capability is required"))
	}
	rc, err := s.rbacFor(cluster, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	cr := clusterRoleFor(name, req.Msg.GetDisplayName(), caps, false)
	if _, err := rc.ClusterRoles().Create(ctx, cr, metav1.CreateOptions{}); err != nil {
		return nil, mapAccessErr(err)
	}
	return connect.NewResponse(&kubeswiftv1.CreateRoleResponse{Role: &kubeswiftv1.Role{
		Name: name, DisplayName: req.Msg.GetDisplayName(), Predefined: false, Capabilities: caps,
	}}), nil
}

func (s *AccessService) DeleteRole(ctx context.Context, req *connect.Request[kubeswiftv1.DeleteRoleRequest]) (*connect.Response[kubeswiftv1.DeleteRoleResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster, name := req.Msg.GetCluster(), req.Msg.GetName()
	if cluster == "" || name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster and name are required"))
	}
	if predefinedByName(name) != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%q is a predefined role and cannot be deleted", name))
	}
	rc, err := s.rbacFor(cluster, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	// Only delete a labelled KubeSwift role — never an arbitrary ClusterRole.
	cr, err := rc.ClusterRoles().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, mapAccessErr(err)
	}
	if cr.Labels[roleLabel] != "true" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%q is not a KubeSwift role", name))
	}
	if err := rc.ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return nil, mapAccessErr(err)
	}
	return connect.NewResponse(&kubeswiftv1.DeleteRoleResponse{}), nil
}

// ListAssignments lists the editor-created bindings (labelled) across cluster
// scope + all namespaces, flattened to one Assignment per subject.
func (s *AccessService) ListAssignments(ctx context.Context, req *connect.Request[kubeswiftv1.ListAssignmentsRequest]) (*connect.Response[kubeswiftv1.ListAssignmentsResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster := req.Msg.GetCluster()
	if cluster == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster is required"))
	}
	rc, err := s.rbacFor(cluster, id)
	if err != nil {
		return connect.NewResponse(&kubeswiftv1.ListAssignmentsResponse{Error: &kubeswiftv1.ClusterError{Cluster: cluster, Message: err.Error()}}), nil
	}
	sel := metav1.ListOptions{LabelSelector: rbacBindingLabel + "=true"}
	out := &kubeswiftv1.ListAssignmentsResponse{}

	crbs, err := rc.ClusterRoleBindings().List(ctx, sel)
	if err != nil {
		return connect.NewResponse(&kubeswiftv1.ListAssignmentsResponse{Error: &kubeswiftv1.ClusterError{Cluster: cluster, Message: err.Error()}}), nil
	}
	for i := range crbs.Items {
		b := &crbs.Items[i]
		for _, subj := range b.Subjects {
			out.Assignments = append(out.Assignments, &kubeswiftv1.Assignment{
				BindingName: b.Name,
				Subject:     &kubeswiftv1.Subject{Kind: subj.Kind, Name: subj.Name},
				Role:        b.RoleRef.Name,
				Scope:       "Cluster",
			})
		}
	}
	rbs, err := rc.RoleBindings(metav1.NamespaceAll).List(ctx, sel)
	if err != nil {
		out.Error = &kubeswiftv1.ClusterError{Cluster: cluster, Message: err.Error()}
	} else {
		for i := range rbs.Items {
			b := &rbs.Items[i]
			for _, subj := range b.Subjects {
				out.Assignments = append(out.Assignments, &kubeswiftv1.Assignment{
					BindingName: b.Name,
					Subject:     &kubeswiftv1.Subject{Kind: subj.Kind, Name: subj.Name},
					Role:        b.RoleRef.Name,
					Scope:       "Namespace",
					Namespace:   b.Namespace,
				})
			}
		}
	}
	sortAssignments(out.Assignments)
	return connect.NewResponse(out), nil
}

// AssignRole binds a subject to a role. namespace "" => a ClusterRoleBinding;
// otherwise a RoleBinding in that namespace. The role's ClusterRole is ensured
// to exist first (predefined roles are created from the capability model on
// demand, so nothing needs pre-applying).
func (s *AccessService) AssignRole(ctx context.Context, req *connect.Request[kubeswiftv1.AssignRoleRequest]) (*connect.Response[kubeswiftv1.AssignRoleResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster := req.Msg.GetCluster()
	subj := req.Msg.GetSubject()
	role := req.Msg.GetRole()
	ns := req.Msg.GetNamespace()
	if cluster == "" || role == "" || subj == nil || subj.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster, role, and subject.name are required"))
	}
	kind := subj.GetKind()
	if kind != "User" && kind != "Group" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(`subject.kind must be "User" or "Group"`))
	}
	rc, err := s.rbacFor(cluster, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err := s.ensureRole(ctx, rc, role); err != nil {
		return nil, mapAccessErr(err)
	}
	roleRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role}
	subjects := []rbacv1.Subject{{Kind: kind, Name: subj.GetName(), APIGroup: rbacv1.GroupName}}
	meta := metav1.ObjectMeta{GenerateName: "kubeswift-access-", Labels: map[string]string{rbacBindingLabel: "true"}}

	assignment := &kubeswiftv1.Assignment{Subject: subj, Role: role}
	if ns == "" {
		created, err := rc.ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: meta, RoleRef: roleRef, Subjects: subjects}, metav1.CreateOptions{})
		if err != nil {
			return nil, mapAccessErr(err)
		}
		assignment.BindingName, assignment.Scope = created.Name, "Cluster"
	} else {
		meta.Namespace = ns
		created, err := rc.RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{ObjectMeta: meta, RoleRef: roleRef, Subjects: subjects}, metav1.CreateOptions{})
		if err != nil {
			return nil, mapAccessErr(err)
		}
		assignment.BindingName, assignment.Scope, assignment.Namespace = created.Name, "Namespace", ns
	}
	return connect.NewResponse(&kubeswiftv1.AssignRoleResponse{Assignment: assignment}), nil
}

func (s *AccessService) RemoveAssignment(ctx context.Context, req *connect.Request[kubeswiftv1.RemoveAssignmentRequest]) (*connect.Response[kubeswiftv1.RemoveAssignmentResponse], error) {
	id, err := s.auth.Authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	cluster, name, ns := req.Msg.GetCluster(), req.Msg.GetBindingName(), req.Msg.GetNamespace()
	if cluster == "" || name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cluster and binding_name are required"))
	}
	rc, err := s.rbacFor(cluster, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if ns == "" {
		err = rc.ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{})
	} else {
		err = rc.RoleBindings(ns).Delete(ctx, name, metav1.DeleteOptions{})
	}
	if err != nil {
		return nil, mapAccessErr(err)
	}
	return connect.NewResponse(&kubeswiftv1.RemoveAssignmentResponse{}), nil
}

// ensureRole makes sure the named ClusterRole exists; a predefined role missing
// from the cluster is created from the capability model. A custom role must
// already exist.
func (s *AccessService) ensureRole(ctx context.Context, rc rbacv1client.RbacV1Interface, name string) error {
	if _, err := rc.ClusterRoles().Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	p := predefinedByName(name)
	if p == nil {
		return fmt.Errorf("role %q does not exist", name)
	}
	if _, err := rc.ClusterRoles().Create(ctx, clusterRoleForPredefined(p), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// mapAccessErr maps a Kubernetes apimachinery error from an impersonated
// member-cluster call to the matching Connect code, so the UI can tell a
// permission denial (RBAC 403) apart from a generic server error. Used gateway-
// wide for impersonated reads/actions that carry NO validating webhook (reads,
// Explorer, Start/Stop). The webhook-heavy write paths (Create/Migrate/Delete)
// keep their own FailedPrecondition mapping on purpose: a k8s 403 there can be a
// webhook policy denial, not an RBAC denial, and the message carries the reason.
func mapAccessErr(err error) *connect.Error {
	switch {
	case apierrors.IsForbidden(err):
		return connect.NewError(connect.CodePermissionDenied, err)
	case apierrors.IsUnauthorized(err):
		return connect.NewError(connect.CodeUnauthenticated, err)
	case apierrors.IsAlreadyExists(err):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case apierrors.IsNotFound(err):
		return connect.NewError(connect.CodeNotFound, err)
	case apierrors.IsInvalid(err):
		return connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

func sortAssignments(as []*kubeswiftv1.Assignment) {
	sort.Slice(as, func(i, j int) bool {
		if as[i].GetRole() != as[j].GetRole() {
			return as[i].GetRole() < as[j].GetRole()
		}
		return as[i].GetSubject().GetName() < as[j].GetSubject().GetName()
	})
}

func joinCSV(s []string) string { return strings.Join(s, ",") }

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
