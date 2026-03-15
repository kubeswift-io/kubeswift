package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
)

func TestBuildPod_HasInitContainerWhenHasSeed(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "minimal"},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Seed:          &resolved.Seed{Datasource: "NoCloud", UserData: "x", MetaData: "y"},
	}

	pod := BuildPod(guest, rg, "test-seed", "test-intent")

	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("initContainers = %d, want 1", len(pod.Spec.InitContainers))
	}
	ic := pod.Spec.InitContainers[0]
	if ic.Name != "network-init" {
		t.Errorf("init container name = %q, want network-init", ic.Name)
	}
	if len(ic.Command) != 1 || ic.Command[0] != "/usr/local/bin/network-init.sh" {
		t.Errorf("init container command = %v, want [network-init.sh]", ic.Command)
	}
}

func TestBuildPod_NoInitContainerWhenNoSeed(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "class"},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Seed:          nil,
	}

	pod := BuildPod(guest, rg, "", "test-intent")

	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("initContainers = %d, want 0 when no seed", len(pod.Spec.InitContainers))
	}
}
