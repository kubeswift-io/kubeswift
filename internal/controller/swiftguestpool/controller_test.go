package swiftguestpool

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// helper to create a SwiftGuest with just a name.
func makeGuest(name string) swiftv1alpha1.SwiftGuest {
	return swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

// ---------------------------------------------------------------------------
// buildIndexMap tests
// ---------------------------------------------------------------------------

func TestBuildIndexMap_Basic(t *testing.T) {
	guests := []swiftv1alpha1.SwiftGuest{
		makeGuest("mypool-0"),
		makeGuest("mypool-1"),
		makeGuest("mypool-2"),
	}
	m := buildIndexMap("mypool", guests)
	if len(m) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(m))
	}
	for _, idx := range []int{0, 1, 2} {
		if _, ok := m[idx]; !ok {
			t.Errorf("expected index %d in map", idx)
		}
	}
}

func TestBuildIndexMap_WithGap(t *testing.T) {
	guests := []swiftv1alpha1.SwiftGuest{
		makeGuest("pool-0"),
		makeGuest("pool-2"),
		makeGuest("pool-4"),
	}
	m := buildIndexMap("pool", guests)
	if len(m) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(m))
	}
	for _, idx := range []int{0, 2, 4} {
		if _, ok := m[idx]; !ok {
			t.Errorf("expected index %d in map", idx)
		}
	}
	for _, idx := range []int{1, 3} {
		if _, ok := m[idx]; ok {
			t.Errorf("did not expect index %d in map", idx)
		}
	}
}

func TestBuildIndexMap_InvalidName(t *testing.T) {
	guests := []swiftv1alpha1.SwiftGuest{
		makeGuest("not-pool-guest"),
		makeGuest("pool-abc"),
		makeGuest("pool-"),
	}
	m := buildIndexMap("pool", guests)
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(m))
	}
}

func TestBuildIndexMap_Empty(t *testing.T) {
	m := buildIndexMap("pool", nil)
	if len(m) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(m))
	}
}

func TestBuildIndexMap_DifferentPool(t *testing.T) {
	guests := []swiftv1alpha1.SwiftGuest{
		makeGuest("other-0"),
		makeGuest("other-1"),
	}
	m := buildIndexMap("mypool", guests)
	if len(m) != 0 {
		t.Fatalf("expected empty map for different pool, got %d entries", len(m))
	}
}

// ---------------------------------------------------------------------------
// findMissingIndices tests
// ---------------------------------------------------------------------------

func TestFindMissingIndices_NoneExist(t *testing.T) {
	m := map[int]swiftv1alpha1.SwiftGuest{}
	missing := findMissingIndices(m, 3)
	expected := []int{0, 1, 2}
	if !intSliceEqual(missing, expected) {
		t.Errorf("expected %v, got %v", expected, missing)
	}
}

func TestFindMissingIndices_AllExist(t *testing.T) {
	m := map[int]swiftv1alpha1.SwiftGuest{
		0: makeGuest("p-0"),
		1: makeGuest("p-1"),
		2: makeGuest("p-2"),
	}
	missing := findMissingIndices(m, 3)
	if len(missing) != 0 {
		t.Errorf("expected no missing indices, got %v", missing)
	}
}

func TestFindMissingIndices_Gap(t *testing.T) {
	m := map[int]swiftv1alpha1.SwiftGuest{
		0: makeGuest("p-0"),
		2: makeGuest("p-2"),
	}
	missing := findMissingIndices(m, 3)
	expected := []int{1}
	if !intSliceEqual(missing, expected) {
		t.Errorf("expected %v, got %v", expected, missing)
	}
}

func TestFindMissingIndices_ScaleUp(t *testing.T) {
	m := map[int]swiftv1alpha1.SwiftGuest{
		0: makeGuest("p-0"),
		1: makeGuest("p-1"),
		2: makeGuest("p-2"),
	}
	missing := findMissingIndices(m, 5)
	expected := []int{3, 4}
	if !intSliceEqual(missing, expected) {
		t.Errorf("expected %v, got %v", expected, missing)
	}
}

func TestFindMissingIndices_DesiredZero(t *testing.T) {
	m := map[int]swiftv1alpha1.SwiftGuest{}
	missing := findMissingIndices(m, 0)
	if len(missing) != 0 {
		t.Errorf("expected no missing indices for desired=0, got %v", missing)
	}
}

// ---------------------------------------------------------------------------
// findExcessIndices tests
// ---------------------------------------------------------------------------

func TestFindExcessIndices_None(t *testing.T) {
	m := map[int]swiftv1alpha1.SwiftGuest{
		0: makeGuest("p-0"),
		1: makeGuest("p-1"),
		2: makeGuest("p-2"),
	}
	excess := findExcessIndices(m, 3)
	if len(excess) != 0 {
		t.Errorf("expected no excess, got %v", excess)
	}
}

