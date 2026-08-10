package resolved

import (
	"testing"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	seedv1alpha1 "github.com/kubeswift-io/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMerge_GuestRunPolicyOverridesSystemDefault(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns", UID: "uid"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{RunPolicy: swiftv1alpha1.RunPolicyStopped, ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}},
	}
	guestClass := &swiftv1alpha1.SwiftGuestClass{Spec: swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}}}
	image := &imagev1alpha1.SwiftImage{Status: imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}}}

	rg := Merge(guest, guestClass, image, nil)
	if rg.Lifecycle.RunPolicy != "Stopped" {
		t.Errorf("RunPolicy = %q, want Stopped", rg.Lifecycle.RunPolicy)
	}
}

func TestMerge_OSType(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns", UID: "uid"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}},
	}
	guestClass := &swiftv1alpha1.SwiftGuestClass{Spec: swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}}}

	// Windows image -> rg.OSType=windows (image is authoritative for disk boot).
	winImage := &imagev1alpha1.SwiftImage{Spec: imagev1alpha1.SwiftImageSpec{OSType: imagev1alpha1.OSTypeWindows}, Status: imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}}}
	if rg := Merge(guest, guestClass, winImage, nil); rg.GetOSType() != "windows" || !rg.IsWindows() {
		t.Errorf("windows image: OSType=%q IsWindows=%v, want windows/true", rg.GetOSType(), rg.IsWindows())
	}

	// Image with osType unset -> linux (legacy, no behaviour change).
	linImage := &imagev1alpha1.SwiftImage{Status: imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}}}
	if rg := Merge(guest, guestClass, linImage, nil); rg.GetOSType() != "linux" || rg.IsWindows() {
		t.Errorf("unset image osType: OSType=%q, want linux", rg.GetOSType())
	}

	// Kernel boot (image == nil) -> always linux.
	if rg := Merge(guest, guestClass, nil, nil); rg.GetOSType() != "linux" {
		t.Errorf("kernel boot: OSType=%q, want linux", rg.GetOSType())
	}
}

func TestMerge_ClassCPUUsedWhenGuestNoOverride(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}, Spec: swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}}}
	guestClass := &swiftv1alpha1.SwiftGuestClass{Spec: swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("4"), Memory: resource.MustParse("4Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("20Gi"), Format: swiftv1alpha1.DiskFormatRaw}}}
	image := &imagev1alpha1.SwiftImage{Status: imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}}}

	rg := Merge(guest, guestClass, image, nil)
	if rg.Resources.CPU != 4 {
		t.Errorf("CPU = %d, want 4", rg.Resources.CPU)
	}
}

func TestMerge_ClassMemoryUsedWhenGuestNoOverride(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}, Spec: swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}}}
	guestClass := &swiftv1alpha1.SwiftGuestClass{Spec: swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("4096Mi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}}}
	image := &imagev1alpha1.SwiftImage{Status: imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}}}

	rg := Merge(guest, guestClass, image, nil)
	if rg.Resources.Memory != 4096 {
		t.Errorf("Memory = %d MiB, want 4096", rg.Resources.Memory)
	}
}

func TestMerge_SystemDefaultArchitectureWhenOmit(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}, Spec: swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}}}
	guestClass := &swiftv1alpha1.SwiftGuestClass{Spec: swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}}}
	image := &imagev1alpha1.SwiftImage{Status: imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}}}

	rg := Merge(guest, guestClass, image, nil)
	if rg.GuestSettings.Architecture != DefaultArchitecture {
		t.Errorf("Architecture = %q, want %q", rg.GuestSettings.Architecture, DefaultArchitecture)
	}
}

func TestMerge_ClassRootDiskSizeAndFormatApplied(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}, Spec: swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}}}
	guestClass := &swiftv1alpha1.SwiftGuestClass{Spec: swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("30Gi"), Format: swiftv1alpha1.DiskFormatQcow2}}}
	image := &imagev1alpha1.SwiftImage{Status: imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatQcow2}}}

	rg := Merge(guest, guestClass, image, nil)
	if rg.RootDisk.Format != "qcow2" {
		t.Errorf("RootDisk.Format = %q, want qcow2", rg.RootDisk.Format)
	}
}

func TestMerge_SeedFromSwiftSeedProfileWhenReferenced(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}, Spec: swiftv1alpha1.SwiftGuestSpec{SeedProfileRef: &corev1.LocalObjectReference{Name: "sp"}, ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}}}
	guestClass := &swiftv1alpha1.SwiftGuestClass{Spec: swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}}}
	image := &imagev1alpha1.SwiftImage{Status: imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}}}
	seedProfile := &seedv1alpha1.SwiftSeedProfile{Spec: seedv1alpha1.SwiftSeedProfileSpec{Datasource: seedv1alpha1.DatasourceNoCloud, UserData: "#cloud-config\nhostname: test", MetaData: "instance-id: 001"}}

	rg := Merge(guest, guestClass, image, seedProfile)
	if rg.Seed == nil {
		t.Fatal("Seed is nil")
	}
	if rg.Seed.UserData != "#cloud-config\nhostname: test" {
		t.Errorf("Seed.UserData = %q", rg.Seed.UserData)
	}
}

