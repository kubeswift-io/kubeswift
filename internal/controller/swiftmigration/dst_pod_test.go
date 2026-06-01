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

	dst, err := newDstPod(mig, guest, src, scheme, dstSidecarConfig{})
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
	// mTLS off (zero config): no stunnel sidecar, no extra volumes.
	if len(dst.Spec.Containers) != 1 {
		t.Errorf("mTLS-off dst pod must have exactly the launcher container; got %d containers", len(dst.Spec.Containers))
	}
	for _, c := range dst.Spec.Containers {
		if c.Name == stunnelSidecarContainerName {
			t.Errorf("mTLS-off dst pod must NOT carry the stunnel sidecar")
		}
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

	if _, err := newDstPod(mig, guest, src, scheme, dstSidecarConfig{}); err == nil {
		t.Errorf("expected error when launcher container is missing")
	}
}

// TestNewDstPod_MTLS_InjectsServerSidecar verifies the Phase 3c
// destination-side wiring: with mTLS enabled, newDstPod appends the
// stunnel server sidecar (role=server, peer-SAN pinned to the SOURCE
// node) plus the config + identity-Secret volumes, and leaves the
// launcher container untouched.
func TestNewDstPod_MTLS_InjectsServerSidecar(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigrationWithUID("mig-a", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Target.NodeName = "miles"
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"},
	}
	src := templateSrcPod("guest", "default")

	dst, err := newDstPod(mig, guest, src, scheme, dstSidecarConfig{
		mtlsEnabled: true,
		srcNodeName: "boba",  // source node — dst pins this SAN
		dstNodeName: "miles", // destination node — dst presents this identity
	})
	if err != nil {
		t.Fatalf("newDstPod (mTLS): %v", err)
	}

	// Launcher container still present and is still the FIRST container
	// (receiver-env lookup + launcherContainerImage rely on finding it by
	// name, but its primacy keeps existing index-0 assumptions valid).
	if dst.Spec.Containers[0].Name != LauncherContainerName {
		t.Fatalf("launcher must remain the first container; got %q", dst.Spec.Containers[0].Name)
	}

	// Exactly one stunnel sidecar, role=server, CHECK_HOST=source node.
	var sidecar *corev1.Container
	for i := range dst.Spec.Containers {
		if dst.Spec.Containers[i].Name == stunnelSidecarContainerName {
			sidecar = &dst.Spec.Containers[i]
			break
		}
	}
	if sidecar == nil {
		t.Fatalf("mTLS dst pod must carry the %q sidecar", stunnelSidecarContainerName)
	}
	env := map[string]string{}
	for _, e := range sidecar.Env {
		env[e.Name] = e.Value
	}
	if env[envStunnelRole] != stunnelRoleServer {
		t.Errorf("sidecar role: want %q, got %q", stunnelRoleServer, env[envStunnelRole])
	}
	if env[envStunnelCheckHost] != "boba" {
		t.Errorf("sidecar CHECK_HOST: want source node %q, got %q", "boba", env[envStunnelCheckHost])
	}
	if _, present := env[envStunnelDstPodIP]; present {
		t.Errorf("server-role sidecar must NOT set DST_POD_IP (that is client-only, PR 3b)")
	}

	// Config + identity volumes present; identity Secret is the DST node's.
	wantVols := map[string]bool{stunnelConfigVolumeName: false, stunnelCertVolumeName: false}
	for _, v := range dst.Spec.Volumes {
		if _, ok := wantVols[v.Name]; ok {
			wantVols[v.Name] = true
		}
		if v.Name == stunnelCertVolumeName {
			if v.Secret == nil || v.Secret.SecretName != "kubeswift-migration-node-miles" {
				t.Errorf("identity volume must mount the DST node Secret kubeswift-migration-node-miles; got %+v", v.Secret)
			}
		}
		if v.Name == stunnelConfigVolumeName {
			if v.ConfigMap == nil || v.ConfigMap.Name != stunnelConfigMapName {
				t.Errorf("config volume must mount %q; got %+v", stunnelConfigMapName, v.ConfigMap)
			}
		}
	}
	for name, found := range wantVols {
		if !found {
			t.Errorf("mTLS dst pod missing expected volume %q", name)
		}
	}
}

