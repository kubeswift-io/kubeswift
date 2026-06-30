package swiftgpu

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func gpuNode(name string, vfioReady bool, free int, model string) *gpuv1alpha1.SwiftGPUNode {
	return &gpuv1alpha1.SwiftGPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: gpuv1alpha1.SwiftGPUNodeStatus{
			VfioReady: vfioReady,
			FreeGPUs:  free,
			GPUModel:  model,
		},
	}
}

func profile(count int, model, mode string) *gpuv1alpha1.SwiftGPUProfile {
	return &gpuv1alpha1.SwiftGPUProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       gpuv1alpha1.SwiftGPUProfileSpec{Count: count, Model: model, PartitionMode: mode},
	}
}

func TestGPUNodeHasCapacity(t *testing.T) {
	tests := []struct {
		name    string
		node    *gpuv1alpha1.SwiftGPUNode // nil => not present
		profile *gpuv1alpha1.SwiftGPUProfile
		wantErr bool
	}{
		{
			name:    "node missing",
			node:    nil,
			profile: profile(1, "", "isolated"),
			wantErr: true,
		},
		{
			name:    "not vfio-ready",
			node:    gpuNode("boba", false, 1, "GeForce GTX 1080"),
			profile: profile(1, "", "isolated"),
			wantErr: true,
		},
		{
			name:    "insufficient free GPUs",
			node:    gpuNode("boba", true, 0, "GeForce GTX 1080"),
			profile: profile(1, "", "isolated"),
			wantErr: true,
		},
		{
			name:    "model mismatch",
			node:    gpuNode("boba", true, 1, "GeForce GTX 1080"),
			profile: profile(1, "H200", "isolated"),
			wantErr: true,
		},
		{
			name:    "fits (empty model matches any)",
			node:    gpuNode("boba", true, 1, "GeForce GTX 1080"),
			profile: profile(1, "", "isolated"),
			wantErr: false,
		},
		{
			name:    "fits (model substring match)",
			node:    gpuNode("boba", true, 2, "NVIDIA Corporation GP104 [GeForce GTX 1080]"),
			profile: profile(2, "GTX 1080", "isolated"),
			wantErr: false,
		},
		{
			name:    "shared mode without FM partition",
			node:    gpuNode("boba", true, 4, "H200 SXM"),
			profile: profile(2, "", "shared"),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(scheme.Scheme)
			if tc.node != nil {
				b = b.WithObjects(tc.node)
			}
			c := b.Build()

			err := GPUNodeHasCapacity(context.Background(), c, "boba", tc.profile)
			if (err != nil) != tc.wantErr {
				t.Errorf("GPUNodeHasCapacity err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