func TestMerge_NoSeedWhenSwiftSeedProfileNotReferenced(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}, Spec: swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}, GuestClassRef: corev1.LocalObjectReference{Name: "gc"}}}
	guestClass := &swiftv1alpha1.SwiftGuestClass{Spec: swiftv1alpha1.SwiftGuestClassSpec{CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"), RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw}}}
	image := &imagev1alpha1.SwiftImage{Status: imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}}}

	rg := Merge(guest, guestClass, image, nil)
	if rg.Seed != nil {
		t.Errorf("Seed = %v, want nil", rg.Seed)
	}
}

// --- NoCloud meta-data defaulting (#457) ---
//
// NoCloud is only recognised when the seed disk carries meta-data. Without it
// cloud-init falls back to DataSourceNone and silently discards user-data, while
// the guest still boots and reports Ready — so these cover the defaulting rather
// than leaving the failure to a cluster to discover.

func seedMergeFixture(t *testing.T, profile *seedv1alpha1.SwiftSeedProfile) *ResolvedGuest {
	t.Helper()
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "web-01", Namespace: "prod"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			SeedProfileRef: &corev1.LocalObjectReference{Name: "sp"},
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "gc"},
		},
	}
	guestClass := &swiftv1alpha1.SwiftGuestClass{Spec: swiftv1alpha1.SwiftGuestClassSpec{
		CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"),
		RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw},
	}}
	image := &imagev1alpha1.SwiftImage{Status: imagev1alpha1.SwiftImageStatus{
		Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw},
	}}
	return Merge(guest, guestClass, image, profile)
}

func TestMerge_NoCloudWithoutMetaDataGetsADefault(t *testing.T) {
	rg := seedMergeFixture(t, &seedv1alpha1.SwiftSeedProfile{Spec: seedv1alpha1.SwiftSeedProfileSpec{
		Datasource: seedv1alpha1.DatasourceNoCloud,
		UserData:   "#cloud-config\nusers: []",
	}})
	want := "instance-id: prod-web-01\nlocal-hostname: web-01\n"
	if rg.Seed.MetaData != want {
		t.Errorf("Seed.MetaData = %q, want %q — without it cloud-init ignores user-data entirely", rg.Seed.MetaData, want)
	}
}

func TestMerge_ExplicitMetaDataIsNotOverridden(t *testing.T) {
	rg := seedMergeFixture(t, &seedv1alpha1.SwiftSeedProfile{Spec: seedv1alpha1.SwiftSeedProfileSpec{
		Datasource: seedv1alpha1.DatasourceNoCloud,
		UserData:   "#cloud-config",
		MetaData:   "instance-id: operator-chose-this\n",
	}})
	if rg.Seed.MetaData != "instance-id: operator-chose-this\n" {
		t.Errorf("Seed.MetaData = %q; defaulting must fill a gap, never override", rg.Seed.MetaData)
	}
}

// metaDataFrom is resolved later by the renderer, so an empty MetaData string
// here does NOT mean the operator supplied nothing — defaulting would shadow the
// Secret/ConfigMap they pointed at.
func TestMerge_MetaDataFromSuppressesTheDefault(t *testing.T) {
	rg := seedMergeFixture(t, &seedv1alpha1.SwiftSeedProfile{Spec: seedv1alpha1.SwiftSeedProfileSpec{
		Datasource:   seedv1alpha1.DatasourceNoCloud,
		UserData:     "#cloud-config",
		MetaDataFrom: &seedv1alpha1.SeedDataValueFrom{},
	}})
	if rg.Seed.MetaData != "" {
		t.Errorf("Seed.MetaData = %q, want empty: metaDataFrom supplies it at render time", rg.Seed.MetaData)
	}
}

func TestMerge_NonNoCloudDatasourceIsUntouched(t *testing.T) {
	rg := seedMergeFixture(t, &seedv1alpha1.SwiftSeedProfile{Spec: seedv1alpha1.SwiftSeedProfileSpec{
		Datasource: "ConfigDrive",
		UserData:   "#cloud-config",
	}})
	if rg.Seed.MetaData != "" {
		t.Errorf("Seed.MetaData = %q, want empty: the meta-data requirement is NoCloud's", rg.Seed.MetaData)
	}
}