// TestNewDstPod_MTLS_FlipsInheritedClientSidecarToServer verifies W-3c-2:
// when the source pod already carries a CLIENT sidecar (PR 3b), newDstPod's
// DeepCopy brings it over and the dst construction must FLIP it to a SERVER
// (and replace the client-only volumes: drop the downward-API input volume,
// swap the per-guest identity Secret for the dst per-node Secret). Without
// this, the dst would run a misconfigured client and mTLS would break even
// for the first migration.
func TestNewDstPod_MTLS_FlipsInheritedClientSidecarToServer(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigrationWithUID("mig-a", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Target.NodeName = "miles"
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"},
	}
	src := templateSrcPod("guest", "default")
	// Source carries a CLIENT sidecar + the client-only volumes (as PR 3b
	// produces): config, per-guest identity Secret, downward-API input.
	src.Spec.Containers = append(src.Spec.Containers, corev1.Container{
		Name: stunnelSidecarContainerName,
		Env: []corev1.EnvVar{
			{Name: envStunnelRole, Value: stunnelRoleClient},
			{Name: "STUNNEL_INPUT_DIR", Value: "/etc/migration-input"},
		},
	})
	src.Spec.Volumes = append(src.Spec.Volumes,
		corev1.Volume{Name: stunnelConfigVolumeName},
		corev1.Volume{Name: stunnelCertVolumeName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "guest-migration-identity"}}},
		corev1.Volume{Name: "migration-input", VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{}}},
	)

	dst, err := newDstPod(mig, guest, src, scheme, dstSidecarConfig{
		mtlsEnabled: true, srcNodeName: "boba", dstNodeName: "miles",
	})
	if err != nil {
		t.Fatalf("newDstPod: %v", err)
	}

	// Exactly ONE migration-stunnel container, role=server.
	var count int
	var sc *corev1.Container
	for i := range dst.Spec.Containers {
		if dst.Spec.Containers[i].Name == stunnelSidecarContainerName {
			count++
			sc = &dst.Spec.Containers[i]
		}
	}
	if count != 1 {
		t.Fatalf("want exactly 1 stunnel sidecar after flip, got %d", count)
	}
	role := ""
	for _, e := range sc.Env {
		if e.Name == envStunnelRole {
			role = e.Value
		}
	}
	if role != stunnelRoleServer {
		t.Errorf("inherited client sidecar must be flipped to server; got role %q", role)
	}

	// The downward-API input volume must be gone; the cert volume must now
	// point at the dst per-node Secret, not the inherited per-guest one.
	for _, v := range dst.Spec.Volumes {
		if v.Name == "migration-input" {
			t.Errorf("dst must not carry the client downward-API input volume")
		}
		if v.Name == stunnelCertVolumeName {
			if v.Secret == nil || v.Secret.SecretName != "kubeswift-migration-node-miles" {
				t.Errorf("dst cert volume must be the dst per-node Secret; got %+v", v.Secret)
			}
		}
	}
}

// TestNewDstPod_MTLS_EmptyNode_Errors verifies the guard: mTLS enabled
// with an unresolved node name is a clean construction error rather than
// a sidecar with an empty SAN pin / unresolvable identity Secret.
func TestNewDstPod_MTLS_EmptyNode_Errors(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigrationWithUID("m", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Target.NodeName = "miles"
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"},
	}
	src := templateSrcPod("guest", "default")

	if _, err := newDstPod(mig, guest, src, scheme, dstSidecarConfig{
		mtlsEnabled: true,
		srcNodeName: "", // unresolved
		dstNodeName: "miles",
	}); err == nil {
		t.Errorf("expected error when mTLS enabled but source node name empty")
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
