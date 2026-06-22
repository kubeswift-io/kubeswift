// Package gateway implements the kubeswift-gateway: the browser-facing Connect
// hub that fronts the KubeSwift operator across a fleet of member clusters. See
// docs/design/ui-backend-enablement.md.
package gateway

import (
	"google.golang.org/protobuf/types/known/timestamppb"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1alpha1 "github.com/projectbeskar/kubeswift/api/fleet/v1alpha1"
	kubeswiftv1 "github.com/projectbeskar/kubeswift/gen/kubeswift/v1"
)

// clusterToProto maps a fleet.kubeswift.io Cluster to the UI-shaped proto view.
// The conditions are flattened into the ready/reachable booleans the cluster
// selector renders, while the full condition list is preserved for detail.
func clusterToProto(c *fleetv1alpha1.Cluster) *kubeswiftv1.Cluster {
	out := &kubeswiftv1.Cluster{
		Name:              c.Name,
		Namespace:         c.Namespace,
		DisplayName:       c.Spec.DisplayName,
		Server:            c.Spec.Server,
		KubernetesVersion: c.Status.KubernetesVersion,
		Ready:             apimeta.IsStatusConditionTrue(c.Status.Conditions, fleetv1alpha1.ClusterConditionReady),
		Reachable:         apimeta.IsStatusConditionTrue(c.Status.Conditions, fleetv1alpha1.ClusterConditionReachable),
	}
	if out.DisplayName == "" {
		out.DisplayName = c.Name
	}
	if c.Status.GuestCount != nil {
		out.GuestCount = *c.Status.GuestCount
	}
	if c.Status.LastConnected != nil {
		out.LastConnected = timestamppb.New(c.Status.LastConnected.Time)
	}
	for i := range c.Status.Conditions {
		out.Conditions = append(out.Conditions, conditionToProto(&c.Status.Conditions[i]))
	}
	return out
}

// conditionToProto flattens a Kubernetes status condition into the proto form.
func conditionToProto(c *metav1.Condition) *kubeswiftv1.Condition {
	out := &kubeswiftv1.Condition{
		Type:    c.Type,
		Status:  string(c.Status),
		Reason:  c.Reason,
		Message: c.Message,
	}
	if !c.LastTransitionTime.IsZero() {
		out.LastTransitionTime = timestamppb.New(c.LastTransitionTime.Time)
	}
	return out
}
