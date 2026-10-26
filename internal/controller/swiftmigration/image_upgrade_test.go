package swiftmigration

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
)

// A live migration copies the source launcher pod to the destination, images
// included, so a guest keeps the launcher it was started with. These tests pin
// that a chart upgrade (a new controller default launcher image) does not stop
// a guest started before it from migrating, and that the copy never carries
// an image patched into the source pod after it was created.

const (
	oldLauncher = "ghcr.io/test/swiftletd:v1.0.0"
	newLauncher = "ghcr.io/test/swiftletd:v2.0.0"
)

// launcherSourcePod is a guest's launcher pod as the SwiftGuest controller
// built it with image, and as its kubelet runs it.
func launcherSourcePod(guestName, image string) *corev1.Pod {
	p := newSourcePod(guestName, "default", "src-uid")
	p.Spec.InitContainers = []corev1.Container{{Name: "network-init", Image: image}}
	p.Spec.Containers = []corev1.Container{{Name: LauncherContainerName, Image: image}}
	return ranAsCreated(p)
}

// migrateToPreparing runs Validating then Preparing for a migration and
// returns the destination pod Preparing created. handle is the Validating
// handler under test (handleValidating for a mode the dispatcher resolves,
// handleValidatingLive for mode=live).
func migrateToPreparing(t *testing.T, objs []client.Object, mig *migrationv1alpha1.SwiftMigration,
	validate func(*SwiftMigrationReconciler, context.Context, *migrationv1alpha1.SwiftMigration, *migrationv1alpha1.SwiftMigrationStatus) *phaseResult,
) (*corev1.Pod, *phaseResult) {
	t.Helper()
	ctx := context.Background()
	scheme := validatingScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, mig)...).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20)}

	status := mig.Status.DeepCopy()
	if res := validate(r, ctx, mig, status); res.FailureMsg != "" || res.Err != nil {
		return nil, res
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhasePreparing {
		t.Fatalf("after Validating: phase %q, want Preparing", status.Phase)
	}
	mig.Status = *status
	status = mig.Status.DeepCopy()
	if res := r.handlePreparingLive(ctx, mig, status); res.FailureMsg != "" || res.Err != nil {
		return nil, res
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.MatchingLabels{LabelMigrationRole: MigrationRoleDestination}); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("destination pods: %d, want 1", len(pods.Items))
	}
	return &pods.Items[0], nil
}

func validateLive(r *SwiftMigrationReconciler, ctx context.Context, m *migrationv1alpha1.SwiftMigration, s *migrationv1alpha1.SwiftMigrationStatus) *phaseResult {
	return r.handleValidatingLive(ctx, m, s)
}

func validateDispatch(r *SwiftMigrationReconciler, ctx context.Context, m *migrationv1alpha1.SwiftMigration, s *migrationv1alpha1.SwiftMigrationStatus) *phaseResult {
	return r.handleValidating(ctx, m, s)
}

