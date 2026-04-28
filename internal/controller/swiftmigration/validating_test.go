package swiftmigration

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// validatingScheme extends the controller test scheme with
// SwiftGuestClass (cluster-scoped, registered separately).
func validatingScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := testScheme(t)
	s.AddKnownTypes(
		schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"},
		&swiftv1alpha1.SwiftGuestClass{}, &swiftv1alpha1.SwiftGuestClassList{},
	)
	return s
}

func newGuestClass(name string, cpu, memMi int64) *swiftv1alpha1.SwiftGuestClass {
	return &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: swiftv1alpha1.SwiftGuestClassSpec{
			CPU:    *resource.NewQuantity(cpu, resource.DecimalSI),
			Memory: *resource.NewQuantity(memMi*1024*1024, resource.BinarySI),
			RootDisk: swiftv1alpha1.RootDiskSpec{
				Size:   resource.MustParse("40Gi"),
				Format: swiftv1alpha1.DiskFormatRaw,
			},
		},
	}
}

func newSpaciousNode(name string, cpu, memMi int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(cpu, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(memMi*1024*1024, resource.BinarySI),
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func newGuestForValidating(name, ns, classRef string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: classRef},
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "macvlan", Namespace: ns}},
			},
		},
		Status: swiftv1alpha1.SwiftGuestStatus{
			NodeName: "boba",
		},
	}
}

func TestValidating_HappyPath(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, class, node).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	advanced, _, errMsg, err := r.handleValidating(context.Background(), mig, status)
	if err != nil {
		t.Fatalf("handleValidating returned err = %v", err)
	}
	if errMsg != "" {
		t.Fatalf("handleValidating returned errMsg = %q, want empty", errMsg)
	}
	if !advanced {
		t.Fatal("handleValidating should advance to Preparing on success")
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhasePreparing {
		t.Errorf("phase = %q, want Preparing", status.Phase)
	}
	if status.Mode != migrationv1alpha1.SwiftMigrationModeOffline {
		t.Errorf("mode = %q, want offline", status.Mode)
	}
	if status.SourceNode != "boba" || status.DestinationNode != "miles" {
		t.Errorf("source/destination = %s→%s, want boba→miles", status.SourceNode, status.DestinationNode)
	}
	// Compatible condition must be True.
	found := false
	for _, c := range status.Conditions {
		if c.Type == migrationv1alpha1.SwiftMigrationConditionCompatible && c.Status == metav1.ConditionTrue {
			found = true
		}
	}
	if !found {
		t.Error("Compatible=True condition should be set on validation success")
	}
}

func TestValidating_GuestDisappeared(t *testing.T) {
	scheme := validatingScheme(t)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	// No SwiftGuest in the cluster.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, err := r.handleValidating(context.Background(), mig, status)
	if err != nil {
		t.Fatalf("handleValidating returned err = %v", err)
	}
	if !strings.Contains(errMsg, "no longer exists") {
		t.Errorf("errMsg = %q, want mention of 'no longer exists'", errMsg)
	}
}

func TestValidating_TargetNodeMissing(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if !strings.Contains(errMsg, "no longer exists") || !strings.Contains(errMsg, "miles") {
		t.Errorf("errMsg = %q, want mention of missing target node", errMsg)
	}
}

func TestValidating_InsufficientCPU(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "big-class")
	class := newGuestClass("big-class", 64, 2048) // 64 CPU
	node := newSpaciousNode("miles", 8, 65536)    // 8 allocatable
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if !strings.Contains(errMsg, "insufficient CPU") {
		t.Errorf("errMsg = %q, want mention of insufficient CPU", errMsg)
	}
	// Operator-actionable: must include both need and have.
	if !strings.Contains(errMsg, "need") || !strings.Contains(errMsg, "have") {
		t.Errorf("errMsg should report need vs have; got %q", errMsg)
	}
}

func TestValidating_InsufficientMemory(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "big-class")
	// 64Gi memory request — node has 4Gi.
	class := newGuestClass("big-class", 1, 64*1024)
	node := newSpaciousNode("miles", 8, 4096)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if !strings.Contains(errMsg, "insufficient memory") {
		t.Errorf("errMsg = %q, want mention of insufficient memory", errMsg)
	}
}

func TestValidating_ExistingPodsCountedInCapacity(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 4, 4096)
	node := newSpaciousNode("miles", 6, 8192) // headroom only just enough for 1 guest
	// Existing pod consumes 4 CPU on miles, leaving 2 CPU headroom < 4 needed.
	hogger := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "hogger", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "miles",
			Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(4, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(1024*1024*1024, resource.BinarySI),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node, hogger).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if !strings.Contains(errMsg, "insufficient CPU") {
		t.Errorf("errMsg should reflect existing pod consumption; got %q", errMsg)
	}
}

