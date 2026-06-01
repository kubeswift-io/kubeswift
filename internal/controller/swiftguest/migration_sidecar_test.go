package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/migrationsidecar"
)

func migrationSidecarScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{})
	metav1.AddToGroupVersion(s, gvSwift)
	return s
}

func boolPtr(b bool) *bool { return &b }

func TestMigrationEligible(t *testing.T) {
	cases := []struct {
		name string
		spec *swiftv1alpha1.MigrationSpec
		want bool
	}{
		{"nil migration block", nil, true},
		{"enabled unset", &swiftv1alpha1.MigrationSpec{}, true},
		{"enabled true", &swiftv1alpha1.MigrationSpec{Enabled: boolPtr(true)}, true},
		{"enabled false", &swiftv1alpha1.MigrationSpec{Enabled: boolPtr(false)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{Migration: tc.spec}}
			if got := migrationEligible(g); got != tc.want {
				t.Errorf("migrationEligible=%v, want %v", got, tc.want)
			}
		})
	}
}

// podWithLauncher is a minimal launcher pod for sidecar-injection tests.
func podWithLauncher() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "team-a"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "launcher", Image: "swiftletd:latest"}},
		},
	}
}

func TestApplyMigrationSourceSidecar_InjectsIdleClient(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "team-a"}}
	pod := podWithLauncher()

	applyMigrationSourceSidecar(pod, guest)

	// Launcher still first; sidecar appended.
	if pod.Spec.Containers[0].Name != "launcher" {
		t.Fatalf("launcher must remain first container; got %q", pod.Spec.Containers[0].Name)
	}
	var sc *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == migrationsidecar.ContainerName {
			sc = &pod.Spec.Containers[i]
		}
	}
	if sc == nil {
		t.Fatalf("source sidecar %q not injected", migrationsidecar.ContainerName)
	}

	env := map[string]string{}
	for _, e := range sc.Env {
		env[e.Name] = e.Value
	}
	if env[migrationsidecar.EnvRole] != migrationsidecar.RoleClient {
		t.Errorf("sidecar role: want %q, got %q", migrationsidecar.RoleClient, env[migrationsidecar.EnvRole])
	}
	// Idle client: peer SAN + dst IP are NOT env (they arrive via files).
	if _, ok := env[migrationsidecar.EnvCheckHost]; ok {
		t.Errorf("client sidecar must NOT set CHECK_HOST in env (reads from downward-API file)")
	}
	if _, ok := env[migrationsidecar.EnvDstPodIP]; ok {
		t.Errorf("client sidecar must NOT set DST_POD_IP in env (reads from downward-API file)")
	}
	if env[migrationsidecar.EnvInputDir] != migrationsidecar.InputDir {
		t.Errorf("client sidecar must set STUNNEL_INPUT_DIR=%q", migrationsidecar.InputDir)
	}

	// Volumes: config CM, per-guest identity Secret, downward-API input.
	vols := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v
	}
	cfg, ok := vols[migrationsidecar.ConfigVolumeName]
	if !ok || cfg.ConfigMap == nil || cfg.ConfigMap.Name != migrationsidecar.ConfigMapName {
		t.Errorf("config volume must mount ConfigMap %q; got %+v", migrationsidecar.ConfigMapName, cfg.ConfigMap)
	}
	cert, ok := vols[migrationsidecar.CertVolumeName]
	if !ok || cert.Secret == nil || cert.Secret.SecretName != "guest-migration-identity" {
		t.Errorf("identity volume must mount Secret guest-migration-identity; got %+v", cert.Secret)
	}
	in, ok := vols[migrationsidecar.InputVolumeName]
	if !ok || in.DownwardAPI == nil {
		t.Fatalf("input volume must be a downward-API volume; got %+v", in)
	}
	wantPaths := map[string]string{
		migrationsidecar.InputFileDstIP:   "metadata.annotations['" + migrationsidecar.AnnotationDstPodIP + "']",
		migrationsidecar.InputFilePeerSAN: "metadata.annotations['" + migrationsidecar.AnnotationPeerSAN + "']",
	}
	gotPaths := map[string]string{}
	for _, it := range in.DownwardAPI.Items {
		if it.FieldRef != nil {
			gotPaths[it.Path] = it.FieldRef.FieldPath
		}
	}
	for path, field := range wantPaths {
		if gotPaths[path] != field {
			t.Errorf("downward-API item %q: want fieldPath %q, got %q", path, field, gotPaths[path])
		}
	}

	// Idempotent: second call does not duplicate.
	nContainers := len(pod.Spec.Containers)
	nVolumes := len(pod.Spec.Volumes)
	applyMigrationSourceSidecar(pod, guest)
	if len(pod.Spec.Containers) != nContainers || len(pod.Spec.Volumes) != nVolumes {
		t.Errorf("applyMigrationSourceSidecar not idempotent: containers %d->%d, volumes %d->%d",
			nContainers, len(pod.Spec.Containers), nVolumes, len(pod.Spec.Volumes))
	}
}