func liveMigration() *migrationv1alpha1.SwiftMigration {
	mig := newMigrationWithUID("m", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.AllowIPChange = true
	return mig
}

func assertDstImages(t *testing.T, dst *corev1.Pod, want string) {
	t.Helper()
	for _, c := range append(append([]corev1.Container{}, dst.Spec.InitContainers...), dst.Spec.Containers...) {
		if c.Image != want {
			t.Errorf("destination container %q image %q, want the source's %q", c.Name, c.Image, want)
		}
	}
}

func explicitObjs(guestName, srcImage string) []client.Object {
	guest := newGuestForValidating(guestName, "default", "class-default")
	guest.Spec.Interfaces = nil // default node-local networking
	return []client.Object{
		guest,
		newGuestClass("class-default", 2, 2048),
		newSpaciousNode("worker-2", 8, 65536),
		launcherSourcePod(guestName, srcImage),
	}
}

// Same release: the source launcher is the controller's current default.
func TestLiveMigration_SameRelease_Passes(t *testing.T) {
	t.Setenv(swiftguest.LauncherImageEnv, oldLauncher)
	dst, res := migrateToPreparing(t, explicitObjs("guest", oldLauncher), liveMigration(), validateLive)
	if res != nil {
		t.Fatalf("migration refused: %+v", res)
	}
	assertDstImages(t, dst, oldLauncher)
}

// The bug: a guest started before a chart upgrade runs the previous release's
// launcher; the upgraded controller's default is the new one. An explicit
// SwiftMigration must pass, and the destination keeps the old launcher.
func TestLiveMigration_SourceFromPreviousRelease_ExplicitMigrationPasses(t *testing.T) {
	t.Setenv(swiftguest.LauncherImageEnv, newLauncher)
	dst, res := migrateToPreparing(t, explicitObjs("guest", oldLauncher), liveMigration(), validateLive)
	if res != nil {
		t.Fatalf("pre-upgrade guest refused after the upgrade: reason=%s msg=%q err=%v", res.FailureReason, res.FailureMsg, res.Err)
	}
	assertDstImages(t, dst, oldLauncher)
}

// Node drain after an upgrade: the drain controller creates the SwiftMigration
// (swiftdrain.createDrainMigration): mode auto for drainPolicy Migrate, live
// for LiveMigrate, AllowIPChange, reason node-drain. Both reach the live path
// and must move the pre-upgrade guest on its own launcher.
func TestLiveMigration_NodeDrainAfterUpgrade_Passes(t *testing.T) {
	for _, mode := range []migrationv1alpha1.SwiftMigrationMode{
		migrationv1alpha1.SwiftMigrationModeAuto, migrationv1alpha1.SwiftMigrationModeLive,
	} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv(swiftguest.LauncherImageEnv, newLauncher)
			enabled := true
			guest := newGuestForValidating("drained", "default", "live-class")
			guest.Spec.Migration = &swiftv1alpha1.MigrationSpec{Enabled: &enabled, PreferredMode: "auto"}
			guest.Spec.Interfaces = nil
			guest.Status.NodeName = "worker-2"
			objs := []client.Object{
				guest,
				newGuestClass("live-class", 2, 4096),
				newSpaciousNode("worker-1", 8, 65536),
				launcherSourcePod("drained", oldLauncher),
			}
			mig := newMigrationWithUID("drain-m", "default", "0123456789abcdef0123456789abcdef")
			mig.Labels = map[string]string{"kubeswift.io/drain": "true"}
			mig.Spec = migrationv1alpha1.SwiftMigrationSpec{
				GuestRef:      migrationv1alpha1.SwiftMigrationGuestRef{Name: "drained"},
				Target:        migrationv1alpha1.SwiftMigrationTarget{NodeName: "worker-1"},
				Mode:          mode,
				Reason:        "node-drain",
				AllowIPChange: true,
				Timeout:       &metav1.Duration{Duration: 5 * 60 * 1e9},
			}
			mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating

			dst, res := migrateToPreparing(t, objs, mig, validateDispatch)
			if res != nil {
				t.Fatalf("drain migration of a pre-upgrade guest refused: reason=%s msg=%q err=%v", res.FailureReason, res.FailureMsg, res.Err)
			}
			if dst.Spec.NodeName != "worker-1" {
				t.Errorf("destination node %q, want worker-1", dst.Spec.NodeName)
			}
			assertDstImages(t, dst, oldLauncher)
		})
	}
}