func TestValidating_FailedPodsExcludedFromCapacity(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 4, 4096)
	// A Failed pod on miles should NOT count toward used resources.
	failed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "failed", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "miles",
			Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: *resource.NewQuantity(8, resource.DecimalSI),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node, failed).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	advanced, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if errMsg != "" {
		t.Errorf("Failed pod should not consume capacity; got errMsg=%q", errMsg)
	}
	if !advanced {
		t.Error("validation should pass with only Failed pod on target node")
	}
}

func TestValidating_AllowIPChangeSetsCondition(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Spec.Interfaces = nil // default networking
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	mig := newMigration("m", "default")
	mig.Spec.AllowIPChange = true
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	r.handleValidating(context.Background(), mig, status)

	found := false
	for _, c := range status.Conditions {
		if c.Type == migrationv1alpha1.SwiftMigrationConditionIPWillChange && c.Status == metav1.ConditionTrue {
			found = true
		}
	}
	if !found {
		t.Error("IPWillChange=True condition should be set when allowIPChange=true triggers on default networking")
	}
}

func TestValidating_MigrationDisabledMidFlight(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	disabled := false
	guest.Spec.Migration = &swiftv1alpha1.MigrationSpec{Enabled: &disabled}
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if !strings.Contains(errMsg, "migration.enabled=false") {
		t.Errorf("errMsg = %q, want mention of migration.enabled=false", errMsg)
	}
}

func TestValidating_NodeMissingAllocatable(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "miles"},
		Status: corev1.NodeStatus{
			// Allocatable intentionally empty.
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if !strings.Contains(errMsg, "no Allocatable CPU reported") {
		t.Errorf("errMsg = %q, want mention of missing Allocatable", errMsg)
	}
}

func TestValidating_GuestClassNotFound(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "ghost-class")
	// SwiftGuestClass intentionally absent.
	node := newSpaciousNode("miles", 8, 65536)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, node).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if !strings.Contains(errMsg, "SwiftGuestClass") || !strings.Contains(errMsg, "not found") {
		t.Errorf("errMsg = %q, want mention of missing SwiftGuestClass", errMsg)
	}
}

func TestValidating_InsufficientMemory_NeedHaveSubstrings(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "big-class")
	class := newGuestClass("big-class", 1, 64*1024) // 64 GiB memory
	node := newSpaciousNode("miles", 8, 4096)       // 4 GiB allocatable
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if !strings.Contains(errMsg, "need") || !strings.Contains(errMsg, "have") {
		t.Errorf("memory rejection should report need/have substrings; got %q", errMsg)
	}
}

func TestValidating_InitContainerRequestsCounted(t *testing.T) {
	// An existing pod whose ONLY large request is on an init container.
	// The capacity check must count this against headroom (conservative
	// summation) — otherwise a node briefly running an init-heavy pod
	// would report misleading headroom.
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 4, 4096)
	node := newSpaciousNode("miles", 6, 8192)
	initHog := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "init-hog", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "miles",
			InitContainers: []corev1.Container{{
				Name: "init",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI),
					},
				},
			}},
			Containers: []corev1.Container{{Name: "c"}}, // no requests
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node, initHog).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if !strings.Contains(errMsg, "insufficient CPU") {
		t.Errorf("init container request should count toward used CPU; got errMsg=%q", errMsg)
	}
}

func TestValidating_Idempotent(t *testing.T) {
	// Two consecutive calls to handleValidating with the happy-path
	// fixtures must not produce duplicate Conditions of the same type.
	// Important because the reconcile loop may re-enter Validating
	// after a watch event before the first call's status patch lands.
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	r.handleValidating(context.Background(), mig, status)
	firstLen := len(status.Conditions)
	// Reset phase so the handler re-runs (simulating a re-entrant
	// reconcile during the brief window between patch and watch).
	status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating
	r.handleValidating(context.Background(), mig, status)

	if got := len(status.Conditions); got != firstLen {
		t.Errorf("Conditions length after re-run = %d, want %d (no duplicates)", got, firstLen)
	}
	// And no Type appears more than once.
	seen := map[string]int{}
	for _, c := range status.Conditions {
		seen[c.Type]++
	}
	for typ, count := range seen {
		if count > 1 {
			t.Errorf("Condition type %q appears %d times; should be deduped", typ, count)
		}
	}
}

func TestValidating_NodeCordonedMidFlight(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	node := newSpaciousNode("miles", 8, 65536)
	node.Spec.Unschedulable = true
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, node).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if !strings.Contains(errMsg, "cordoned") {
		t.Errorf("errMsg = %q, want mention of cordoned", errMsg)
	}
}

func TestValidating_NodeNotReadyMidFlight(t *testing.T) {
	scheme := validatingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	class := newGuestClass("class-default", 2, 2048)
	notReady := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "miles"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(8, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(65536*1024*1024, resource.BinarySI),
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, class, notReady).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handleValidating(context.Background(), mig, status)
	if !strings.Contains(errMsg, "not Ready") {
		t.Errorf("errMsg = %q, want mention of not Ready", errMsg)
	}
}
