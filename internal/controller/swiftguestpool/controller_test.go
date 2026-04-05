package swiftguestpool

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
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

// ---------------------------------------------------------------------------
// computeTemplateHash tests
// ---------------------------------------------------------------------------

func TestComputeTemplateHash_Stable(t *testing.T) {
	tmpl := swiftv1alpha1.SwiftGuestTemplateSpec{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef: &corev1.LocalObjectReference{Name: "ubuntu-noble"},
			RunPolicy: swiftv1alpha1.RunPolicyRunning,
		},
	}
	h1 := computeTemplateHash(&tmpl)
	h2 := computeTemplateHash(&tmpl)
	if h1 != h2 {
		t.Errorf("same template produced different hashes: %s vs %s", h1, h2)
	}
	if h1 == "" || h1 == "unknown" {
		t.Errorf("expected valid hash, got %q", h1)
	}
}

func TestComputeTemplateHash_DifferentSpec(t *testing.T) {
	tmpl1 := swiftv1alpha1.SwiftGuestTemplateSpec{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef: &corev1.LocalObjectReference{Name: "ubuntu-noble"},
		},
	}
	tmpl2 := swiftv1alpha1.SwiftGuestTemplateSpec{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef: &corev1.LocalObjectReference{Name: "ubuntu-jammy"},
		},
	}
	h1 := computeTemplateHash(&tmpl1)
	h2 := computeTemplateHash(&tmpl2)
	if h1 == h2 {
		t.Errorf("different imageRef should produce different hashes, both got %s", h1)
	}
}

func TestComputeTemplateHash_MetadataIgnored(t *testing.T) {
	spec := swiftv1alpha1.SwiftGuestSpec{
		ImageRef:  &corev1.LocalObjectReference{Name: "ubuntu-noble"},
		RunPolicy: swiftv1alpha1.RunPolicyRunning,
	}
	tmpl1 := swiftv1alpha1.SwiftGuestTemplateSpec{
		Metadata: swiftv1alpha1.PoolObjectMeta{
			Labels: map[string]string{"env": "dev"},
		},
		Spec: spec,
	}
	tmpl2 := swiftv1alpha1.SwiftGuestTemplateSpec{
		Metadata: swiftv1alpha1.PoolObjectMeta{
			Labels: map[string]string{"env": "prod"},
		},
		Spec: spec,
	}
	h1 := computeTemplateHash(&tmpl1)
	h2 := computeTemplateHash(&tmpl2)
	if h1 != h2 {
		t.Errorf("metadata-only change should not affect hash: %s vs %s", h1, h2)
	}
}

// ---------------------------------------------------------------------------
// hasOutdatedGuests tests
// ---------------------------------------------------------------------------

func makeGuestWithHash(name, hash string) swiftv1alpha1.SwiftGuest {
	return swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				swiftv1alpha1.AnnotationTemplateHash: hash,
			},
		},
	}
}

func TestHasOutdatedGuests_None(t *testing.T) {
	r := &SwiftGuestPoolReconciler{}
	indexMap := map[int]swiftv1alpha1.SwiftGuest{
		0: makeGuestWithHash("pool-0", "abc123"),
		1: makeGuestWithHash("pool-1", "abc123"),
		2: makeGuestWithHash("pool-2", "abc123"),
	}
	if r.hasOutdatedGuests(indexMap, "abc123") {
		t.Error("expected no outdated guests when all hashes match")
	}
}

func TestHasOutdatedGuests_Some(t *testing.T) {
	r := &SwiftGuestPoolReconciler{}
	indexMap := map[int]swiftv1alpha1.SwiftGuest{
		0: makeGuestWithHash("pool-0", "abc123"),
		1: makeGuestWithHash("pool-1", "old-hash"),
		2: makeGuestWithHash("pool-2", "abc123"),
	}
	if !r.hasOutdatedGuests(indexMap, "abc123") {
		t.Error("expected outdated guests when one hash differs")
	}
}

func TestHasOutdatedGuests_Empty(t *testing.T) {
	r := &SwiftGuestPoolReconciler{}
	indexMap := map[int]swiftv1alpha1.SwiftGuest{}
	if r.hasOutdatedGuests(indexMap, "abc123") {
		t.Error("expected no outdated guests when map is empty")
	}
}

// ---------------------------------------------------------------------------
// buildTopologyConstraints tests
// ---------------------------------------------------------------------------

func TestBuildTopologyConstraints_Pack(t *testing.T) {
	r := &SwiftGuestPoolReconciler{}
	pool := &swiftv1alpha1.SwiftGuestPool{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet"},
		Spec: swiftv1alpha1.SwiftGuestPoolSpec{
			SpreadPolicy: swiftv1alpha1.SpreadPolicyPack,
		},
	}
	constraints := r.buildTopologyConstraints(pool)
	if constraints != nil {
		t.Errorf("expected nil constraints for Pack policy, got %v", constraints)
	}
}

func TestBuildTopologyConstraints_Spread(t *testing.T) {
	r := &SwiftGuestPoolReconciler{}
	pool := &swiftv1alpha1.SwiftGuestPool{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet"},
		Spec: swiftv1alpha1.SwiftGuestPoolSpec{
			SpreadPolicy: swiftv1alpha1.SpreadPolicySpread,
		},
	}
	constraints := r.buildTopologyConstraints(pool)
	if len(constraints) != 1 {
		t.Fatalf("expected 1 constraint for Spread policy, got %d", len(constraints))
	}
	c := constraints[0]
	if c.TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("expected hostname topology key, got %s", c.TopologyKey)
	}
	if c.MaxSkew != 1 {
		t.Errorf("expected maxSkew=1, got %d", c.MaxSkew)
	}
	if c.WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Errorf("expected ScheduleAnyway, got %v", c.WhenUnsatisfiable)
	}
}

