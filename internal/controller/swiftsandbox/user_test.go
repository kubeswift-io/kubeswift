package swiftsandbox

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/random"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

func TestWorkloadUser(t *testing.T) {
	sc := func(u, g *int64, nonRoot, ape *bool) *sandboxv1alpha1.SandboxSecurityContext {
		return &sandboxv1alpha1.SandboxSecurityContext{RunAsUser: u, RunAsGroup: g, RunAsNonRoot: nonRoot, AllowPrivilegeEscalation: ape}
	}
	for _, tc := range []struct {
		name  string
		sc    *sandboxv1alpha1.SandboxSecurityContext
		image string
		want  *runtimeintent.WorkloadUser
	}{
		{"image without USER, no securityContext", nil, "", nil},
		{"image USER only", nil, "app", &runtimeintent.WorkloadUser{User: "app"}},
		{"image USER with group", nil, "1000:1000", &runtimeintent.WorkloadUser{User: "1000:1000"}},
		{"runAsUser replaces the image user, keeps its group", sc(ptr.To[int64](2000), nil, nil, nil), "app:staff", &runtimeintent.WorkloadUser{User: "2000:staff", Explicit: true}},
		{"runAsGroup replaces the image group", sc(nil, ptr.To[int64](3000), nil, nil), "app", &runtimeintent.WorkloadUser{User: "app:3000", Explicit: true}},
		{"runAsGroup on an image without USER", sc(nil, ptr.To[int64](3000), nil, nil), "", &runtimeintent.WorkloadUser{User: "0:3000", Explicit: true}},
		{"runAsUser 0 over a non-root image", sc(ptr.To[int64](0), nil, nil, nil), "app", &runtimeintent.WorkloadUser{User: "0", Explicit: true}},
		{"allowPrivilegeEscalation false alone", sc(nil, nil, nil, ptr.To(false)), "", &runtimeintent.WorkloadUser{NoNewPrivs: true, Explicit: true}},
		{"allowPrivilegeEscalation true is the default", sc(nil, nil, nil, ptr.To(true)), "", nil},
		{"runAsNonRoot false is the default", sc(nil, nil, ptr.To(false), nil), "app", &runtimeintent.WorkloadUser{User: "app"}},
		{"runAsNonRoot", sc(nil, nil, ptr.To(true), nil), "app", &runtimeintent.WorkloadUser{User: "app", NonRoot: true, Explicit: true}},
	} {
		if got := workloadUser(tc.sc, tc.image); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestRunAsNonRootViolation(t *testing.T) {
	for _, tc := range []struct {
		w    *runtimeintent.WorkloadUser
		want bool
	}{
		{nil, false},
		{&runtimeintent.WorkloadUser{User: ""}, false},
		{&runtimeintent.WorkloadUser{NonRoot: true}, true},
		{&runtimeintent.WorkloadUser{User: "0", NonRoot: true}, true},
		{&runtimeintent.WorkloadUser{User: "0:1000", NonRoot: true}, true},
		{&runtimeintent.WorkloadUser{User: "root", NonRoot: true}, true},
		{&runtimeintent.WorkloadUser{User: "1000", NonRoot: true}, false},
		// A name the guest resolves: it refuses there if it is uid 0.
		{&runtimeintent.WorkloadUser{User: "app", NonRoot: true}, false},
	} {
		if _, got := runAsNonRootViolation(tc.w); got != tc.want {
			t.Errorf("%+v: violation=%v, want %v", tc.w, got, tc.want)
		}
	}
}

// The image USER and the securityContext reach the config disk; a bare image
// with only a USER still gets one.
func TestBuild_WorkloadUserOnTheConfigDisk(t *testing.T) {
	sb := plainSandbox("x")
	sb.Spec.SecurityContext = &sandboxv1alpha1.SandboxSecurityContext{AllowPrivilegeEscalation: ptr.To(false)}
	exec := resolveExec(sb, materialize.ImageConfig{User: "nonroot"})
	ri := buildIntent(sb, "sandbox", "/r.ext4", "", exec, false)
	want := &runtimeintent.WorkloadUser{User: "nonroot", NoNewPrivs: true, Explicit: true}
	if ri.SandboxExec == nil || !reflect.DeepEqual(ri.SandboxExec.User, want) {
		t.Fatalf("intent user = %+v, want %+v", ri.SandboxExec, want)
	}
	if !strings.Contains(ri.KernelBoot.Cmdline, "kubeswift.config=") {
		t.Errorf("no config disk for a workload user: %q", ri.KernelBoot.Cmdline)
	}
}

func TestImageUserIgnored(t *testing.T) {
	pod := func(features *string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
		if features != nil {
			p.Annotations[bridgeFeaturesAnnotation] = *features
		}
		return p
	}
	old, cur := "files mounts", "files run-as-user"
	image := &runtimeintent.WorkloadUser{User: "app"}
	if imageUserIgnored(image, pod(&old)) == "" {
		t.Error("an image user on a bridge without run-as-user said nothing")
	}
	if msg := imageUserIgnored(image, pod(&cur)); msg != "" {
		t.Errorf("bridge with run-as-user: %q", msg)
	}
	if msg := imageUserIgnored(image, pod(nil)); msg != "" {
		t.Errorf("launcher that advertised nothing yet: %q", msg)
	}
	if msg := imageUserIgnored(&runtimeintent.WorkloadUser{User: "app", Explicit: true}, pod(&old)); msg != "" {
		t.Errorf("an explicit user is refused by swiftletd, not warned about: %q", msg)
	}
}

// A checkout runs as the pool image's USER under its own securityContext, and
// a runAsNonRoot that the image breaks claims no slot.
func TestCheckout_WorkloadUser(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		imageUser  string
		sc         *sandboxv1alpha1.SandboxSecurityContext
		wantFailed bool
		wantUser   *runtimeintent.WorkloadUser
	}{
		{"image user", "1000", nil, false, &runtimeintent.WorkloadUser{User: "1000"}},
		{"securityContext", "", &sandboxv1alpha1.SandboxSecurityContext{RunAsUser: ptr.To[int64](4242), AllowPrivilegeEscalation: ptr.To(false)}, false,
			&runtimeintent.WorkloadUser{User: "4242", NoNewPrivs: true, Explicit: true}},
		{"runAsNonRoot on a root image", "", &sandboxv1alpha1.SandboxSecurityContext{RunAsNonRoot: ptr.To(true)}, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := shapedPool()
			pool.Namespace = "default"
			pool.Status.ImageUser = tc.imageUser
			slot := readySlot("p-slot-aaaaa", poolSlotProfile(pool))
			slot.Namespace = "default"
			sb := matchingSandbox()
			sb.Namespace = "default"
			sb.Spec.SecurityContext = tc.sc
			r, c := sandboxReconciler(pool, slot, sb)
			reconcileSB(t, r, "sb")
			got := getSandbox(t, c, "sb")
			var p corev1.Pod
			if err := c.Get(ctx, client.ObjectKeyFromObject(slot), &p); err != nil {
				t.Fatal(err)
			}
			if tc.wantFailed {
				if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || !strings.Contains(got.Status.Message, "runAsNonRoot") {
					t.Fatalf("phase=%s message=%q, want Failed for runAsNonRoot", got.Status.Phase, got.Status.Message)
				}
				if got.Status.PodRef != "" || p.Annotations[annSandboxExecActionID] != "" {
					t.Errorf("a slot was claimed (podRef %q) for a sandbox that cannot run", got.Status.PodRef)
				}
				return
			}
			if got.Status.PodRef != slot.Name {
				t.Fatalf("want a checkout of %s, got %q (%s)", slot.Name, got.Status.PodRef, got.Status.Message)
			}
			var args struct {
				User *runtimeintent.WorkloadUser `json:"user"`
			}
			if err := json.Unmarshal([]byte(p.Annotations[annSandboxExecActionArgs]), &args); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(args.User, tc.wantUser) {
				t.Errorf("action args user = %+v, want %+v", args.User, tc.wantUser)
			}
			if got.Status.User != tc.wantUser.User {
				t.Errorf("status.user = %q, want %q", got.Status.User, tc.wantUser.User)
			}
		})
	}
}

