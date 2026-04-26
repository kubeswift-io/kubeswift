package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
)

func minimalGuest() *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "g1",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationActiveRestore:       "r1",
				AnnotationRestoreSnapshotPath: "/var/lib/kubeswift/snapshots/default-snap1",
				AnnotationRestoreNodeName:     "boba",
			},
		},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			RunPolicy: swiftv1alpha1.RunPolicyRunning,
		},
	}
}

func minimalResolved() *resolved.ResolvedGuest {
	return &resolved.ResolvedGuest{
		Resources: resolved.Resources{CPU: 2, Memory: 2048},
		Network:   true,
	}
}

// findVolume returns the named volume or nil.
func findVolume(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// findInit returns the named init container or nil.
func findInit(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

// findMount returns the named mount on the named container or nil.
func findMount(c *corev1.Container, name string) *corev1.VolumeMount {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == name {
			return &c.VolumeMounts[i]
		}
	}
	return nil
}

func TestRestoreParamsFromAnnotations_AbsentMeansNotRestoreMode(t *testing.T) {
	if _, ok := RestoreParamsFromAnnotations(nil); ok {
		t.Errorf("nil annotations: ok=true, want false")
	}
	if _, ok := RestoreParamsFromAnnotations(map[string]string{"foo": "bar"}); ok {
		t.Errorf("unrelated annotations: ok=true, want false")
	}
}

func TestRestoreParamsFromAnnotations_DefaultsModeToInPlace(t *testing.T) {
	annos := map[string]string{
		AnnotationActiveRestore:       "r1",
		AnnotationRestoreSnapshotPath: "/var/lib/kubeswift/snapshots/x",
		AnnotationRestoreNodeName:     "n1",
	}
	p, ok := RestoreParamsFromAnnotations(annos)
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if p.Mode != RestoreModeInPlace {
		t.Errorf("Mode = %q, want %q (default)", p.Mode, RestoreModeInPlace)
	}
	if p.IsClone() {
		t.Errorf("IsClone() = true, want false")
	}
}

func TestRestoreParamsFromAnnotations_PicksUpAllFields(t *testing.T) {
	annos := map[string]string{
		AnnotationActiveRestore:              "r1",
		AnnotationRestoreSnapshotPath:        "/var/lib/kubeswift/snapshots/x",
		AnnotationRestoreNodeName:            "n1",
		AnnotationRestoreMode:                RestoreModeClone,
		AnnotationRestoreMACRewrites:         "52:54:00:aa:bb:01",
		AnnotationRestoreAppendCmdlineMarker: "true",
	}
	p, ok := RestoreParamsFromAnnotations(annos)
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if p.SnapshotPath != "/var/lib/kubeswift/snapshots/x" {
		t.Errorf("SnapshotPath = %q", p.SnapshotPath)
	}
	if p.NodeName != "n1" {
		t.Errorf("NodeName = %q", p.NodeName)
	}
	if !p.IsClone() {
		t.Errorf("IsClone() = false, want true")
	}
	if p.MACRewrites != "52:54:00:aa:bb:01" {
		t.Errorf("MACRewrites = %q", p.MACRewrites)
	}
	if !p.AppendCmdlineMarker {
		t.Errorf("AppendCmdlineMarker = false, want true")
	}
	if got := p.InPodSnapshotPath(); got != RestoreStagingPath {
		t.Errorf("InPodSnapshotPath() = %q, want %q", got, RestoreStagingPath)
	}
}

func TestRestoreParamsFromAnnotations_InPlaceUsesSourceMount(t *testing.T) {
	annos := map[string]string{
		AnnotationActiveRestore:       "r1",
		AnnotationRestoreSnapshotPath: "/var/lib/kubeswift/snapshots/x",
		AnnotationRestoreNodeName:     "n1",
		AnnotationRestoreMode:         RestoreModeInPlace,
	}
	p, ok := RestoreParamsFromAnnotations(annos)
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if got := p.InPodSnapshotPath(); got != RestoreSourcePath {
		t.Errorf("InPodSnapshotPath() = %q, want %q (in-place reads RO source)", got, RestoreSourcePath)
	}
}

func TestBuildRestorePod_InPlaceFastPath_NoStagerInitContainer(t *testing.T) {
	guest := minimalGuest()
	rg := minimalResolved()
	params := RestoreParams{
		SnapshotPath: "/var/lib/kubeswift/snapshots/default-snap1",
		NodeName:     "boba",
		Mode:         RestoreModeInPlace,
	}

	pod := BuildRestorePod(guest, rg, "g1-runtime-intent", nil, params)

	if pod.Name != "g1" {
		t.Errorf("pod.Name = %q, want g1", pod.Name)
	}
	if pod.Spec.NodeSelector["kubernetes.io/hostname"] != "boba" {
		t.Errorf("nodeSelector hostname = %q, want boba", pod.Spec.NodeSelector["kubernetes.io/hostname"])
	}
	if findInit(pod, SnapshotStagerInitContainerName) != nil {
		t.Errorf("in-place must NOT include a snapshot-stager init container")
	}
	if findVolume(pod, snapshotStagingVolume) != nil {
		t.Errorf("in-place must NOT have the staging emptyDir volume")
	}
	src := findVolume(pod, snapshotSourceVolume)
	if src == nil {
		t.Fatal("snapshot-source volume missing")
	}
	if src.HostPath == nil {
		t.Fatal("snapshot-source must be hostPath")
	}
	if src.HostPath.Path != "/var/lib/kubeswift/snapshots/default-snap1" {
		t.Errorf("snapshot-source path = %q", src.HostPath.Path)
	}
	// Launcher reads from RO source mount.
	launcher := pod.Spec.Containers[0]
	mount := findMount(&launcher, snapshotSourceVolume)
	if mount == nil {
		t.Fatal("launcher missing snapshot-source mount")
	}
	if !mount.ReadOnly {
		t.Errorf("snapshot-source must be mounted ReadOnly in launcher (in-place)")
	}
	if mount.MountPath != RestoreSourcePath {
		t.Errorf("snapshot-source mountPath = %q, want %q", mount.MountPath, RestoreSourcePath)
	}
}

func TestBuildRestorePod_CloneAddsStagerAndStagingVolume(t *testing.T) {
	guest := minimalGuest()
	rg := minimalResolved()
	params := RestoreParams{
		SnapshotPath:        "/var/lib/kubeswift/snapshots/default-snap1",
		NodeName:            "boba",
		Mode:                RestoreModeClone,
		MACRewrites:         "52:54:00:aa:bb:01",
		AppendCmdlineMarker: true,
	}
	clone := &RootDiskCloneResult{PVCName: "swiftguest-root-clone1"}

	pod := BuildRestorePod(guest, rg, "g1-runtime-intent", clone, params)

	staging := findVolume(pod, snapshotStagingVolume)
	if staging == nil {
		t.Fatal("clone must have staging emptyDir")
	}
	if staging.EmptyDir == nil {
		t.Fatal("staging volume must be emptyDir")
	}
	stager := findInit(pod, SnapshotStagerInitContainerName)
	if stager == nil {
		t.Fatal("clone must include snapshot-stager init container")
	}
	if len(stager.Command) == 0 || stager.Command[0] != "/usr/local/bin/snapshot-stager" {
		t.Errorf("stager command = %v, want /usr/local/bin/snapshot-stager", stager.Command)
	}
	// Stager must mount source RO + staging RW.
	srcMount := findMount(stager, snapshotSourceVolume)
	if srcMount == nil || !srcMount.ReadOnly || srcMount.MountPath != RestoreSourcePath {
		t.Errorf("stager source mount wrong: %+v", srcMount)
	}
	stagingMount := findMount(stager, snapshotStagingVolume)
	if stagingMount == nil || stagingMount.ReadOnly || stagingMount.MountPath != RestoreStagingPath {
		t.Errorf("stager staging mount wrong: %+v", stagingMount)
	}
	// Args carry the cmdline marker + MAC rewrite list.
	gotMarker := false
	gotMACs := false
	for i, a := range stager.Args {
		if a == "--append-cmdline-marker=true" {
			gotMarker = true
		}
		if a == "--rewrite-macs" && i+1 < len(stager.Args) && stager.Args[i+1] == "52:54:00:aa:bb:01" {
			gotMACs = true
		}
	}
	if !gotMarker {
		t.Errorf("stager args missing --append-cmdline-marker=true: %v", stager.Args)
	}
	if !gotMACs {
		t.Errorf("stager args missing --rewrite-macs CSV: %v", stager.Args)
	}
	// Launcher reads from staging (NOT source).
	launcher := pod.Spec.Containers[0]
	if findMount(&launcher, snapshotSourceVolume) != nil {
		t.Errorf("launcher should NOT mount the read-only source in clone mode (only the staged copy)")
	}
	stagingLauncherMount := findMount(&launcher, snapshotStagingVolume)
	if stagingLauncherMount == nil || stagingLauncherMount.MountPath != RestoreStagingPath {
		t.Errorf("launcher missing staging mount or wrong path: %+v", stagingLauncherMount)
	}
	// Root-disk PVC is the clone's PVC (NOT the source's).
	rd := findVolume(pod, "root-disk")
	if rd == nil || rd.PersistentVolumeClaim == nil {
		t.Fatal("root-disk volume missing or wrong type")
	}
	if rd.PersistentVolumeClaim.ClaimName != "swiftguest-root-clone1" {
		t.Errorf("root-disk PVC = %q, want swiftguest-root-clone1", rd.PersistentVolumeClaim.ClaimName)
	}
}

func TestBuildRestorePod_RestartPolicyNeverAndNoSeedVolume(t *testing.T) {
	guest := minimalGuest()
	rg := minimalResolved()
	params := RestoreParams{Mode: RestoreModeInPlace, NodeName: "n1"}

	pod := BuildRestorePod(guest, rg, "intent-cm", nil, params)

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %s, want Never", pod.Spec.RestartPolicy)
	}
	// Seed ConfigMap must NOT be mounted — the source VM's cloud-init
	// state is baked into the snapshot's memory.
	if findVolume(pod, "seed") != nil {
		t.Errorf("restore pod must not have seed volume — cloud-init state is in the snapshot")
	}
}