func TestBuildTopologyConstraints_Explicit(t *testing.T) {
	r := &SwiftGuestPoolReconciler{}
	explicit := corev1.TopologySpreadConstraint{
		MaxSkew:           2,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.DoNotSchedule,
	}
	pool := &swiftv1alpha1.SwiftGuestPool{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet"},
		Spec: swiftv1alpha1.SwiftGuestPoolSpec{
			SpreadPolicy:              swiftv1alpha1.SpreadPolicySpread, // should be ignored
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{explicit},
		},
	}
	constraints := r.buildTopologyConstraints(pool)
	if len(constraints) != 1 {
		t.Fatalf("expected 1 explicit constraint, got %d", len(constraints))
	}
	c := constraints[0]
	if c.TopologyKey != "topology.kubernetes.io/zone" {
		t.Errorf("expected zone topology key, got %s", c.TopologyKey)
	}
	if c.MaxSkew != 2 {
		t.Errorf("expected maxSkew=2 from explicit constraint, got %d", c.MaxSkew)
	}
}

func TestBuildTopologyConstraints_LabelSelector(t *testing.T) {
	r := &SwiftGuestPoolReconciler{}
	pool := &swiftv1alpha1.SwiftGuestPool{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet"},
		Spec: swiftv1alpha1.SwiftGuestPoolSpec{
			SpreadPolicy: swiftv1alpha1.SpreadPolicySpread,
		},
	}
	constraints := r.buildTopologyConstraints(pool)
	if len(constraints) != 1 {
		t.Fatalf("expected 1 constraint, got %d", len(constraints))
	}
	sel := constraints[0].LabelSelector
	if sel == nil {
		t.Fatal("expected label selector on spread constraint, got nil")
	}
	val, ok := sel.MatchLabels[swiftv1alpha1.LabelPoolName]
	if !ok {
		t.Fatal("expected pool label in selector matchLabels")
	}
	if val != "fleet" {
		t.Errorf("expected pool name 'fleet' in selector, got %s", val)
	}
}

func TestBuildTopologyConstraints_ExplicitAddsSelector(t *testing.T) {
	r := &SwiftGuestPoolReconciler{}
	// Explicit constraint without a label selector -- controller should add one.
	explicit := corev1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector:     nil,
	}
	pool := &swiftv1alpha1.SwiftGuestPool{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet"},
		Spec: swiftv1alpha1.SwiftGuestPoolSpec{
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{explicit},
		},
	}
	constraints := r.buildTopologyConstraints(pool)
	if constraints[0].LabelSelector == nil {
		t.Fatal("expected controller to inject label selector on explicit constraint without one")
	}
	val := constraints[0].LabelSelector.MatchLabels[swiftv1alpha1.LabelPoolName]
	if val != "fleet" {
		t.Errorf("expected pool name in injected selector, got %s", val)
	}
}

func TestBuildTopologyConstraints_ExplicitPreservesExistingSelector(t *testing.T) {
	r := &SwiftGuestPoolReconciler{}
	explicit := corev1.TopologySpreadConstraint{
		MaxSkew:           1,
		TopologyKey:       "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.DoNotSchedule,
		LabelSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"custom": "selector"},
		},
	}
	pool := &swiftv1alpha1.SwiftGuestPool{
		ObjectMeta: metav1.ObjectMeta{Name: "fleet"},
		Spec: swiftv1alpha1.SwiftGuestPoolSpec{
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{explicit},
		},
	}
	constraints := r.buildTopologyConstraints(pool)
	sel := constraints[0].LabelSelector
	if _, ok := sel.MatchLabels["custom"]; !ok {
		t.Error("expected existing custom selector to be preserved")
	}
	if _, ok := sel.MatchLabels[swiftv1alpha1.LabelPoolName]; ok {
		t.Error("expected controller NOT to overwrite user-provided selector")
	}
}

// ---------------------------------------------------------------------------
// pvcName tests
// ---------------------------------------------------------------------------

func TestPVCName(t *testing.T) {
	tests := []struct {
		templateName string
		poolName     string
		index        int
		want         string
	}{
		{"cache", "fleet", 3, "cache-fleet-3"},
		{"data", "gpu-pool", 0, "data-gpu-pool-0"},
		{"scratch", "p", 99, "scratch-p-99"},
	}
	for _, tt := range tests {
		got := pvcName(tt.templateName, tt.poolName, tt.index)
		if got != tt.want {
			t.Errorf("pvcName(%q, %q, %d) = %q, want %q",
				tt.templateName, tt.poolName, tt.index, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// updatedReason tests
// ---------------------------------------------------------------------------

func TestUpdatedReason_AllUpdated(t *testing.T) {
	got := updatedReason(3, 3)
	if got != "AllReplicasUpdated" {
		t.Errorf("expected AllReplicasUpdated, got %s", got)
	}
}

func TestUpdatedReason_InProgress(t *testing.T) {
	got := updatedReason(1, 3)
	if got != "RollingUpdateInProgress" {
		t.Errorf("expected RollingUpdateInProgress, got %s", got)
	}
}
