package swiftguest

import (
	"strconv"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
	"github.com/kubeswift-io/kubeswift/internal/sharedbase"
)

// A pool is a large, preallocated piece of a node's disk. A guest pinned to a
// node that was never offered for that must not build one there — the case that
// would otherwise put one on a control-plane node and take it into
// DiskPressure.
func TestReconcile_SharedBaseGuest_RefusesAnUnlabelledPinnedNode(t *testing.T) {
	objs := sharedBaseFixtures()
	objs[0].(*swiftv1alpha1.SwiftGuest).Spec.NodeName = "worker-1"
	c := guestClientBuilder(append(objs, node("worker-1"))...).WithStatusSubresource(&batchv1.Job{}).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	cond := guestCondition(t, got, ConditionStorageReady)
	if cond.Reason != reasonBaseDiskNodeIneligible || !strings.Contains(cond.Message, sharedbase.NodeLabel) {
		t.Errorf("StorageReady = %s %q; want it to name the label the node lacks", cond.Reason, cond.Message)
	}
	var jobs batchv1.JobList
	if err := c.List(t.Context(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("a materialise Job was created for an unlabelled node (%d)", len(jobs.Items))
	}
}

// With no node offered at all, say so — rather than leave a Job pending on a
// scheduler that can never place it.
func TestReconcile_SharedBaseGuest_RefusesWhenNoNodeIsLabelled(t *testing.T) {
	c := guestClientBuilder(append(sharedBaseFixtures(), node("worker-1"), node("worker-2"))...).
		WithStatusSubresource(&batchv1.Job{}).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	cond := guestCondition(t, got, ConditionStorageReady)
	if cond.Reason != reasonBaseDiskNoNode || !strings.Contains(cond.Message, "kubectl label node") {
		t.Errorf("StorageReady = %s %q; want it to say how to offer a node", cond.Reason, cond.Message)
	}
	var jobs batchv1.JobList
	if err := c.List(t.Context(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("a materialise Job was created with nowhere to run it (%d)", len(jobs.Items))
	}
}

// The Job carries the selector as well as the gate above, so a node that loses
// the label takes no new disk even when the guest is bound straight to it.
func TestMaterialiseJob_SelectsAnEligibleNode(t *testing.T) {
	c := guestClientBuilder(append(sharedBaseFixtures(), basediskNode("worker-1"))...).
		WithStatusSubresource(&batchv1.Job{}).Build()
	if _, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}); err != nil {
		t.Fatal(err)
	}
	sel := materialiseJobOf(t, c).Spec.Template.Spec.NodeSelector
	if sel[sharedbase.NodeLabel] != sharedbase.NodeLabelValue {
		t.Errorf("nodeSelector = %v, want %s=%s", sel, sharedbase.NodeLabel, sharedbase.NodeLabelValue)
	}
}

// A disk that exists outlives the label: its node cannot be swapped, so a guest
// already built keeps running whatever the label says now.
func TestReconcile_SharedBaseGuest_KeepsRunningWhenItsNodeLosesTheLabel(t *testing.T) {
	c := guestClientBuilder(append(guestWithDisk("worker-1"), node("worker-1"))...). // unlabelled now
												WithStatusSubresource(&batchv1.Job{}).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if cond := guestCondition(t, got, ConditionStorageReady); cond.Status != "True" {
		t.Fatalf("StorageReady = %s %s %q; a built disk must not be withdrawn by a label change",
			cond.Status, cond.Reason, cond.Message)
	}
	if len(launchers(t, c)) != 1 {
		t.Error("the guest lost its launcher when its node lost the label")
	}
}

func TestPoolSize(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		want      uint64
		wantErr   string
	}{
		{name: "unset", env: "", want: defaultPoolSize},
		{name: "quantity", env: "60Gi", want: 60 << 30},
		{name: "plain bytes", env: "1000000", want: 1000000},
		{name: "not a quantity", env: "40GB!", wantErr: "not a quantity"},
		{name: "zero", env: "0", wantErr: "greater than zero"},
		{name: "negative", env: "-5Gi", wantErr: "greater than zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(PoolSizeEnv, tc.env)
			got, err := PoolSize()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one saying %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// The configured size reaches the node command that creates the pool.
func TestMaterialiseJob_CarriesTheConfiguredPoolSize(t *testing.T) {
	t.Setenv(PoolSizeEnv, "60Gi")
	c := guestClientBuilder(append(sharedBaseFixtures(), basediskNode("worker-1"))...).
		WithStatusSubresource(&batchv1.Job{}).Build()
	if _, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}); err != nil {
		t.Fatal(err)
	}
	want := "--pool-data-bytes=" + strconv.FormatUint(60<<30, 10)
	args := materialiseJobOf(t, c).Spec.Template.Spec.Containers[0].Args
	found := false
	for _, a := range args {
		if a == want {
			found = true
		}
	}
	if !found {
		t.Errorf("args %v, want %s", args, want)
	}
}