func TestBuildRestorePod_NetworkInitOnlyWhenHasNetwork(t *testing.T) {
	guest := minimalGuest()
	rg := minimalResolved()
	rg.Network = false

	params := RestoreParams{Mode: RestoreModeInPlace, NodeName: "n1"}
	pod := BuildRestorePod(guest, rg, "intent-cm", nil, params)
	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == "network-init" {
			t.Errorf("network-init must not run when guest has no network")
		}
	}

	rg.Network = true
	pod = BuildRestorePod(guest, rg, "intent-cm", nil, params)
	found := false
	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == "network-init" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("network-init must run when guest has network")
	}
}

func TestBuildRestorePod_StagerHasNoPrivilege(t *testing.T) {
	guest := minimalGuest()
	rg := minimalResolved()
	params := RestoreParams{Mode: RestoreModeClone, NodeName: "n1"}

	pod := BuildRestorePod(guest, rg, "intent-cm", &RootDiskCloneResult{PVCName: "p"}, params)
	stager := findInit(pod, SnapshotStagerInitContainerName)
	if stager == nil {
		t.Fatal("stager missing")
	}
	sc := stager.SecurityContext
	if sc == nil {
		t.Fatal("stager has no SecurityContext")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("stager AllowPrivilegeEscalation = %v, want false", sc.AllowPrivilegeEscalation)
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 {
		t.Errorf("stager must drop ALL capabilities; got %+v", sc.Capabilities)
	}
}