// What the old check partly guarded against, now checked against what the
// copy depends on: a source pod whose spec was changed after creation (the
// API server bumps metadata.generation, labels and annotations excepted)
// must not be copied to the destination, whichever container was patched
// and whatever the controller's default is. A container the kubelet already
// restarted on a patched image is refused by its restart count.
func TestLiveMigration_PatchedSourcePod_Refused(t *testing.T) {
	cases := []struct {
		name  string
		patch func(*corev1.Pod)
		want  string
	}{
		{"launcher container, not yet restarted", func(p *corev1.Pod) {
			p.Spec.Containers[0].Image = "evil.example/root:1"
			p.Generation = 2
		}, "generation 2"},
		{"init container", func(p *corev1.Pod) {
			p.Spec.InitContainers[0].Image = "evil.example/root:1"
			p.Generation = 2
		}, "generation 2"},
		{"patched to the new default", func(p *corev1.Pod) {
			p.Spec.Containers[0].Image = newLauncher
			p.Generation = 2
		}, "generation 2"},
		{"restarted on the patched image", func(p *corev1.Pod) {
			p.Spec.Containers[0].Image = "evil.example/root:1"
			p.Status.ContainerStatuses[0].Image = "evil.example/root:1"
			p.Status.ContainerStatuses[0].RestartCount = 1
		}, `container "launcher" restarted`},
		{"restarted, API server without generation", func(p *corev1.Pod) {
			p.Generation = 0
			p.Spec.Containers[0].Image = "evil.example/root:1"
			p.Status.ContainerStatuses[0].Image = "evil.example/root:1"
			p.Status.ContainerStatuses[0].RestartCount = 1
		}, `container "launcher" restarted`},
		{"init container, API server without generation", func(p *corev1.Pod) {
			p.Generation = 0
			p.Spec.InitContainers[0].Image = "evil.example/root:1"
		}, `container "network-init"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(swiftguest.LauncherImageEnv, newLauncher)
			objs := explicitObjs("guest", oldLauncher)
			tc.patch(objs[3].(*corev1.Pod))
			_, res := migrateToPreparing(t, objs, liveMigration(), validateLive)
			if res == nil || res.FailureReason != migrationv1alpha1.FailureReasonImageTagMismatch {
				t.Fatalf("got %+v, want Failed/ImageTagMismatch", res)
			}
			if !strings.Contains(res.FailureMsg, tc.want) {
				t.Errorf("message %q should contain %q", res.FailureMsg, tc.want)
			}
		})
	}
}

// An untouched pod on an API server without generation: containerd reports a
// container's image as the image's first tag, so a launcher started from one
// tag reports another tag of the same image (measured on containerd 1.7.30,
// where swiftletd:v0.20.0 and an alias tag share a digest). That is not a
// patch; another repository is.
func TestLiveMigration_NoGeneration_TagAliasPasses(t *testing.T) {
	t.Setenv(swiftguest.LauncherImageEnv, newLauncher)
	objs := explicitObjs("guest", "ghcr.io/test/swiftletd:sha-1234567")
	src := objs[3].(*corev1.Pod)
	src.Generation = 0
	src.Status.InitContainerStatuses[0].Image = oldLauncher
	src.Status.ContainerStatuses[0].Image = oldLauncher
	dst, res := migrateToPreparing(t, objs, liveMigration(), validateLive)
	if res != nil {
		t.Fatalf("untouched pod refused: %+v", res)
	}
	assertDstImages(t, dst, "ghcr.io/test/swiftletd:sha-1234567")
}

// A pod at the guest's launcher name that the guest does not control is not
// the guest's launcher: never copy it.
func TestLiveMigration_SourcePodNotControlledByGuest_Refused(t *testing.T) {
	t.Setenv(swiftguest.LauncherImageEnv, oldLauncher)
	objs := explicitObjs("guest", oldLauncher)
	objs[3].(*corev1.Pod).OwnerReferences = nil
	_, res := migrateToPreparing(t, objs, liveMigration(), validateLive)
	if res == nil || res.FailureReason != migrationv1alpha1.FailureReasonOther || !strings.Contains(res.FailureMsg, "not controlled by SwiftGuest") {
		t.Fatalf("got %+v, want Failed/Other naming the ownership", res)
	}
}

// Without generation, statuses not reported yet: retry, do not fail.
func TestLiveMigration_NoGeneration_ImageNotReported_Retries(t *testing.T) {
	t.Setenv(swiftguest.LauncherImageEnv, oldLauncher)
	objs := explicitObjs("guest", oldLauncher)
	src := objs[3].(*corev1.Pod)
	src.Generation = 0
	src.Status.ContainerStatuses = nil
	_, res := migrateToPreparing(t, objs, liveMigration(), validateLive)
	if res == nil || res.Err == nil || res.FailureMsg != "" {
		t.Fatalf("got %+v, want a transient error", res)
	}
}

// Validating checked an earlier read of the source pod; Preparing copies the
// one it reads now. A patch in between must still be refused, before any
// destination pod exists.
func TestPreparingLive_SourcePatchedAfterValidating_Refused(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, src := preparingLiveFixture(t, "uid-1")
	src.Spec.Containers[0].Image = "evil.example/root:1"
	src.Generation = 2
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, src).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	res := r.handlePreparingLive(context.Background(), mig, mig.Status.DeepCopy())
	if res.FailureReason != migrationv1alpha1.FailureReasonImageTagMismatch {
		t.Fatalf("got %+v, want Failed/ImageTagMismatch", res)
	}
	var pods corev1.PodList
	_ = c.List(context.Background(), &pods, client.MatchingLabels{LabelMigrationRole: MigrationRoleDestination})
	if len(pods.Items) != 0 {
		t.Errorf("a destination pod was created from a patched source: %d", len(pods.Items))
	}
}

// Re-entry (leader handover) adopts an existing destination pod only if it
// has the source's images. One patched after creation is a conflict; one with
// the source's pre-upgrade launcher is the expected pod.
func TestPreparingLive_ExistingDstImages(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dstImage string
		conflict bool
	}{
		{"source launcher after an upgrade", "kubeswift/swiftletd:latest", false},
		{"patched", "evil.example/root:1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(swiftguest.LauncherImageEnv, newLauncher)
			scheme := testScheme(t)
			mig, guest, src := preparingLiveFixture(t, "uid-1")
			dst := preExistingDstPod(mig, guest, scheme, t)
			dst.Spec.Containers[0].Image = tc.dstImage
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, src, dst).WithStatusSubresource(mig).Build()
			r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

			res := r.handlePreparingLive(context.Background(), mig, mig.Status.DeepCopy())
			if tc.conflict {
				if res.FailureReason != migrationv1alpha1.FailureReasonDstPodConflict || !strings.Contains(res.FailureMsg, "evil.example") {
					t.Fatalf("got %+v, want Failed/DstPodConflict naming the image", res)
				}
				return
			}
			if res.FailureMsg != "" || res.Err != nil {
				t.Fatalf("existing destination with the source's images refused: %+v", res)
			}
		})
	}
}

func TestImageRepository(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/kubeswift-io/kubeswift/swiftletd:v0.20.0":             "ghcr.io/kubeswift-io/kubeswift/swiftletd",
		"ghcr.io/kubeswift-io/kubeswift/swiftletd@sha256:abcd":         "ghcr.io/kubeswift-io/kubeswift/swiftletd",
		"ghcr.io/kubeswift-io/kubeswift/swiftletd:v0.20.0@sha256:abcd": "ghcr.io/kubeswift-io/kubeswift/swiftletd",
		"busybox:1.36":       "docker.io/library/busybox",
		"busybox":            "docker.io/library/busybox",
		"alpine/socat:1.8":   "docker.io/alpine/socat",
		"localhost:5000/x:1": "localhost:5000/x",
		"localhost:5000/x":   "localhost:5000/x",
		"registry:5000/a/b":  "registry:5000/a/b",
	} {
		if got := imageRepository(in); got != want {
			t.Errorf("imageRepository(%q) = %q, want %q", in, got, want)
		}
	}
}
