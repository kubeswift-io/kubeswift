package swiftmigration

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/scheme"
)

func srcNode(name, guestKey, pci string) *gpuv1alpha1.SwiftGPUNode {
	return &gpuv1alpha1.SwiftGPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: gpuv1alpha1.SwiftGPUNodeStatus{
			VfioReady: true,
			GPUModel:  "NVIDIA Corporation GP104 [GeForce GTX 1080]",
			FreeGPUs:  0,
			GPUs: []gpuv1alpha1.GPUDevice{
				{Index: 0, PCIAddress: pci, NUMANode: 0, Allocated: true, AllocatedTo: guestKey},
			},
		},
	}
}

func tgtNode(name, pci string, vfioReady bool) *gpuv1alpha1.SwiftGPUNode {
	free := 0
	if vfioReady {
		free = 1
	}
	return &gpuv1alpha1.SwiftGPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: gpuv1alpha1.SwiftGPUNodeStatus{
			VfioReady: vfioReady,
			GPUModel:  "NVIDIA Corporation GP104 [GeForce GTX 1080]",
			FreeGPUs:  free,
			GPUs: []gpuv1alpha1.GPUDevice{
				{Index: 0, PCIAddress: pci, NUMANode: 0},
			},
		},
	}
}

func gpuProfileObj() *gpuv1alpha1.SwiftGPUProfile {
	return &gpuv1alpha1.SwiftGPUProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       gpuv1alpha1.SwiftGPUProfileSpec{Count: 1, PartitionMode: "isolated"},
	}
}

func gpuGuestOnSrc(srcNodeName, pci string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default", UID: "g-uid"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			GPUProfileRef: &corev1.LocalObjectReference{Name: "p"},
		},
		Status: swiftv1alpha1.SwiftGuestStatus{
			GPU: &swiftv1alpha1.GPUStatus{NodeName: srcNodeName, Devices: []string{pci}, Hypervisor: "cloud-hypervisor", PartitionID: -1},
		},
	}
}

func gpuMig() *migrationv1alpha1.SwiftMigration {
	return &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec: migrationv1alpha1.SwiftMigrationSpec{
			GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: "g"},
			Target:   migrationv1alpha1.SwiftMigrationTarget{NodeName: "boba"},
			Mode:     migrationv1alpha1.SwiftMigrationModeOffline,
		},
	}
}

func gpuReconciler(objs ...client.Object) (*SwiftMigrationReconciler, client.Client) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}, &swiftv1alpha1.SwiftGuest{}).
		WithObjects(objs...).Build()
	return &SwiftMigrationReconciler{Client: c, Scheme: scheme.Scheme}, c
}

func nodeAllocTo(t *testing.T, c client.Client, node, pci string) string {
	t.Helper()
	var n gpuv1alpha1.SwiftGPUNode
	if err := c.Get(context.Background(), types.NamespacedName{Name: node}, &n); err != nil {
		t.Fatalf("get node %q: %v", node, err)
	}
	for _, g := range n.Status.GPUs {
		if g.PCIAddress == pci {
			return g.AllocatedTo
		}
	}
	return "<no-such-gpu>"
}

func TestReserveTargetGPUs(t *testing.T) {
	r, c := gpuReconciler(srcNode("miles", "default/g", "0000:aa:00.0"),
		tgtNode("boba", "0000:bb:00.0", true), gpuProfileObj())
	g := gpuGuestOnSrc("miles", "0000:aa:00.0")

	if res := r.reserveTargetGPUs(context.Background(), gpuMig(), g, &migrationv1alpha1.SwiftMigrationStatus{}); res != nil {
		t.Fatalf("reserveTargetGPUs returned a phaseResult (failure): %+v", res)
	}
	// Target GPU is now reserved for the guest; source is still allocated.
	if got := nodeAllocTo(t, c, "boba", "0000:bb:00.0"); got != "default/g" {
		t.Errorf("target GPU should be reserved for default/g; got %q", got)
	}
	if got := nodeAllocTo(t, c, "miles", "0000:aa:00.0"); got != "default/g" {
		t.Errorf("source GPU must stay allocated to default/g pre-cutover; got %q", got)
	}
}

func TestReserveTargetGPUs_NotVfioReady_Fails(t *testing.T) {
	r, _ := gpuReconciler(srcNode("miles", "default/g", "0000:aa:00.0"),
		tgtNode("boba", "0000:bb:00.0", false), gpuProfileObj())
	g := gpuGuestOnSrc("miles", "0000:aa:00.0")

	if res := r.reserveTargetGPUs(context.Background(), gpuMig(), g, &migrationv1alpha1.SwiftMigrationStatus{}); res == nil {
		t.Errorf("reserve on a non-vfio-ready target must fail (before stopping the source)")
	}
}

