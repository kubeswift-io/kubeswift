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
	s.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{},
		&swiftv1alpha1.SwiftGuestClass{}, &swiftv1alpha1.SwiftGuestClassList{})
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

// newReadyKernelNode returns a Ready node with the
// kubeswift.io/kernel-node=true label so kernel-boot guests pass the
// boot-type-specific node-label requirement in validateClusterState.
func newReadyKernelNode(name string) *corev1.Node {
	n := newReadyNode(name)
	n.Labels = map[string]string{"kubeswift.io/kernel-node": "true"}
	return n
}

// --- Shape rules (no Client) ---

// TestValidateShape_AcceptLiveMode locks in Phase 3a's acceptance of
// mode=live. Phase 1 rejected mode=live at shape; PR #41 shipped the
// swiftletd D1/D2/D3 dependencies and this PR ships the controller,
// so live mode is now valid at admission. Live-mode-specific cluster-
// state checks (per-source-node concurrency, kernel-boot vs PVC
// storage gate) live in validateClusterState; live-mode-specific
// shape rules (242-char guest-name cap) live in validateShape and
// have their own tests.
func TestValidateShape_AcceptLiveMode(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	if err := validateShape(mig); err != nil {
		t.Errorf("validateShape mode=live should be accepted in Phase 3a; got err=%v", err)
	}
}

// TestValidateShape_LiveModeRejectsLongGuestName covers the Phase 3a
// guest-name length cap. The destination launcher pod is named
// `<guest>-mig-<short-uid>` (11-char suffix); Kubernetes pod names
// are bounded at 253 chars (DNS-1123). Cap GUEST name (not
// SwiftMigration name) at 242 with headroom. Phase 1 offline mode
// reuses the guest name unchanged post-migration and is unaffected.
func TestValidateShape_LiveModeRejectsLongGuestName(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	// 243 chars — one over the cap.
	mig.Spec.GuestRef.Name = strings.Repeat("a", 243)
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	if err := validateShape(mig); err == nil || !strings.Contains(err.Error(), "242") {
		t.Errorf("validateShape mode=live with 243-char guest name should reject mentioning 242; got %v", err)
	}
}

// TestValidateShape_LiveModeAcceptsBoundaryGuestName confirms the
// 242-char cap is inclusive of 242 (the literal limit). A 242-char
// guest name is accepted; 243 is rejected (separate test).
func TestValidateShape_LiveModeAcceptsBoundaryGuestName(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.GuestRef.Name = strings.Repeat("a", 242)
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	if err := validateShape(mig); err != nil {
		t.Errorf("validateShape mode=live with 242-char guest name (at cap) should accept; got err=%v", err)
	}
}

