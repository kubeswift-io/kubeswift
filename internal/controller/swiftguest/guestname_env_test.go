package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// Every launcher names the guest it reports GuestRunning to. swiftletd used
// the pod name, which a migration's destination pod does not share with its
// guest (<guest>-mig-<uid>); it carries this variable from its source pod.
func TestLauncherPods_NameTheirGuest(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-a", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "seed"},
		},
	}
	disk := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Seed:          &resolved.Seed{Datasource: "NoCloud", UserData: "x", MetaData: "y"},
		Network:       true,
	}
	kernel := &resolved.ResolvedGuest{
		Resources:  resolved.Resources{CPU: 2, Memory: 2048},
		KernelBoot: &resolved.KernelBoot{LocalPath: "/var/lib/kubeswift/kernels/default-k", KernelCmdline: "console=ttyS0"},
		Network:    true,
	}
	for name, pod := range map[string]*corev1.Pod{
		"disk boot":   BuildPod(guest, disk, "seed", "intent", nil),
		"kernel boot": BuildPod(guest, kernel, "", "intent", nil),
		"gpu":         BuildGPUDiskBootPod(gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1), gpuResolvedGuest(), "seed", "intent", "1Gi", nil),
		"restore": BuildRestorePod(guest, disk, "", "intent", nil, RestoreParams{
			SnapshotPath: "/var/lib/kubeswift/snapshots/default_snap1", NodeName: "worker-1", Mode: RestoreModeInPlace,
		}),
	} {
		t.Run(name, func(t *testing.T) {
			var launcher *corev1.Container
			for i := range pod.Spec.Containers {
				if pod.Spec.Containers[i].Name == "launcher" {
					launcher = &pod.Spec.Containers[i]
				}
			}
			if launcher == nil {
				t.Fatal("no launcher container")
			}
			want := guest.Name
			if name == "gpu" {
				want = gpuGuest("gpu-node-1", nil, -1).Name
			}
			for _, e := range launcher.Env {
				if e.Name == EnvGuestName {
					if e.Value != want {
						t.Errorf("%s = %q, want %q", EnvGuestName, e.Value, want)
					}
					return
				}
			}
			t.Errorf("launcher has no %s", EnvGuestName)
		})
	}
}
