package swiftmigration

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// migrationScheme builds a scheme registering the types the validator
// touches. corev1 is needed for Node lookup; swift v1alpha1 for the
// source SwiftGuest; migration v1alpha1 for the validated object.
func migrationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{})
	metav1.AddToGroupVersion(s, gvSwift)
	gvMig := schema.GroupVersion{Group: "migration.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvMig, &migrationv1alpha1.SwiftMigration{}, &migrationv1alpha1.SwiftMigrationList{})
	metav1.AddToGroupVersion(s, gvMig)
	return s
}

func newSwiftMigration(name, ns string) *migrationv1alpha1.SwiftMigration {
	return &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: migrationv1alpha1.SwiftMigrationSpec{
			GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: "guest"},
			Target:   migrationv1alpha1.SwiftMigrationTarget{NodeName: "miles"},
			Mode:     migrationv1alpha1.SwiftMigrationModeAuto,
		},
	}
}

func newSwiftGuest(name, ns string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
		},
		Status: swiftv1alpha1.SwiftGuestStatus{
			NodeName: "boba",
		},
	}
}

func newReadyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// --- Shape rules (no Client) ---

func TestValidateShape_RejectLiveMode(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "live") {
		t.Errorf("validateShape mode=live should reject with mention of 'live'; got err=%v", err)
	}
	// The phase-gate constant must appear in the message so Phase 3
	// reviewers can find the rejection by grepping for it.
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), migrationv1alpha1.PhaseLiveMigrationNotShipped) {
		t.Errorf("validateShape mode=live should mention the gate constant %q; got %v",
			migrationv1alpha1.PhaseLiveMigrationNotShipped, err)
	}
}

func TestValidateShape_AcceptAutoOfflineEmpty(t *testing.T) {
	for _, mode := range []migrationv1alpha1.SwiftMigrationMode{
		"",
		migrationv1alpha1.SwiftMigrationModeAuto,
		migrationv1alpha1.SwiftMigrationModeOffline,
	} {
		t.Run(string(mode), func(t *testing.T) {
			mig := newSwiftMigration("m", "default")
			mig.Spec.Mode = mode
			if err := validateShape(mig); err != nil {
				t.Errorf("validateShape mode=%q should accept; got %v", mode, err)
			}
		})
	}
}

func TestValidateShape_RejectInvalidMode(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.Mode = "bogus"
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "not a recognised value") {
		t.Errorf("validateShape mode=bogus should reject; got %v", err)
	}
}

func TestValidateShape_RejectMissingTarget(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.Target = migrationv1alpha1.SwiftMigrationTarget{}
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("validateShape empty target should reject; got %v", err)
	}
}

func TestValidateShape_RejectBothNodeNameAndSelector(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.Target.NodeSelector = map[string]string{"role": "worker"}
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("validateShape both nodeName+nodeSelector should reject as mutually exclusive; got %v", err)
	}
}

func TestValidateShape_RejectNodeSelectorPhase1(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.Target.NodeName = ""
	mig.Spec.Target.NodeSelector = map[string]string{"role": "worker"}
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "nodeSelector is not yet shipped") {
		t.Errorf("validateShape nodeSelector-only should reject as Phase-4-only; got %v", err)
	}
}

func TestValidateShape_RejectMissingGuestRef(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.GuestRef.Name = ""
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "guestRef.name is required") {
		t.Errorf("validateShape missing guestRef.name should reject; got %v", err)
	}
}

func TestValidateShape_RejectIgnoreTimeoutStrategy(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.TimeoutStrategy = migrationv1alpha1.SwiftMigrationTimeoutStrategyIgnore
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "ignore is reserved for live mode") {
		t.Errorf("validateShape timeoutStrategy=ignore should reject as live-mode-only; got %v", err)
	}
}

// --- Phase 1 input bounds (security review) ---

func TestValidateShape_RejectTimeoutOverMax(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.Timeout = &metav1.Duration{Duration: 25 * time.Hour}
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("validateShape timeout > 24h should reject; got %v", err)
	}
}

func TestValidateShape_AcceptTimeoutAtMax(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.Timeout = &metav1.Duration{Duration: MaxTimeout}
	if err := validateShape(mig); err != nil {
		t.Errorf("validateShape timeout=24h should accept; got %v", err)
	}
}

func TestValidateShape_RejectParallelConnectionsOverMax(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.ParallelConnections = MaxParallelConnections + 1
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "parallelConnections") {
		t.Errorf("validateShape parallelConnections > max should reject; got %v", err)
	}
}