// TestApplyMigrationSourceSidecar_SetsLauncherMTLSEnv verifies the PR 4
// secured-mode signal: the launcher container gets KUBESWIFT_MIGRATION_MTLS=1
// so swiftletd enters secured mode (S1 loopback enforcement + ack bypass).
// The env goes on the launcher, NOT the stunnel sidecar.
func TestApplyMigrationSourceSidecar_SetsLauncherMTLSEnv(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "team-a"}}
	pod := podWithLauncher()

	applyMigrationSourceSidecar(pod, guest)

	var launcher *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "launcher" {
			launcher = &pod.Spec.Containers[i]
		}
	}
	if launcher == nil {
		t.Fatal("launcher container missing")
	}
	var got string
	var count int
	for _, e := range launcher.Env {
		if e.Name == migrationsidecar.EnvMTLSEnabled {
			got = e.Value
			count++
		}
	}
	if got != migrationsidecar.EnvMTLSEnabledValue {
		t.Errorf("launcher %s: want %q, got %q", migrationsidecar.EnvMTLSEnabled, migrationsidecar.EnvMTLSEnabledValue, got)
	}
	// Idempotent: re-applying must not duplicate the env var.
	applyMigrationSourceSidecar(pod, guest)
	count = 0
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name != "launcher" {
			continue
		}
		for _, e := range pod.Spec.Containers[i].Env {
			if e.Name == migrationsidecar.EnvMTLSEnabled {
				count++
			}
		}
	}
	if count != 1 {
		t.Errorf("KUBESWIFT_MIGRATION_MTLS must appear exactly once after re-apply; got %d", count)
	}
}

func TestEnsureMigrationStunnelConfig_CopiesAcrossNamespaces(t *testing.T) {
	scheme := migrationSidecarScheme(t)
	const sysNS = "kubeswift-system"
	src := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: migrationsidecar.ConfigMapName, Namespace: sysNS},
		Data:       map[string]string{"server.conf": "S", "client.conf": "C"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(src).Build()
	ctx := context.Background()

	if err := EnsureMigrationStunnelConfig(ctx, c, sysNS, "team-a"); err != nil {
		t.Fatalf("EnsureMigrationStunnelConfig: %v", err)
	}
	var got corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: migrationsidecar.ConfigMapName}, &got); err != nil {
		t.Fatalf("copied ConfigMap not found in team-a: %v", err)
	}
	if got.Data["server.conf"] != "S" || got.Data["client.conf"] != "C" {
		t.Errorf("copied ConfigMap data mismatch: %+v", got.Data)
	}
}

func TestEnsureMigrationStunnelConfig_SameNamespaceFastPath(t *testing.T) {
	scheme := migrationSidecarScheme(t)
	// No source ConfigMap seeded; same-namespace must be a no-op (no error).
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	if err := EnsureMigrationStunnelConfig(context.Background(), c, "kubeswift-system", "kubeswift-system"); err != nil {
		t.Errorf("same-namespace fast path must be a no-op; got %v", err)
	}
}

func TestEnsureMigrationStunnelConfig_MissingSourceErrors(t *testing.T) {
	scheme := migrationSidecarScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	if err := EnsureMigrationStunnelConfig(context.Background(), c, "kubeswift-system", "team-a"); err == nil {
		t.Errorf("missing source ConfigMap must error (operator misconfig); got nil")
	}
}

func TestEnsurePerGuestMigrationIdentitySecret_CreatesEmptyOwnedPlaceholder(t *testing.T) {
	scheme := migrationSidecarScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "team-a", UID: "guest-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	ctx := context.Background()

	if err := EnsurePerGuestMigrationIdentitySecret(ctx, c, scheme, guest); err != nil {
		t.Fatalf("EnsurePerGuestMigrationIdentitySecret: %v", err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "guest-migration-identity"}, &s); err != nil {
		t.Fatalf("placeholder Secret not created: %v", err)
	}
	if s.Type != corev1.SecretTypeOpaque {
		t.Errorf("placeholder must be Opaque (TLS type requires keys); got %q", s.Type)
	}
	if len(s.Data) != 0 {
		t.Errorf("placeholder must be empty; got %d keys", len(s.Data))
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != "guest" {
		t.Errorf("placeholder must be owned by the SwiftGuest; got %+v", s.OwnerReferences)
	}
}

func TestEnsurePerGuestMigrationIdentitySecret_DoesNotClobberPopulated(t *testing.T) {
	scheme := migrationSidecarScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "team-a", UID: "guest-uid"}}
	// Simulate a migration having populated the identity.
	populated := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "guest-migration-identity", Namespace: "team-a"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"tls.crt": []byte("CRT"), "tls.key": []byte("KEY"), "ca.crt": []byte("CA")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, populated).Build()
	ctx := context.Background()

	if err := EnsurePerGuestMigrationIdentitySecret(ctx, c, scheme, guest); err != nil {
		t.Fatalf("EnsurePerGuestMigrationIdentitySecret: %v", err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "guest-migration-identity"}, &s); err != nil {
		t.Fatalf("Secret missing after ensure: %v", err)
	}
	if string(s.Data["tls.crt"]) != "CRT" {
		t.Errorf("ensure must NOT clobber a migration-populated identity; tls.crt=%q", s.Data["tls.crt"])
	}
}
