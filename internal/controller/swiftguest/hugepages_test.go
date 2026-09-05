package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

func baseResources(memMiB int) corev1.ResourceRequirements {
	q := *resource.NewQuantity(int64(memMiB+LauncherMemoryOverheadMiB)*1024*1024, resource.BinarySI)
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: q},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: q},
	}
}

// The whole point of the feature: guest RAM MOVES to hugepages, it is not
// requested twice. The kubelet subtracts reserved hugepages from allocatable
// `memory`, so double-booking would consume 2x the guest's RAM from the node
// and wedge scheduling well before the hugepages ran out.
func TestApplyHugepagesMovesGuestRAMOffOrdinaryMemory(t *testing.T) {
	const mem = 4096
	res := baseResources(mem)
	rg := &resolved.ResolvedGuest{}
	rg.Hugepages = "2Mi"

	vol, mount := applyHugepages(&res, rg, mem)
	if vol == nil || mount == nil {
		t.Fatal("expected a hugetlbfs volume and mount")
	}

	hp, ok := res.Limits["hugepages-2Mi"]
	if !ok {
		t.Fatal("no hugepages-2Mi limit set")
	}
	if got := hp.Value(); got != int64(mem)*1024*1024 {
		t.Errorf("hugepages-2Mi = %d, want %d (the full guest RAM)", got, int64(mem)*1024*1024)
	}

	// Ordinary memory must now be ONLY the launcher overhead.
	wantOverhead := int64(LauncherMemoryOverheadMiB) * 1024 * 1024
	if got := res.Limits[corev1.ResourceMemory]; got.Value() != wantOverhead {
		t.Errorf("memory limit = %d, want %d — guest RAM must not be booked twice",
			got.Value(), wantOverhead)
	}

	// Kubernetes requires request == limit for hugepages.
	if req := res.Requests["hugepages-2Mi"]; req.Value() != hp.Value() {
		t.Errorf("hugepages request %d != limit %d; Kubernetes rejects unequal hugepage requests",
			req.Value(), hp.Value())
	}
}

// The emptyDir medium must name the SIZE. A bare "HugePages" medium is only
// unambiguous on a node offering one page size, and these nodes expose both.
func TestApplyHugepagesUsesSizeQualifiedMedium(t *testing.T) {
	for _, tc := range []struct{ size, wantMedium, wantResource string }{
		{"2Mi", "HugePages-2Mi", "hugepages-2Mi"},
		{"1Gi", "HugePages-1Gi", "hugepages-1Gi"},
	} {
		res := baseResources(2048)
		rg := &resolved.ResolvedGuest{}
		rg.Hugepages = tc.size

		vol, mount := applyHugepages(&res, rg, 2048)
		if vol == nil {
			t.Fatalf("%s: no volume", tc.size)
		}
		if got := string(vol.VolumeSource.EmptyDir.Medium); got != tc.wantMedium {
			t.Errorf("%s: medium = %q, want %q", tc.size, got, tc.wantMedium)
		}
		if _, ok := res.Limits[corev1.ResourceName(tc.wantResource)]; !ok {
			t.Errorf("%s: missing %s limit", tc.size, tc.wantResource)
		}
		if mount.MountPath != HugepagesMountPath {
			t.Errorf("%s: mount path = %q, want %q", tc.size, mount.MountPath, HugepagesMountPath)
		}
	}
}

// Unset must be a complete no-op — every existing class keeps its exact
// resource block and gains no volume.
func TestApplyHugepagesUnsetIsANoOp(t *testing.T) {
	const mem = 4096
	res := baseResources(mem)
	before := res.Limits[corev1.ResourceMemory]
	rg := &resolved.ResolvedGuest{} // Hugepages == ""

	vol, mount := applyHugepages(&res, rg, mem)
	if vol != nil || mount != nil {
		t.Fatal("unset hugepages must not add a volume or mount")
	}
	if got := res.Limits[corev1.ResourceMemory]; got.Value() != before.Value() {
		t.Errorf("memory limit changed with hugepages unset: %d -> %d", before.Value(), got.Value())
	}
	for name := range res.Limits {
		if name != corev1.ResourceMemory {
			t.Errorf("unexpected resource added: %s", name)
		}
	}
}

// The CRD speaks Kubernetes units; Cloud Hypervisor rejects them. If this
// translation is lost, CH fails with Conversion("hugepage_size", "2Mi").
func TestGetHugepagesTranslatesToCloudHypervisorUnits(t *testing.T) {
	for _, tc := range []struct{ in, wantCH, wantResource string }{
		{"", "", ""},
		{"2Mi", "2M", "hugepages-2Mi"},
		{"1Gi", "1G", "hugepages-1Gi"},
	} {
		rg := &resolved.ResolvedGuest{}
		rg.Hugepages = tc.in
		if got := rg.GetHugepages(); got != tc.wantCH {
			t.Errorf("GetHugepages(%q) = %q, want %q", tc.in, got, tc.wantCH)
		}
		if got := rg.GetHugepagesResourceName(); got != tc.wantResource {
			t.Errorf("GetHugepagesResourceName(%q) = %q, want %q", tc.in, got, tc.wantResource)
		}
	}
}
