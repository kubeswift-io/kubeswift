package gateway

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1alpha1 "github.com/kubeswift-io/kubeswift/api/fleet/v1alpha1"
)

// The pool's own status write must not trigger another probe: each probe
// writes lastConnected, and re-probing on that update looped about once a
// second forever for any member more than a second away.
func TestStatusOnlyUpdate(t *testing.T) {
	base := func() *fleetv1alpha1.Cluster {
		return &fleetv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{
			Name: "edge-1", Namespace: "kubeswift-system", Generation: 3, ResourceVersion: "100",
		}}
	}

	statusWrite := base()
	statusWrite.ResourceVersion = "101"
	now := metav1.Now()
	statusWrite.Status.LastConnected = &now
	if !statusOnlyUpdate(base(), statusWrite) {
		t.Error("a status-only update re-probes the member")
	}

	specChange := base()
	specChange.ResourceVersion = "101"
	specChange.Generation = 4
	if statusOnlyUpdate(base(), specChange) {
		t.Error("a spec change must re-probe the member")
	}

	relabelled := base()
	relabelled.ResourceVersion = "101"
	relabelled.Labels = map[string]string{"region": "eu"}
	if statusOnlyUpdate(base(), relabelled) {
		t.Error("a metadata change must re-probe the member")
	}

	// An informer resync delivers the same object: that is the periodic
	// health re-check, so it still probes.
	if statusOnlyUpdate(base(), base()) {
		t.Error("a resync must re-probe the member")
	}
}