func TestValidateShape_RejectParallelConnectionsNegative(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.ParallelConnections = -1
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Errorf("validateShape parallelConnections < 0 should reject; got %v", err)
	}
}

func TestValidateShape_RejectReasonTooLong(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.Reason = strings.Repeat("a", MaxReasonLen+1)
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Errorf("validateShape reason > %d chars should reject; got %v", MaxReasonLen, err)
	}
}

func TestValidateShape_RejectReasonControlChars(t *testing.T) {
	for _, badReason := range []string{
		"node\nfake-event-injection",
		"escape\x1b[31mred",
		"carriage\rreturn",
		"null\x00byte",
	} {
		t.Run(badReason, func(t *testing.T) {
			mig := newSwiftMigration("m", "default")
			mig.Spec.Reason = badReason
			if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "control character") {
				t.Errorf("validateShape reason with control char should reject; got %v", err)
			}
		})
	}
}

func TestValidateShape_AcceptReasonWhitespace(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.Reason = "node-drain (manual)\tby\tadmin"
	if err := validateShape(mig); err != nil {
		t.Errorf("validateShape reason with space/tab should accept; got %v", err)
	}
}

// --- Cluster-state rules (Client populated) ---

func TestValidateClusterState_GuestNotFound(t *testing.T) {
	scheme := migrationScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	_, err := v.validate(context.Background(), mig)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("validate with no source guest should reject; got %v", err)
	}
}

func TestValidateClusterState_MigrationDisabled(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	disabled := false
	guest.Spec.Migration = &swiftv1alpha1.MigrationSpec{Enabled: &disabled}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	_, err := v.validate(context.Background(), mig)
	if err == nil {
		t.Fatalf("validate with migration.enabled=false should reject")
	}
	if !strings.Contains(err.Error(), "migration.enabled=false") {
		t.Errorf("error should mention migration.enabled=false; got %q", err.Error())
	}
	// The fix-hint suffix tells the operator how to unblock; lock it
	// in so a future message that drops it regresses the test.
	if !strings.Contains(err.Error(), "set enabled=true to allow") {
		t.Errorf("error should include the fix-hint 'set enabled=true to allow'; got %q", err.Error())
	}
}

func TestValidateClusterState_SameNodeRejected(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default") // status.NodeName = boba
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("boba")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	mig.Spec.Target.NodeName = "boba"
	_, err := v.validate(context.Background(), mig)
	if err == nil {
		t.Fatal("validate same-node migration should reject")
	}
	// Operators need to see both the target name and the source name in
	// the message — names are how they diagnose "wait, that's where it
	// is already." Lock both in.
	if !strings.Contains(err.Error(), `"boba"`) {
		t.Errorf("error should name the conflicting node; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "same-node migration is meaningless") {
		t.Errorf("error should explain the rejection reason; got %q", err.Error())
	}
}

func TestValidateClusterState_TargetNodeMissing(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	_, err := v.validate(context.Background(), mig)
	if err == nil || !strings.Contains(err.Error(), "target node") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("validate missing target node should reject; got %v", err)
	}
}

func TestValidateClusterState_TargetNodeCordoned(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	cordoned := newReadyNode("miles")
	cordoned.Spec.Unschedulable = true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, cordoned).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	_, err := v.validate(context.Background(), mig)
	if err == nil || !strings.Contains(err.Error(), "cordoned") {
		t.Errorf("validate cordoned target should reject; got %v", err)
	}
}

func TestValidateClusterState_TargetNodeNotReady(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	notReady := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "miles"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, notReady).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	_, err := v.validate(context.Background(), mig)
	if err == nil || !strings.Contains(err.Error(), "not Ready") {
		t.Errorf("validate not-Ready target should reject; got %v", err)
	}
}