// TestValidateShape_OfflineModeAcceptsLongGuestName confirms the
// 242-char cap fires only for live mode. Offline-mode guest names
// are unaffected (offline mode reuses guest.Name as the post-migration
// pod name with no suffix).
func TestValidateShape_OfflineModeAcceptsLongGuestName(t *testing.T) {
	mig := newSwiftMigration("m", "default")
	mig.Spec.GuestRef.Name = strings.Repeat("a", 250)
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeOffline
	if err := validateShape(mig); err != nil {
		t.Errorf("validateShape mode=offline with 250-char guest name should accept; got err=%v", err)
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

// --- Per-operation validation discipline (Bug A + B + C fix) ---
//
// Background: this PR rationalizes admission validation to fire only
// where it adds value. ValidateCreate runs full cluster-state checks
// (the submission gate). ValidateUpdate runs spec immutability + spec
// shape, never cluster-state. Cluster-state changes between CREATE
// and UPDATE are the controller's domain — webhook re-validation
// turns transient cluster conditions into stuck resources.
//
// The three bugs that motivated this design:
//
//   - Bug A (HIGH): operator deletes source SwiftGuest, then deletes
//     SwiftMigration. removeFinalizer patch hits ValidateUpdate; old
//     code ran cluster-state and rejected on missing guest. Result:
//     SwiftMigration could not be deleted via any kubectl operation.
//   - Bug B (MEDIUM): completed SwiftMigrations stuck reconciling.
//     Watch fan-out enqueues every active migration; removeFinalizer
//     patch hit ValidateUpdate; cluster-state rejected because
//     source==target post-cutover. Retry storm forever.
//   - Bug C (MEDIUM): in-flight (Pending/Validating/Preparing)
//     migration whose source guest disappeared mid-flight had its
//     ensureFinalizer patch rejected on every reconcile. Trapped
//     in a non-terminal phase the controller couldn't fail-and-clean.
//
// All three share root cause: validation logic firing on every
// operation without discriminating between submission (gate value)
// and metadata churn (no value, real cost).

// TestValidateUpdate_DeletionTimestamp_NoClusterState — Bug A
// regression. SwiftMigration with deletionTimestamp set + source
// guest absent. The controller's removeFinalizer patch shape must
// pass admission.
func TestValidateUpdate_DeletionTimestamp_NoClusterState(t *testing.T) {
	scheme := migrationScheme(t)
	// No source SwiftGuest in the cluster.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	v := &Validator{Client: c}

	old := newSwiftMigration("m", "default")
	old.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseCompleted
	now := metav1.Now()
	old.DeletionTimestamp = &now
	old.Finalizers = []string{"migration.kubeswift.io/cleanup"}
	new := old.DeepCopy()
	new.Finalizers = nil

	if _, err := v.ValidateUpdate(context.Background(), old, new); err != nil {
		t.Errorf("ValidateUpdate on deleting SwiftMigration should not run cluster-state; got %v", err)
	}
}

// TestValidateUpdate_TerminalPhase_NoClusterState — Bug B regression.
// Parameterized over all three terminal phases. Source guest exists
// on what was the migration's target — exact post-cutover scenario
// the live cluster hit.
func TestValidateUpdate_TerminalPhase_NoClusterState(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Status.NodeName = "miles" // matches mig.Spec.Target.NodeName
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	for _, phase := range []migrationv1alpha1.SwiftMigrationPhase{
		migrationv1alpha1.SwiftMigrationPhaseCompleted,
		migrationv1alpha1.SwiftMigrationPhaseFailed,
		migrationv1alpha1.SwiftMigrationPhaseCancelled,
	} {
		t.Run(string(phase), func(t *testing.T) {
			old := newSwiftMigration("m", "default")
			old.Status.Phase = phase
			old.Finalizers = []string{"migration.kubeswift.io/cleanup"}
			new := old.DeepCopy()
			new.Finalizers = nil
			if _, err := v.ValidateUpdate(context.Background(), old, new); err != nil {
				t.Errorf("ValidateUpdate on phase=%s should not run cluster-state; got %v", phase, err)
			}
		})
	}
}

// TestValidateUpdate_InFlight_NoClusterState — Bug C regression.
// Mid-flight (Pending/Validating/Preparing/StopAndCopy/Resuming)
// migration whose source SwiftGuest was deleted mid-migration.
// The controller's ensureFinalizer / annotation-flip metadata
// patches must pass admission so the controller can drive the
// migration to Failed and clean up.
//
// Pre-fix this was rejected with "source SwiftGuest 'guest' not
// found" on every metadata patch, trapping the migration in a
// non-terminal phase that no kubectl could untangle.
func TestValidateUpdate_InFlight_NoClusterState(t *testing.T) {
	scheme := migrationScheme(t)
	// Source guest deleted mid-migration — absent from the fake client.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	v := &Validator{Client: c}

	for _, phase := range []migrationv1alpha1.SwiftMigrationPhase{
		"",
		migrationv1alpha1.SwiftMigrationPhasePending,
		migrationv1alpha1.SwiftMigrationPhaseValidating,
		migrationv1alpha1.SwiftMigrationPhasePreparing,
		migrationv1alpha1.SwiftMigrationPhaseStopAndCopy,
		migrationv1alpha1.SwiftMigrationPhaseResuming,
	} {
		t.Run(string(phase), func(t *testing.T) {
			old := newSwiftMigration("m", "default")
			old.Status.Phase = phase
			new := old.DeepCopy()
			new.ObjectMeta.Annotations = map[string]string{"controller-touch": "1"}
			if _, err := v.ValidateUpdate(context.Background(), old, new); err != nil {
				t.Errorf("ValidateUpdate on phase=%s with metadata-only change should not run cluster-state; got %v", phase, err)
			}
		})
	}
}

// TestValidateUpdate_StillEnforcesShape verifies the per-operation
// discipline is "skip cluster-state on UPDATE", NOT "skip all
// validation on UPDATE". Spec immutability is still enforced (a
// dedicated test exists for that — TestValidateUpdate_SpecImmutable),
// and spec shape is still validated as defense-in-depth in case a
// future patch path bypasses immutability.
func TestValidateUpdate_StillEnforcesShape(t *testing.T) {
	scheme := migrationScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	v := &Validator{Client: c}

	// Phase 3a accepts mode=live, so the shape-canary uses a still-
	// invalid value. parallelConnections > MaxParallelConnections is
	// a stable shape rule (input bounds, no cluster-state dependency)
	// that fires the same way on UPDATE as on CREATE — the canary
	// proves shape validation runs.
	old := newSwiftMigration("m", "default")
	old.Spec.ParallelConnections = MaxParallelConnections + 1
	new := old.DeepCopy()
	if _, err := v.ValidateUpdate(context.Background(), old, new); err == nil || !strings.Contains(err.Error(), "parallelConnections") {
		t.Errorf("ValidateUpdate must still enforce shape on UPDATE; got %v", err)
	}
}

// TestValidateCreate_RunsClusterState verifies CREATE retains full
// validation. The submission point is when cluster-state gating adds
// value (operator's intent first hits the API server). Anti-regression
// against accidentally extending the per-operation skip to CREATE.
func TestValidateCreate_RunsClusterState(t *testing.T) {
	scheme := migrationScheme(t)
	// No source guest — CREATE must reject.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	_, err := v.ValidateCreate(context.Background(), mig)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("ValidateCreate must run cluster-state validation; got %v", err)
	}
}

// TestIsTerminalPhase covers the helper directly to lock in the set of
// phases treated as terminal. Adding a new terminal phase in a future
// release must be reflected here AND in the controller's mirror copy.
func TestIsTerminalPhase(t *testing.T) {
	for _, tc := range []struct {
		phase migrationv1alpha1.SwiftMigrationPhase
		want  bool
	}{
		{migrationv1alpha1.SwiftMigrationPhaseCompleted, true},
		{migrationv1alpha1.SwiftMigrationPhaseFailed, true},
		{migrationv1alpha1.SwiftMigrationPhaseCancelled, true},
		{migrationv1alpha1.SwiftMigrationPhasePending, false},
		{migrationv1alpha1.SwiftMigrationPhaseValidating, false},
		{migrationv1alpha1.SwiftMigrationPhasePreparing, false},
		{migrationv1alpha1.SwiftMigrationPhaseStopAndCopy, false},
		{migrationv1alpha1.SwiftMigrationPhaseResuming, false},
		{"", false},
		{"future-phase", false},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			if got := isTerminalPhase(tc.phase); got != tc.want {
				t.Errorf("isTerminalPhase(%q) = %v, want %v", tc.phase, got, tc.want)
			}
		})
	}
}

