package scheme

import (
	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
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
	gvImage := schema.GroupVersion{Group: "image.kubeswift.io", Version: "v1alpha1"}
	Scheme.AddKnownTypes(gvImage, &imagev1alpha1.SwiftImage{}, &imagev1alpha1.SwiftImageList{})
	gvSeed := schema.GroupVersion{Group: "seed.kubeswift.io", Version: "v1alpha1"}
	Scheme.AddKnownTypes(gvSeed, &seedv1alpha1.SwiftSeedProfile{}, &seedv1alpha1.SwiftSeedProfileList{})
}
