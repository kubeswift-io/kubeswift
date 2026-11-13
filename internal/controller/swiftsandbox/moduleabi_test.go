package swiftsandbox

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// abiRegistry is an in-process registry for the module ABI tests.
type abiRegistry struct {
	t    *testing.T
	host string
}

func newABIRegistry(t *testing.T) *abiRegistry {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &abiRegistry{t: t, host: u.Host}
}

// image pushes a small image whose config carries labels.
func (r *abiRegistry) image(repo string, labels map[string]string) string {
	r.t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		r.t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		r.t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.Config.Labels = labels
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		r.t.Fatal(err)
	}
	return r.write(repo, img)
}

// kernel pushes a kernel artifact shaped like `oras push`: one layer per
// file, each titled with its file name. files maps title to content.
func (r *abiRegistry) kernel(repo string, files map[string]string) string {
	r.t.Helper()
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	for title, content := range files {
		var err error
		img, err = mutate.Append(img, mutate.Addendum{
			Layer:       static.NewLayer([]byte(content), "text/plain"),
			Annotations: map[string]string{"org.opencontainers.image.title": title},
		})
		if err != nil {
			r.t.Fatal(err)
		}
	}
	return r.write(repo, img)
}

func (r *abiRegistry) write(repo string, img v1.Image) string {
	r.t.Helper()
	ref, err := name.ParseReference(r.host + "/" + repo)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		r.t.Fatal(err)
	}
	return ref.String()
}

func TestArtifactFile(t *testing.T) {
	reg := newABIRegistry(t)
	k := reg.kernel("kernels/gpu-sandbox:1", map[string]string{
		"bzImage":    "kernel",
		"module-abi": "6.6.158-ksabi1\n",
	})
	b, found, err := materialize.ArtifactFile(materialize.Options{ImageRef: k}, materialize.ModuleABIFile)
	if err != nil || !found || strings.TrimSpace(string(b)) != "6.6.158-ksabi1" {
		t.Fatalf("module-abi = %q found=%v err=%v", b, found, err)
	}

	old := reg.kernel("kernels/gpu-sandbox:0", map[string]string{"bzImage": "kernel"})
	if _, found, err := materialize.ArtifactFile(materialize.Options{ImageRef: old}, materialize.ModuleABIFile); err != nil || found {
		t.Fatalf("artifact without module-abi: found=%v err=%v, want not found", found, err)
	}

	big := reg.kernel("kernels/gpu-sandbox:big", map[string]string{"module-abi": strings.Repeat("x", 5000)})
	if _, _, err := materialize.ArtifactFile(materialize.Options{ImageRef: big}, materialize.ModuleABIFile); err == nil {
		t.Fatal("a module-abi larger than the cap was read")
	}
}

func TestModuleABIVerdict(t *testing.T) {
	label := func(v string) map[string]string { return map[string]string{materialize.ModuleABILabel: v} }
	for _, tc := range []struct {
		name        string
		kernelABI   string
		kernelFound bool
		labels      map[string]string
		want        string
	}{
		{"match", "6.6.158-ksabi1", true, label("6.6.158-ksabi1"), ""},
		{"mismatch", "6.6.158-ksabi2", true, label("6.6.158-ksabi1"), reasonKernelModuleABIMismatch},
		{"image declares none", "6.6.158-ksabi1", true, nil, reasonModuleABIUndeclared},
		{"image declares empty", "6.6.158-ksabi1", true, label(""), reasonModuleABIUndeclared},
		{"kernel predates the guard", "", false, label("6.6.158-ksabi1"), ""},
		{"neither", "", false, nil, ""},
	} {
		got, msg := moduleABIVerdict("gpu-sandbox", tc.kernelABI, tc.kernelFound, "reg/img:1", tc.labels)
		if got != tc.want {
			t.Errorf("%s: reason %q (%s), want %q", tc.name, got, msg, tc.want)
		}
		if got != "" && (!strings.Contains(msg, "gpu-sandbox") || !strings.Contains(msg, tc.kernelABI)) {
			t.Errorf("%s: message %q does not name the kernel and its ABI", tc.name, msg)
		}
	}
}