// Cold: runAsNonRoot over an image without USER fails before any launcher;
// runAsUser launches and records the user.
func TestCreateLaunch_WorkloadUser(t *testing.T) {
	reg := newABIRegistry(t)
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref := reg.write("test/plain:1", img)
	for _, tc := range []struct {
		name       string
		sc         *sandboxv1alpha1.SandboxSecurityContext
		wantFailed bool
		wantUser   string
	}{
		{"runAsNonRoot, image runs as root", &sandboxv1alpha1.SandboxSecurityContext{RunAsNonRoot: ptr.To(true)}, true, ""},
		{"runAsUser", &sandboxv1alpha1.SandboxSecurityContext{RunAsUser: ptr.To[int64](1000), RunAsNonRoot: ptr.To(true)}, false, "1000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := plainSandbox(ref)
			sb.Spec.SecurityContext = tc.sc
			r, c := sandboxReconciler(sb, readyKernel("default", "sandbox"))
			r.Recorder = record.NewFakeRecorder(20)
			var got *sandboxv1alpha1.SwiftSandbox
			launched := false
			for i := 0; i < 50; i++ {
				reconcileSB(t, r, "sb")
				got = getSandbox(t, c, "sb")
				var pod corev1.Pod
				launched = c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod) == nil
				if launched || got.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if failed := got.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed; failed != tc.wantFailed {
				t.Fatalf("failed=%v launched=%v message=%q, want failed=%v", failed, launched, got.Status.Message, tc.wantFailed)
			}
			if tc.wantFailed {
				if launched {
					t.Error("a launcher exists for a runAsNonRoot sandbox that would run as root")
				}
				return
			}
			if !launched || got.Status.User != tc.wantUser {
				t.Fatalf("launched=%v status.user=%q, want %q", launched, got.Status.User, tc.wantUser)
			}
		})
	}
}

// The bridge is advertised after the launcher container is already running, so
// a sandbox can be Running before its launcher shows an old bridge. The
// dropped image USER is still reported, once, and status.user says root.
func TestImageUserIgnored_AfterRunning(t *testing.T) {
	sb := plainSandbox("busybox:1")
	sb.UID = "sb-uid"
	sb.Status.Phase = sandboxv1alpha1.SwiftSandboxRunning
	sb.Status.PodRef = "sb"
	sb.Status.User = "app"
	pod := ownLauncher(sb, buildPod(sb, "sandbox"))
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[bridgeFeaturesAnnotation] = "files mounts"
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: launcherName, Ready: true}}}
	r, c := sandboxReconciler(sb, pod, readyKernel("default", "sandbox"))
	rec := record.NewFakeRecorder(20)
	r.Recorder = rec
	for i := 0; i < 3; i++ {
		reconcileSB(t, r, "sb")
	}
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxRunning || got.Status.User != "" {
		t.Fatalf("phase=%s user=%q, want Running with no user (root)", got.Status.Phase, got.Status.User)
	}
	n := 0
	for len(rec.Events) > 0 {
		if e := <-rec.Events; strings.Contains(e, reasonImageUserIgnored) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d %s events, want 1", n, reasonImageUserIgnored)
	}
}
