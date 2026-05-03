package swiftmigration

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// templateSrcPod builds a minimal source-pod fixture suitable for
// dst-pod construction tests. Has the load-bearing bits: a launcher
// container, a few labels and annotations, and a node assignment that
// the helper must override.
func templateSrcPod(guestName, ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            guestName,
			Namespace:       ns,
			UID:             "src-uid",
			ResourceVersion: "100",
			Labels: map[string]string{
				LabelGuestName:           guestName,
				"app.kubernetes.io/name": "kubeswift",
			},
			Annotations: map[string]string{
				"kubeswift.io/guest-ip": "10.0.0.5",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      "boba",
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  LauncherContainerName,
				Image: "kubeswift/swiftletd:latest",
				Env: []corev1.EnvVar{
					{Name: "FOO", Value: "bar"},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

func newMigrationWithUID(name, ns, uid string) *migrationv1alpha1.SwiftMigration {
	mig := newMigration(name, ns)
	mig.UID = types.UID(uid)
	return mig
}

func TestDstPodName_DeterministicShortUID(t *testing.T) {
	mig := newMigrationWithUID("m1", "default", "abcdef1234567890abcdef1234567890")
	got, err := dstPodName(mig, "guest")
	if err != nil {
		t.Fatalf("dstPodName: %v", err)
	}
	if got != "guest-mig-abcdef" {
		t.Errorf("name: want guest-mig-abcdef, got %q", got)
	}

	// Same SwiftMigration UID must yield the same name across calls.
	got2, _ := dstPodName(mig, "guest")
	if got != got2 {
		t.Errorf("dstPodName non-deterministic: %q vs %q", got, got2)
	}
}

func TestDstPodName_DifferentMigrations_DifferentNames(t *testing.T) {
	a := newMigrationWithUID("m1", "default", "aaaaaaaa-1111-2222-3333-444444444444")
	b := newMigrationWithUID("m2", "default", "bbbbbbbb-1111-2222-3333-444444444444")
	nameA, _ := dstPodName(a, "guest")
	nameB, _ := dstPodName(b, "guest")
	if nameA == nameB {
		t.Errorf("two SwiftMigrations on same SwiftGuest must produce distinct dst pod names; got both %q", nameA)
	}
}

func TestDstPodName_EmptyUID_Errors(t *testing.T) {
	mig := newMigration("m", "default")
	mig.UID = ""
	if _, err := dstPodName(mig, "guest"); err == nil {
		t.Errorf("empty UID must yield error; got nil")
	}
}

func TestDstPodName_ShortUID_Errors(t *testing.T) {
	mig := newMigrationWithUID("m", "default", "abc")
	if _, err := dstPodName(mig, "guest"); err == nil {
		t.Errorf("UID shorter than %d chars must yield error", shortUIDLength)
	}
}

func TestDstPodName_OversizeName_Errors(t *testing.T) {
	mig := newMigrationWithUID("m", "default", "abcdef1234567890abcdef1234567890")
	bigGuest := strings.Repeat("a", 60) // 60 + len("-mig-abcdef")=11 = 71 > 64
	if _, err := dstPodName(mig, bigGuest); err == nil {
		t.Errorf("oversize guest name must yield error (DNS-1123 cap)")
	}
}

func TestNewDstPod_SetsNameLabelsAnnotationsEnvNodeName(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigrationWithUID("mig-a", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Target.NodeName = "miles"
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"},
	}
	src := templateSrcPod("guest", "default")

	dst, err := newDstPod(mig, guest, src, scheme)
	if err != nil {
		t.Fatalf("newDstPod: %v", err)
	}

	// Name
	if dst.Name != "guest-mig-abcdef" {
		t.Errorf("name: want guest-mig-abcdef, got %q", dst.Name)
	}
	// Namespace
	if dst.Namespace != "default" {
		t.Errorf("namespace: want default, got %q", dst.Namespace)
	}
	// Stale metadata stripped
	if dst.UID != "" {
		t.Errorf("UID should be reset; got %q", dst.UID)
	}
	if dst.ResourceVersion != "" {
		t.Errorf("ResourceVersion should be reset; got %q", dst.ResourceVersion)
	}
	// Labels: guest, migration-role, migration-name, plus preserved labels
	if dst.Labels[LabelGuestName] != "guest" {
		t.Errorf("guest label: want guest, got %q", dst.Labels[LabelGuestName])
	}
	if dst.Labels[LabelMigrationRole] != MigrationRoleDestination {
		t.Errorf("migration-role label: want destination, got %q", dst.Labels[LabelMigrationRole])
	}
	if dst.Labels[LabelMigrationName] != "mig-a" {
		t.Errorf("migration label: want mig-a, got %q", dst.Labels[LabelMigrationName])
	}
	if dst.Labels["app.kubernetes.io/name"] != "kubeswift" {
		t.Errorf("preserved src label missing")
	}
	// Annotations: ack present, guest-ip dropped (src runtime annotations)
	if dst.Annotations[AnnotationMigrationPhase2Ack] != AnnotationMigrationPhase2AckValue {
		t.Errorf("ack annotation: want %q=%q, got %q", AnnotationMigrationPhase2Ack, AnnotationMigrationPhase2AckValue, dst.Annotations[AnnotationMigrationPhase2Ack])
	}
	if _, present := dst.Annotations["kubeswift.io/guest-ip"]; present {
		t.Errorf("src runtime annotation kubeswift.io/guest-ip should not be on dst pod")
	}
	// OwnerRef on SwiftGuest
	if len(dst.OwnerReferences) != 1 {
		t.Fatalf("ownerRefs: want 1, got %d", len(dst.OwnerReferences))
	}
	owner := dst.OwnerReferences[0]
	if owner.Kind != "SwiftGuest" || owner.Name != "guest" {
		t.Errorf("ownerRef: want SwiftGuest/guest, got %s/%s", owner.Kind, owner.Name)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Errorf("ownerRef.Controller: want true")
	}
	// NodeName overridden to dst node
	if dst.Spec.NodeName != "miles" {
		t.Errorf("NodeName: want miles, got %q", dst.Spec.NodeName)
	}
	// KUBESWIFT_MIGRATION_ROLE=receiver added; preserved env preserved
	envs := dst.Spec.Containers[0].Env
	foundReceiver := false
	foundFoo := false
	for _, e := range envs {
		if e.Name == EnvKubeswiftMigrationRole && e.Value == EnvKubeswiftMigrationRoleReceiver {
			foundReceiver = true
		}
		if e.Name == "FOO" && e.Value == "bar" {
			foundFoo = true
		}
	}
	if !foundReceiver {
		t.Errorf("KUBESWIFT_MIGRATION_ROLE=receiver missing on launcher container")
	}
	if !foundFoo {
		t.Errorf("preserved FOO env missing on launcher container")
	}
	// Status reset
	if dst.Status.Phase != "" {
		t.Errorf("status should be reset; got phase=%q", dst.Status.Phase)
	}
}

func TestNewDstPod_NoLauncherContainer_Errors(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigrationWithUID("m", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Target.NodeName = "miles"
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"},
	}
	src := templateSrcPod("guest", "default")
	src.Spec.Containers[0].Name = "not-launcher"

	if _, err := newDstPod(mig, guest, src, scheme); err == nil {
		t.Errorf("expected error when launcher container is missing")
	}
}

func TestDstPodMatches_GoodLabels_ReturnsTrue(t *testing.T) {
	mig := newMigration("mig-a", "default")
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest"}}
	tru := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "guest-mig-abcdef",
			Labels: map[string]string{
				LabelGuestName:     "guest",
				LabelMigrationRole: MigrationRoleDestination,
				LabelMigrationName: "mig-a",
			},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "SwiftGuest", Name: "guest", Controller: &tru},
			},
		},
	}
	if !dstPodMatches(pod, mig, guest) {
		t.Errorf("matching pod: want true")
	}
}