func TestValidateClusterState_KernelBootMissingLabel(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Spec.ImageRef = nil
	guest.Spec.KernelRef = &corev1.LocalObjectReference{Name: "kernel-1"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	_, err := v.validate(context.Background(), mig)
	if err == nil || !strings.Contains(err.Error(), "kubeswift.io/kernel-node") {
		t.Errorf("validate kernel-boot guest to non-kernel-node should reject; got %v", err)
	}
}

func TestValidateClusterState_GPUCrossNodeRejected(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu-pcie"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	_, err := v.validate(context.Background(), mig)
	if err == nil {
		t.Fatal("validate GPU guest cross-node should reject")
	}
	if !strings.Contains(err.Error(), "VFIO devices") {
		t.Errorf("error should mention VFIO devices; got %q", err.Error())
	}
	// The Phase-1-not-supported gate text — lock in so a future relax
	// of the rule (Phase 4 work) requires updating this assertion.
	if !strings.Contains(err.Error(), "cross-node migration is not supported in Phase 1") {
		t.Errorf("error should mention the Phase 1 gate; got %q", err.Error())
	}
}

func TestValidateClusterState_GPUUnscheduledRejected(t *testing.T) {
	// GPU guest that hasn't been scheduled yet (status.NodeName empty).
	// Phase 1 rejects unconditionally — the architect's GPU rejection
	// rule is unconditional (security tightening), not gated on
	// sourceNode being known. Without this rule, an operator could
	// submit a SwiftMigration on an unscheduled GPU guest and create a
	// race with the SwiftGPU controller's allocation logic.
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Status.NodeName = "" // unscheduled
	guest.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gpu-pcie"}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	_, err := v.validate(context.Background(), mig)
	if err == nil || !strings.Contains(err.Error(), "VFIO devices") {
		t.Errorf("validate unscheduled GPU guest should reject; got %v", err)
	}
}

func TestValidateClusterState_SRIOVCrossNodeRejected(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
		{Name: "data", Type: swiftv1alpha1.InterfaceTypeSRIOV, ResourceName: "intel.com/sriov_netdevice"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	_, err := v.validate(context.Background(), mig)
	if err == nil || !strings.Contains(err.Error(), "VFIO devices") {
		t.Errorf("validate SR-IOV guest cross-node should reject; got %v", err)
	}
}

func TestValidateClusterState_DefaultNetworkingNeedsAllowIPChange(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default") // no spec.interfaces, default networking
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default") // allowIPChange=false (default)
	_, err := v.validate(context.Background(), mig)
	if err == nil || !strings.Contains(err.Error(), "allowIPChange") {
		t.Errorf("validate default-networking cross-node without allowIPChange should reject; got %v", err)
	}
}

func TestValidateClusterState_DefaultNetworkingWithAllowIPChange(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	mig.Spec.AllowIPChange = true
	warnings, err := v.validate(context.Background(), mig)
	if err != nil {
		t.Errorf("validate default-networking with allowIPChange should accept; got %v", err)
	}
	// Warning is the user-visible "you opted in, IP will change" notice.
	// Assert content, not just non-emptiness — a regression that emits
	// "foo" would still pass a length-only check.
	if len(warnings) == 0 {
		t.Fatal("validate should surface a warning when allowIPChange=true triggers")
	}
	if !strings.Contains(warnings[0], "fresh IP") || !strings.Contains(warnings[0], "allowIPChange=true") {
		t.Errorf("warning should mention 'fresh IP' and 'allowIPChange=true'; got %q", warnings[0])
	}
}

func TestValidateClusterState_MultusInterface_NoIPChangeNeeded(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
		{
			Name: "data",
			NetworkRef: &swiftv1alpha1.NetworkReference{
				Name:      "macvlan-data",
				Namespace: "default",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default") // allowIPChange=false; should still pass
	_, err := v.validate(context.Background(), mig)
	if err != nil {
		t.Errorf("validate multus-attached guest cross-node should accept without allowIPChange; got %v", err)
	}
}

func TestValidateClusterState_HappyPath(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
		{
			Name:       "data",
			NetworkRef: &swiftv1alpha1.NetworkReference{Name: "macvlan", Namespace: "default"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	warnings, err := v.validate(context.Background(), mig)
	if err != nil {
		t.Errorf("happy-path validate should accept; got %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("happy-path validate should produce no warnings; got %v", warnings)
	}
}

// --- Pinning tests for behavior that's easy to refactor away ---

// TestValidateClusterState_SourceNotYetScheduled pins the validator's
// tolerance of an unscheduled source guest (status.NodeName == "").
// Phase 1's Preparing phase makes runPolicy patches a no-op when the
// source isn't running, so accepting this case is correct.
func TestValidateClusterState_SourceNotYetScheduled(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Status.NodeName = ""
	guest.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
		{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "macvlan", Namespace: "default"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	if _, err := v.validate(context.Background(), mig); err != nil {
		t.Errorf("validate unscheduled source guest with multus interface should accept; got %v", err)
	}
}

// TestValidateClusterState_MixedInterfaces_DefaultPlusMultus pins the
// behavior of isDefaultNodeLocalNetworking on a mixed-interface guest:
// any non-nil NetworkRef makes the helper return false (treats the
// guest as multi-node-capable). The default-bridge interface's IP will
// still change cross-node, but the operator presumably has reasons to
// run mixed networking — silently accepting the cross-node migration
// is the architect's call.
func TestValidateClusterState_MixedInterfaces_DefaultPlusMultus(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
		{Name: "mgmt"}, // default bridge
		{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "macvlan", Namespace: "default"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default") // allowIPChange=false
	warnings, err := v.validate(context.Background(), mig)
	if err != nil {
		t.Errorf("validate mixed-interface guest cross-node should accept (multus interface treats as multi-node-capable); got %v", err)
	}
	// No allowIPChange warning because the multus interface marks the
	// guest as not-default. If a future refactor changes
	// isDefaultNodeLocalNetworking to be stricter, this will fail.
	if len(warnings) != 0 {
		t.Errorf("mixed-interface accepted path should produce no warnings; got %v", warnings)
	}
}

// TestValidateClusterState_SameNodeOnDefaultNet_RejectionOrdering pins
// the order: same-node check fires BEFORE the default-networking check.
// This matters because both apply to a default-networking same-node
// migration, but operators should see the more-specific "same node"
// error, not the less-specific "default networking" error.
func TestValidateClusterState_SameNodeOnDefaultNet_RejectionOrdering(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default") // status.NodeName = boba, default networking
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("boba")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	mig.Spec.Target.NodeName = "boba" // same node
	_, err := v.validate(context.Background(), mig)
	if err == nil {
		t.Fatal("validate same-node default-net migration should reject")
	}
	// The same-node check should fire first.
	if !strings.Contains(err.Error(), "same-node migration is meaningless") {
		t.Errorf("same-node check should fire first; got %q", err.Error())
	}
	if strings.Contains(err.Error(), "allowIPChange") {
		t.Errorf("default-net check fired before same-node — ordering broken; got %q", err.Error())
	}
}

// TestValidateClusterState_CrossNamespaceReferenceAsNotFound pins the
// same-namespace constraint: a SwiftMigration in namespace A that names
// a SwiftGuest of the same name in namespace B is rejected as NotFound
// because the lookup is namespace-scoped to mig.Namespace. This is a
// regression anchor for Phase 4 (drain) which will create
// SwiftMigrations on behalf of evictions and must respect the same
// constraint.
func TestValidateClusterState_CrossNamespaceReferenceAsNotFound(t *testing.T) {
	scheme := migrationScheme(t)
	otherNsGuest := newSwiftGuest("guest", "other-namespace")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(otherNsGuest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default") // namespace=default, guest.Name=guest, but guest is in other-namespace
	_, err := v.validate(context.Background(), mig)
	if err == nil || !strings.Contains(err.Error(), "not found in namespace") {
		t.Errorf("validate cross-namespace reference should reject as NotFound in mig.Namespace; got %v", err)
	}
}

// TestValidateClusterState_EmptyTargetGuard exercises the defensive
// target=="" guard added per security review. validateShape requires
// nodeName non-empty so this path is normally unreachable, but the
// guard catches a future-Phase-4 patch that bypasses validateShape.
func TestValidateClusterState_EmptyTargetGuard(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	// Bypass validateShape: call validateClusterState directly with an
	// empty target. Simulates the future-Phase-4 path.
	mig.Spec.Target.NodeName = ""
	_, err := v.validateClusterState(context.Background(), mig)
	if err == nil || !strings.Contains(err.Error(), "target.nodeName is empty") {
		t.Errorf("validateClusterState with empty target should reject; got %v", err)
	}
}

// --- Immutability ---

func TestValidateUpdate_SpecImmutable(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
		{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "macvlan", Namespace: "default"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles"), newReadyNode("frida")).Build()
	v := &Validator{Client: c}

	old := newSwiftMigration("m", "default")
	new := newSwiftMigration("m", "default")
	new.Spec.Target.NodeName = "frida" // changed
	_, err := v.ValidateUpdate(context.Background(), old, new)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("ValidateUpdate with spec change should reject as immutable; got %v", err)
	}
}

func TestValidateUpdate_NoSpecChange(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
		{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "macvlan", Namespace: "default"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	old := newSwiftMigration("m", "default")
	new := newSwiftMigration("m", "default")
	new.ObjectMeta.Annotations = map[string]string{"changed": "true"}
	if _, err := v.ValidateUpdate(context.Background(), old, new); err != nil {
		t.Errorf("ValidateUpdate with metadata-only change should accept; got %v", err)
	}
}