func TestFindExcessIndices_ScaleDown(t *testing.T) {
	m := map[int]swiftv1alpha1.SwiftGuest{
		0: makeGuest("p-0"),
		1: makeGuest("p-1"),
		2: makeGuest("p-2"),
		3: makeGuest("p-3"),
		4: makeGuest("p-4"),
	}
	excess := findExcessIndices(m, 3)
	expected := []int{4, 3}
	if !intSliceEqual(excess, expected) {
		t.Errorf("expected %v, got %v", expected, excess)
	}
}

func TestFindExcessIndices_ScaleToZero(t *testing.T) {
	m := map[int]swiftv1alpha1.SwiftGuest{
		0: makeGuest("p-0"),
		1: makeGuest("p-1"),
		2: makeGuest("p-2"),
	}
	excess := findExcessIndices(m, 0)
	expected := []int{2, 1, 0}
	if !intSliceEqual(excess, expected) {
		t.Errorf("expected %v, got %v", expected, excess)
	}
}

func TestFindExcessIndices_WithGap(t *testing.T) {
	m := map[int]swiftv1alpha1.SwiftGuest{
		0: makeGuest("p-0"),
		1: makeGuest("p-1"),
		3: makeGuest("p-3"),
		4: makeGuest("p-4"),
	}
	excess := findExcessIndices(m, 2)
	expected := []int{4, 3}
	if !intSliceEqual(excess, expected) {
		t.Errorf("expected %v, got %v", expected, excess)
	}
}

func TestFindExcessIndices_DesiredEqualsCurrent(t *testing.T) {
	m := map[int]swiftv1alpha1.SwiftGuest{
		0: makeGuest("p-0"),
	}
	excess := findExcessIndices(m, 1)
	if len(excess) != 0 {
		t.Errorf("expected no excess, got %v", excess)
	}
}

// ---------------------------------------------------------------------------
// helper functions tests (condBool, availableReason, progressingReason, setCondition)
// ---------------------------------------------------------------------------

func TestCondBool(t *testing.T) {
	if condBool(true) != metav1.ConditionTrue {
		t.Error("condBool(true) should return ConditionTrue")
	}
	if condBool(false) != metav1.ConditionFalse {
		t.Error("condBool(false) should return ConditionFalse")
	}
}

func TestAvailableReason(t *testing.T) {
	if r := availableReason(1); r != "MinimumReplicasAvailable" {
		t.Errorf("expected MinimumReplicasAvailable, got %s", r)
	}
	if r := availableReason(0); r != "NoReplicasAvailable" {
		t.Errorf("expected NoReplicasAvailable, got %s", r)
	}
}

func TestProgressingReason(t *testing.T) {
	tests := []struct {
		total, desired, failed int32
		want                   string
	}{
		{3, 3, 0, "ReplicasUpToDate"},
		{2, 3, 0, "NewReplicasCreated"},
		{4, 3, 0, "ScalingDown"},
		{3, 3, 1, "FailedReplicasExist"},
		{2, 3, 1, "FailedReplicasExist"}, // failed takes priority
	}
	for _, tt := range tests {
		got := progressingReason(tt.total, tt.desired, tt.failed)
		if got != tt.want {
			t.Errorf("progressingReason(%d, %d, %d) = %s, want %s",
				tt.total, tt.desired, tt.failed, got, tt.want)
		}
	}
}

func TestSetCondition_Append(t *testing.T) {
	var conditions []metav1.Condition
	now := metav1.Now()
	setCondition(&conditions, metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "MinimumReplicasAvailable",
		Message:            "1/1 replicas ready",
	})
	if len(conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(conditions))
	}
	if conditions[0].Type != "Available" {
		t.Errorf("expected type Available, got %s", conditions[0].Type)
	}
}

func TestSetCondition_Update(t *testing.T) {
	now := metav1.Now()
	conditions := []metav1.Condition{
		{
			Type:               "Available",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			Reason:             "NoReplicasAvailable",
			Message:            "0/1 replicas ready",
		},
	}
	setCondition(&conditions, metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "MinimumReplicasAvailable",
		Message:            "1/1 replicas ready",
	})
	if len(conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(conditions))
	}
	if conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("expected ConditionTrue after update, got %s", conditions[0].Status)
	}
}

func TestSetCondition_NoOpWhenUnchanged(t *testing.T) {
	now := metav1.Now()
	conditions := []metav1.Condition{
		{
			Type:               "Available",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "MinimumReplicasAvailable",
			Message:            "original message",
		},
	}
	setCondition(&conditions, metav1.Condition{
		Type:               "Available",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "MinimumReplicasAvailable",
		Message:            "updated message",
	})
	// Same status and reason -- should not overwrite.
	if conditions[0].Message != "original message" {
		t.Errorf("expected original message preserved, got %s", conditions[0].Message)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func intSliceEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