// WS1.5: a GPU sandbox whose image declares modules for another kernel fails
// before any launcher exists; a matching or undeclared image, or a kernel
// artifact without module-abi, launches (undeclared with a Warning Event).
func TestCreateLaunch_GPUModuleABI(t *testing.T) {
	reg := newABIRegistry(t)
	guarded := reg.kernel("kernels/gpu-sandbox:2", map[string]string{"bzImage": "k", "module-abi": "6.6.158-ksabi1\n"})
	unguarded := reg.kernel("kernels/gpu-sandbox:1", map[string]string{"bzImage": "k"})
	for _, tc := range []struct {
		name       string
		kernel     string
		labels     map[string]string
		wantFailed bool
		wantEvent  string
	}{
		{"matching image", guarded, map[string]string{materialize.ModuleABILabel: "6.6.158-ksabi1"}, false, ""},
		{"stale image", guarded, map[string]string{materialize.ModuleABILabel: "6.6.158"}, true, ""},
		{"undeclared image", guarded, nil, false, reasonModuleABIUndeclared},
		{"kernel without module-abi", unguarded, map[string]string{materialize.ModuleABILabel: "6.6.158-ksabi1"}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := reg.image("test/gpu:"+strings.ReplaceAll(tc.name, " ", "-"), tc.labels)
			sb := plainSandbox(img)
			sb.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{ResourceClaimTemplateName: "one-gpu"}
			k := readyKernel("default", "gpu-sandbox")
			k.Spec.OCIRef.Image = tc.kernel
			r, c := sandboxReconciler(sb, k)
			rec := record.NewFakeRecorder(20)
			r.Recorder = rec

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
			failed := got.Status.Phase == sandboxv1alpha1.SwiftSandboxFailed
			if failed != tc.wantFailed {
				t.Fatalf("failed=%v launched=%v phase=%s message=%q, want failed=%v", failed, launched, got.Status.Phase, got.Status.Message, tc.wantFailed)
			}
			if tc.wantFailed {
				if launched {
					t.Error("a launcher pod exists for a sandbox with a stale module ABI")
				}
				if !strings.Contains(got.Status.Message, "6.6.158-ksabi1") {
					t.Errorf("message %q does not name the kernel's module ABI", got.Status.Message)
				}
				return
			}
			if !launched {
				t.Fatalf("no launcher pod (phase=%s message=%q)", got.Status.Phase, got.Status.Message)
			}
			if tc.wantEvent != "" && !hasEvent(rec, tc.wantEvent) {
				t.Errorf("no %s Event", tc.wantEvent)
			}
		})
	}
}

func hasEvent(rec *record.FakeRecorder, reason string) bool {
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, reason) {
				return true
			}
		default:
			return false
		}
	}
}

// WS1.5: a warm GPU pool whose image carries modules for another kernel warms
// no slot and so holds no GPU; the pool is Degraded and says why.
func TestPoolReconcile_GPUModuleABIMismatchHoldsNoGPU(t *testing.T) {
	reg := newABIRegistry(t)
	kernelRef := reg.kernel("kernels/gpu-sandbox:2", map[string]string{"bzImage": "k", "module-abi": "6.6.158-ksabi1\n"})
	pool := gpuPool("gp", "default", "gtx")
	pool.Spec.Image = reg.image("test/gpu:stale", map[string]string{materialize.ModuleABILabel: "6.6.158"})
	k := readyKernel("default", gpuSandboxKernelProfile)
	k.Spec.OCIRef.Image = kernelRef
	node := oneGPUNode("worker-1")
	r, c := poolReconciler(pool, node, workerNode("worker-1"), pcieProfile("gtx", "default"), k)

	var got sandboxv1alpha1.SwiftSandboxPool
	for i := 0; i < 50; i++ {
		reconcilePool(t, r, "gp")
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gp"}, &got); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(got.Status.Message, "6.6.158-ksabi1") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxPoolDegraded || !strings.Contains(got.Status.Message, "6.6.158-ksabi1") {
		t.Fatalf("pool phase=%s message=%q, want Degraded naming the kernel's module ABI", got.Status.Phase, got.Status.Message)
	}
	if a := nodeAllocatedTo(t, c); a != "" {
		t.Errorf("a GPU was allocated (%q) for a pool whose image cannot run on its kernel", a)
	}
}