// --- Live-mode storage gate (W6 follow-up) ---
//
// The gate fires from validateClusterState when spec.mode=live. Phase 1
// validateShape rejects mode=live before validateClusterState runs, so
// the gate is unreachable through the public API today. These tests
// drive gateLiveModeStorage directly so the rule is locked in for
// Phase 3, when validateShape starts accepting mode=live.

func TestGateLiveModeStorage_RWOFilesystemRejected(t *testing.T) {
	// Default storage (RWO+Filesystem). Live migration of a disk-boot
	// guest with RWO storage requires the not-yet-implemented Phase 3
	// RWO-handoff choreography; the gate rejects with a clear remedy
	// message naming RWX+Block.
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	class := &swiftv1alpha1.SwiftGuestClass{ObjectMeta: metav1.ObjectMeta{Name: "class"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class).Build()
	v := &Validator{Client: c}

	err := v.gateLiveModeStorage(context.Background(), guest)
	if err == nil {
		t.Fatal("gate should reject default RWO+Filesystem for live mode")
	}
	if !strings.Contains(err.Error(), "ReadWriteMany") || !strings.Contains(err.Error(), "Block") {
		t.Errorf("error must name the required RWM+Block combo so operators know how to fix it; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "spec.mode=offline") {
		t.Errorf("error should hint at mode=offline as the alternative; got %q", err.Error())
	}
}

func TestGateLiveModeStorage_ClassRWXBlockAccepted(t *testing.T) {
	// Class declares RWX+Block. The guest inherits via per-field merge
	// (no override). Live mode passes the gate.
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	class := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class"},
		Spec: swiftv1alpha1.SwiftGuestClassSpec{
			Storage: &swiftv1alpha1.StorageSpec{
				AccessMode: corev1.ReadWriteMany,
				VolumeMode: corev1.PersistentVolumeBlock,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class).Build()
	v := &Validator{Client: c}

	if err := v.gateLiveModeStorage(context.Background(), guest); err != nil {
		t.Errorf("gate should accept class RWX+Block; got %v", err)
	}
}

func TestGateLiveModeStorage_GuestOverridesClassRejected(t *testing.T) {
	// Class is RWX+Block (live-capable) but the guest explicitly downgrades
	// AccessMode to RWO. Per-field merge resolves the guest override; the
	// gate rejects.
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Spec.Storage = &swiftv1alpha1.StorageSpec{
		AccessMode: corev1.ReadWriteOnce,
	}
	class := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class"},
		Spec: swiftv1alpha1.SwiftGuestClassSpec{
			Storage: &swiftv1alpha1.StorageSpec{
				AccessMode: corev1.ReadWriteMany,
				VolumeMode: corev1.PersistentVolumeBlock,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class).Build()
	v := &Validator{Client: c}

	err := v.gateLiveModeStorage(context.Background(), guest)
	if err == nil {
		t.Fatal("gate should reject when guest downgrades AccessMode to RWO")
	}
	if !strings.Contains(err.Error(), "accessMode=ReadWriteOnce") {
		t.Errorf("error should report the resolved (downgraded) AccessMode; got %q", err.Error())
	}
}

func TestGateLiveModeStorage_MissingClassDoesNotDoubleReject(t *testing.T) {
	// SwiftGuestClass missing: the controller will fail resolution and
	// surface ResolutionFailed. The webhook MUST NOT double-reject on an
	// unrelated condition — return nil so the more-specific failure
	// path is the one operators see.
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	v := &Validator{Client: c}

	if err := v.gateLiveModeStorage(context.Background(), guest); err != nil {
		t.Errorf("gate should defer when class is missing; got %v", err)
	}
}

// TestValidate_ModeLiveDiskBootRejectedByStorageGate locks in the
// Phase 3a contract that mode=live admits at validateShape and is then
// rejected by the cluster-state storage gate when the guest's storage
// is not live-migration-capable. Disk-boot guests with default
// RWO+Filesystem storage hit gateLiveModeStorage's RWX+Block
// requirement; operators see the storage capability error with a
// pointer to docs/design/storage-access-mode.md.
//
// The Phase 1 ancestor of this test asserted shape-level rejection
// ("not yet shipped"); Phase 3a removed that gate — the docstring
// from that ancestor said "When Phase 3 lands, the assertion in this
// test changes to require the storage gate's error" — Phase 3a is
// that landing.
func TestValidate_ModeLiveDiskBootRejectedByStorageGate(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	class := &swiftv1alpha1.SwiftGuestClass{ObjectMeta: metav1.ObjectMeta{Name: "class"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class, newReadyNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true // bypass the IP-change gate so we reach the storage gate
	_, err := v.validate(context.Background(), mig)
	if err == nil {
		t.Fatal("validate with mode=live + RWO+Filesystem disk-boot guest should reject")
	}
	if !strings.Contains(err.Error(), "live migration requires") {
		t.Errorf("Phase 3a should reject at the storage gate; got %q", err.Error())
	}
}

// TestValidate_ModeLiveKernelBootAcceptsAtStorageGate locks in the
// Phase 3a kernel-boot adjustment to gateLiveModeStorage. Kernel-boot
// guests have no shared storage to coordinate (no F2 split-brain
// concern, no cross-node storage handoff); the storage gate must
// accept them rather than rejecting on the absence of RWX+Block.
//
// Design doc clarification captured in PR description: kernel-boot
// guests are live-capable by virtue of having no shared storage.
func TestValidate_ModeLiveKernelBootAcceptsAtStorageGate(t *testing.T) {
	scheme := migrationScheme(t)
	guest := newSwiftGuest("guest", "default")
	guest.Spec.ImageRef = nil
	guest.Spec.KernelRef = &corev1.LocalObjectReference{Name: "faas-minimal"}
	class := &swiftv1alpha1.SwiftGuestClass{ObjectMeta: metav1.ObjectMeta{Name: "class"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class, newReadyKernelNode("miles")).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true
	if _, err := v.validate(context.Background(), mig); err != nil {
		t.Errorf("validate with mode=live + kernel-boot guest should accept (no PVC = live-capable); got %v", err)
	}
}

// TestValidate_ModeLivePerSourceNodeConcurrencyRejected locks in the
// Phase 3a per-source-node concurrency rule. A second live SwiftMigration
// from the same source node is rejected at admission. Phase 1 offline
// migrations don't conflict with live (different state surfaces).
func TestValidate_ModeLivePerSourceNodeConcurrencyRejected(t *testing.T) {
	scheme := migrationScheme(t)
	guest1 := newSwiftGuest("guest1", "default")
	guest1.Spec.ImageRef = nil
	guest1.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
	guest2 := newSwiftGuest("guest2", "default")
	guest2.Spec.ImageRef = nil
	guest2.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
	class := &swiftv1alpha1.SwiftGuestClass{ObjectMeta: metav1.ObjectMeta{Name: "class"}}
	// Existing live migration from sourceNode=boba.
	existing := newSwiftMigration("existing", "default")
	existing.Spec.GuestRef.Name = "guest1"
	existing.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	existing.Status.SourceNode = "boba"
	existing.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		guest1, guest2, class, newReadyKernelNode("miles"), existing,
	).Build()
	v := &Validator{Client: c}

	// New migration also from sourceNode=boba (guest2 lives on boba).
	mig := newSwiftMigration("new", "default")
	mig.Spec.GuestRef.Name = "guest2"
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true

	_, err := v.validate(context.Background(), mig)
	if err == nil {
		t.Fatal("second live SwiftMigration from same source node should be rejected")
	}
	if !strings.Contains(err.Error(), "per-source-node concurrency") {
		t.Errorf("rejection should mention per-source-node concurrency; got %q", err.Error())
	}
}

// TestValidate_ModeLivePerSourceNodeConcurrency_TerminalPeerOk verifies
// that a terminal-phase peer SwiftMigration does NOT block a new
// SwiftMigration from the same source node. Operators creating a
// migration after a previous one Failed/Completed must not be blocked
// by stale state.
func TestValidate_ModeLivePerSourceNodeConcurrency_TerminalPeerOk(t *testing.T) {
	scheme := migrationScheme(t)
	guest1 := newSwiftGuest("guest1", "default")
	guest1.Spec.ImageRef = nil
	guest1.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
	guest2 := newSwiftGuest("guest2", "default")
	guest2.Spec.ImageRef = nil
	guest2.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
	class := &swiftv1alpha1.SwiftGuestClass{ObjectMeta: metav1.ObjectMeta{Name: "class"}}
	completed := newSwiftMigration("completed", "default")
	completed.Spec.GuestRef.Name = "guest1"
	completed.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	completed.Status.SourceNode = "boba"
	completed.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseCompleted

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		guest1, guest2, class, newReadyKernelNode("miles"), completed,
	).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("new", "default")
	mig.Spec.GuestRef.Name = "guest2"
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true

	if _, err := v.validate(context.Background(), mig); err != nil {
		t.Errorf("new live SwiftMigration must not be blocked by Completed peer; got %v", err)
	}
}

// TestValidate_ModeLivePerSourceNodeConcurrency_OfflinePeerOk verifies
// that an in-flight Phase 1 offline SwiftMigration from the same
// source node does NOT block a live SwiftMigration. The two modes
// don't share state surfaces.
func TestValidate_ModeLivePerSourceNodeConcurrency_OfflinePeerOk(t *testing.T) {
	scheme := migrationScheme(t)
	guest1 := newSwiftGuest("guest1", "default")
	guest1.Spec.ImageRef = nil
	guest1.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
	guest2 := newSwiftGuest("guest2", "default")
	guest2.Spec.ImageRef = nil
	guest2.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
	class := &swiftv1alpha1.SwiftGuestClass{ObjectMeta: metav1.ObjectMeta{Name: "class"}}
	offline := newSwiftMigration("offline", "default")
	offline.Spec.GuestRef.Name = "guest1"
	offline.Spec.Mode = migrationv1alpha1.SwiftMigrationModeOffline
	offline.Status.SourceNode = "boba"
	offline.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		guest1, guest2, class, newReadyKernelNode("miles"), offline,
	).Build()
	v := &Validator{Client: c}

	mig := newSwiftMigration("new", "default")
	mig.Spec.GuestRef.Name = "guest2"
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true

	if _, err := v.validate(context.Background(), mig); err != nil {
		t.Errorf("offline peer must not block live; got %v", err)
	}
}
