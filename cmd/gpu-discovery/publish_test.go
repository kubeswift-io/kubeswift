package main

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// The controller allocates a GPU between discovery's read and its status
// patch. status.gpus is an atomic list, so an unlocked patch built from the
// stale read put the GPU back to free, and it was handed out twice. The
// allocation must survive the discovery write.
func TestPublishStatus_KeepsAnAllocationMadeMidCycle(t *testing.T) {
	ctx := context.Background()
	node := &gpuv1alpha1.SwiftGPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Status: gpuv1alpha1.SwiftGPUNodeStatus{
			GPUs: []gpuv1alpha1.GPUDevice{{Index: 0, PCIAddress: "0000:01:00.0", Driver: "nvidia"}},
		},
	}
	raced := false
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(node).
		WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if !raced {
					// The controller wins the race: it allocates the GPU now.
					raced = true
					var cur gpuv1alpha1.SwiftGPUNode
					if err := cl.Get(ctx, client.ObjectKey{Name: "worker-1"}, &cur); err != nil {
						return err
					}
					cur.Status.GPUs[0].Allocated = true
					cur.Status.GPUs[0].AllocatedTo = "default/vm-1"
					if err := cl.Status().Update(ctx, &cur); err != nil {
						return err
					}
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	// The GPU was rebound to vfio-pci since the last cycle, so this write
	// changes the gpus list -- and resends all of it.
	discovered := &SwiftGPUNodeStatus{
		GPUs:      []gpuv1alpha1.GPUDevice{{Index: 0, PCIAddress: "0000:01:00.0", Driver: "vfio-pci"}},
		VfioReady: true,
	}
	if _, ok := publishStatus(ctx, c, "worker-1", discovered); !ok {
		t.Fatal("publishStatus failed")
	}
	var got gpuv1alpha1.SwiftGPUNode
	if err := c.Get(ctx, client.ObjectKey{Name: "worker-1"}, &got); err != nil {
		t.Fatal(err)
	}
	if g := got.Status.GPUs[0]; !g.Allocated || g.AllocatedTo != "default/vm-1" {
		t.Errorf("discovery erased the controller's allocation: %+v", g)
	}
}