func TestDstPodMatches_WrongOwner_ReturnsFalse(t *testing.T) {
	mig := newMigration("mig-a", "default")
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				LabelGuestName:     "guest",
				LabelMigrationRole: MigrationRoleDestination,
				LabelMigrationName: "mig-a",
			},
			// No ownerRef set (or wrong kind)
		},
	}
	if dstPodMatches(pod, mig, guest) {
		t.Errorf("wrong ownerRef: want false")
	}
}

func TestDstPodMatches_WrongMigrationName_ReturnsFalse(t *testing.T) {
	mig := newMigration("mig-a", "default")
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest"}}
	tru := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				LabelGuestName:     "guest",
				LabelMigrationRole: MigrationRoleDestination,
				LabelMigrationName: "mig-b", // different
			},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "SwiftGuest", Name: "guest", Controller: &tru},
			},
		},
	}
	if dstPodMatches(pod, mig, guest) {
		t.Errorf("wrong migration label: want false")
	}
}

func TestDstPodReady_RunningAndReady_True(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	if !dstPodReady(pod) {
		t.Errorf("Running + Ready=True must yield true")
	}
}

func TestDstPodReady_RunningButNotReady_False(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}
	if dstPodReady(pod) {
		t.Errorf("Ready=False must yield false")
	}
}

func TestDstPodReady_Pending_False(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}
	if dstPodReady(pod) {
		t.Errorf("phase=Pending must yield false")
	}
}
