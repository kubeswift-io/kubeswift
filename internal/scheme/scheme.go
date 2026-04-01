package scheme

import (
	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	kernelv1alpha1 "github.com/projectbeskar/kubeswift/api/kernel/v1alpha1"
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var Scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(Scheme))
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	Scheme.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{}, &swiftv1alpha1.SwiftGuestClass{}, &swiftv1alpha1.SwiftGuestClassList{})
	metav1.AddToGroupVersion(Scheme, gvSwift)
	gvImage := schema.GroupVersion{Group: "image.kubeswift.io", Version: "v1alpha1"}
	Scheme.AddKnownTypes(gvImage, &imagev1alpha1.SwiftImage{}, &imagev1alpha1.SwiftImageList{})
	metav1.AddToGroupVersion(Scheme, gvImage)
	gvSeed := schema.GroupVersion{Group: "seed.kubeswift.io", Version: "v1alpha1"}
	Scheme.AddKnownTypes(gvSeed, &seedv1alpha1.SwiftSeedProfile{}, &seedv1alpha1.SwiftSeedProfileList{})
	metav1.AddToGroupVersion(Scheme, gvSeed)
	gvKernel := schema.GroupVersion{Group: "kernel.kubeswift.io", Version: "v1alpha1"}
	Scheme.AddKnownTypes(gvKernel, &kernelv1alpha1.SwiftKernel{}, &kernelv1alpha1.SwiftKernelList{})
	metav1.AddToGroupVersion(Scheme, gvKernel)
	gvGPU := schema.GroupVersion{Group: "gpu.kubeswift.io", Version: "v1alpha1"}
	Scheme.AddKnownTypes(gvGPU, &gpuv1alpha1.SwiftGPUProfile{}, &gpuv1alpha1.SwiftGPUProfileList{}, &gpuv1alpha1.SwiftGPUNode{}, &gpuv1alpha1.SwiftGPUNodeList{})
	metav1.AddToGroupVersion(Scheme, gvGPU)
}