func TestCutoverGPUs_ReleasesSourceAndStampsTarget(t *testing.T) {
	g := gpuGuestOnSrc("miles", "0000:aa:00.0")
	r, c := gpuReconciler(srcNode("miles", "default/g", "0000:aa:00.0"),
		tgtNode("boba", "0000:bb:00.0", true), gpuProfileObj(), g)
	mig := gpuMig()

	// Reserve first (as Preparing would), then cut over.
	if res := r.reserveTargetGPUs(context.Background(), mig, g, &migrationv1alpha1.SwiftMigrationStatus{}); res != nil {
		t.Fatalf("reserve: %+v", res)
	}
	if res := r.cutoverGPUs(context.Background(), mig, g, &migrationv1alpha1.SwiftMigrationStatus{}); res != nil {
		t.Fatalf("cutoverGPUs returned a phaseResult: %+v", res)
	}

	// Source freed, target committed.
	if got := nodeAllocTo(t, c, "miles", "0000:aa:00.0"); got != "" {
		t.Errorf("source GPU must be freed at cutover; got %q", got)
	}
	if got := nodeAllocTo(t, c, "boba", "0000:bb:00.0"); got != "default/g" {
		t.Errorf("target GPU must stay allocated to default/g; got %q", got)
	}
	// guest.status.GPU stamped to the target.
	var gg swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "g"}, &gg); err != nil {
		t.Fatalf("get guest: %v", err)
	}
	if gg.Status.GPU == nil || gg.Status.GPU.NodeName != "boba" {
		t.Fatalf("status.GPU.NodeName must be boba after cutover; got %+v", gg.Status.GPU)
	}
	if len(gg.Status.GPU.Devices) != 1 || gg.Status.GPU.Devices[0] != "0000:bb:00.0" {
		t.Errorf("status.GPU.Devices must be the target device; got %v", gg.Status.GPU.Devices)
	}
	if gg.Status.GPU.Hypervisor != "cloud-hypervisor" {
		t.Errorf("hypervisor must be preserved; got %q", gg.Status.GPU.Hypervisor)
	}
}

func TestCutoverGPUs_Idempotent(t *testing.T) {
	g := gpuGuestOnSrc("miles", "0000:aa:00.0")
	r, c := gpuReconciler(srcNode("miles", "default/g", "0000:aa:00.0"),
		tgtNode("boba", "0000:bb:00.0", true), gpuProfileObj(), g)
	mig := gpuMig()
	_ = r.reserveTargetGPUs(context.Background(), mig, g, &migrationv1alpha1.SwiftMigrationStatus{})
	_ = r.cutoverGPUs(context.Background(), mig, g, &migrationv1alpha1.SwiftMigrationStatus{})
	// Re-fetch the guest (status.GPU now boba) and cut over again — no-op.
	var gg swiftv1alpha1.SwiftGuest
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "g"}, &gg)
	if res := r.cutoverGPUs(context.Background(), mig, &gg, &migrationv1alpha1.SwiftMigrationStatus{}); res != nil {
		t.Errorf("cutover must be idempotent once status.GPU is on the target; got %+v", res)
	}
}

func TestReleaseTargetReservation(t *testing.T) {
	r, c := gpuReconciler(srcNode("miles", "default/g", "0000:aa:00.0"),
		tgtNode("boba", "0000:bb:00.0", true), gpuProfileObj())
	g := gpuGuestOnSrc("miles", "0000:aa:00.0")
	mig := gpuMig()
	if res := r.reserveTargetGPUs(context.Background(), mig, g, &migrationv1alpha1.SwiftMigrationStatus{}); res != nil {
		t.Fatalf("reserve: %+v", res)
	}
	if err := r.releaseTargetReservation(context.Background(), mig, g); err != nil {
		t.Fatalf("releaseTargetReservation: %v", err)
	}
	if got := nodeAllocTo(t, c, "boba", "0000:bb:00.0"); got != "" {
		t.Errorf("target reservation must be released; got %q", got)
	}
	// Source untouched (the guest resumes there).
	if got := nodeAllocTo(t, c, "miles", "0000:aa:00.0"); got != "default/g" {
		t.Errorf("source must stay allocated after a pre-cutover release; got %q", got)
	}
}

func TestGPUPreflight(t *testing.T) {
	r, _ := gpuReconciler(tgtNode("boba", "0000:bb:00.0", true), gpuProfileObj())
	if res := r.gpuPreflight(context.Background(), gpuMig(), gpuGuestOnSrc("miles", "0000:aa:00.0")); res != nil {
		t.Errorf("pre-flight on a vfio-ready target with free GPUs should pass; got %+v", res)
	}

	r2, _ := gpuReconciler(tgtNode("boba", "0000:bb:00.0", false), gpuProfileObj())
	if res := r2.gpuPreflight(context.Background(), gpuMig(), gpuGuestOnSrc("miles", "0000:aa:00.0")); res == nil {
		t.Errorf("pre-flight on a non-vfio-ready target should fail")
	}
}
