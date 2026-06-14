package resolved

import (
	"context"
	"fmt"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	kernelv1alpha1 "github.com/projectbeskar/kubeswift/api/kernel/v1alpha1"
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Resolver resolves SwiftGuest to ResolvedGuest by fetching and merging referenced resources.
type Resolver interface {
	Resolve(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*ResolvedGuest, error)
}

// NewResolver returns a Resolver that uses the given client.
func NewResolver(c client.Client) Resolver {
	return &resolver{client: c}
}

type resolver struct {
	client client.Client
}

// Resolve fetches referenced resources, validates, merges, and returns ResolvedGuest or ResolutionError.
func (r *resolver) Resolve(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*ResolvedGuest, error) {
	// cloneFromSnapshot is a distinct boot source resolved by the SwiftGuest
	// controller's clone path (Snapshot Phase 4 PR 3), not by the image/kernel
	// resolver. Until that path lands, surface an honest message rather than the
	// misleading "exactly one of imageRef or kernelRef" below.
	if guest.UsesCloneFromSnapshot() {
		return nil, &ResolutionError{Reason: "cloneFromSnapshot boot is not yet implemented (Snapshot Phase 4)"}
	}

	hasImage := guest.Spec.ImageRef != nil
	hasKernel := guest.Spec.KernelRef != nil

	if hasImage == hasKernel {
		return nil, &ResolutionError{Reason: "exactly one of imageRef or kernelRef must be set"}
	}

	// Fetch GuestClass (cluster-scoped)
	guestClass := &swiftv1alpha1.SwiftGuestClass{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: guest.Spec.GuestClassRef.Name}, guestClass); err != nil {
		return nil, &ResolutionError{Reason: "SwiftGuestClass not found: " + err.Error(), AffectedResource: guest.Spec.GuestClassRef.Name}
	}

	var rg *ResolvedGuest
	var err error
	if hasKernel {
		rg, err = r.resolveKernelBoot(ctx, guest, guestClass)
	} else {
		rg, err = r.resolveDiskBoot(ctx, guest, guestClass)
	}
	if err != nil {
		return nil, err
	}

	// Resolve secondary data disks (work with all boot paths), in declaration
	// order: the legacy singular spec.dataDiskRef first, then spec.dataDiskRefs[].
	dataDisks, err := r.resolveDataDisks(ctx, guest)
	if err != nil {
		return nil, err
	}
	rg.DataDisks = dataDisks

	return rg, nil
}

func (r *resolver) resolveKernelBoot(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, guestClass *swiftv1alpha1.SwiftGuestClass) (*ResolvedGuest, error) {
	sk := &kernelv1alpha1.SwiftKernel{}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: guest.Namespace, Name: guest.Spec.KernelRef.Name}, sk); err != nil {
		return nil, &ResolutionError{Reason: "SwiftKernel not found", AffectedResource: guest.Spec.KernelRef.Name}
	}
	if sk.Status.Phase != kernelv1alpha1.SwiftKernelPhaseReady {
		return nil, &ResolutionError{Reason: "SwiftKernel not Ready", AffectedResource: guest.Spec.KernelRef.Name}
	}

	// Fetch SeedProfile if referenced
	var seedProfile *seedv1alpha1.SwiftSeedProfile
	if guest.Spec.SeedProfileRef != nil {
		sp := &seedv1alpha1.SwiftSeedProfile{}
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: guest.Namespace, Name: guest.Spec.SeedProfileRef.Name}, sp); err != nil {
			return nil, &ResolutionError{Reason: "SwiftSeedProfile not found: " + err.Error(), AffectedResource: guest.Spec.SeedProfileRef.Name}
		}
		seedProfile = sp
	}

	rg := Merge(guest, guestClass, nil, seedProfile)

	cmdline := sk.Spec.KernelCmdline
	if guest.Spec.KernelCmdline != "" {
		cmdline = guest.Spec.KernelCmdline
	}
	rg.KernelBoot = &KernelBoot{
		LocalPath:     kernelv1alpha1.KernelLocalPath(sk.Namespace, sk.Name),
		KernelCmdline: cmdline,
	}

	return rg, nil
}

// resolveDataDisks builds the ordered secondary-disk list. The legacy singular
// spec.dataDiskRef is the first entry (image-backed, Filesystem, at the
// historical /disks/data/image.raw path). The plural spec.dataDiskRefs[]
// (image-backed / blank / attached-PVC) are appended after it.
func (r *resolver) resolveDataDisks(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) ([]ResolvedDataDisk, error) {
	var out []ResolvedDataDisk

	if guest.Spec.DataDiskRef != nil {
		pi, err := r.resolveDataDiskImage(ctx, guest.Namespace, guest.Spec.DataDiskRef.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, ResolvedDataDisk{
			Name:      "data",
			PVCName:   pi.PVCName,
			Block:     false,
			HostPath:  runtimeintent.DisksDataPath + "/" + runtimeintent.DataDiskImageFile,
			MountPath: runtimeintent.DisksDataPath,
			Format:    "raw",
			Ready:     pi.Ready,
		})
	}

	return out, nil
}

// resolveDataDiskImage resolves an image-backed data disk's SwiftImage to its
// prepared (Ready) PVC.
func (r *resolver) resolveDataDiskImage(ctx context.Context, namespace, name string) (*PreparedImage, error) {
	image := &imagev1alpha1.SwiftImage{}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, image); err != nil {
		return nil, &ResolutionError{Reason: "dataDiskRef SwiftImage not found: " + err.Error(), AffectedResource: name}
	}
	if image.Status.Phase != imagev1alpha1.SwiftImagePhaseReady {
		return nil, &ResolutionError{Reason: "dataDiskRef SwiftImage not Ready", AffectedResource: name}
	}
	pi := mergePreparedImage(image)
	return &pi, nil
}

func (r *resolver) resolveDiskBoot(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, guestClass *swiftv1alpha1.SwiftGuestClass) (*ResolvedGuest, error) {
	// Fetch Image (namespaced)
	image := &imagev1alpha1.SwiftImage{}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: guest.Namespace, Name: guest.Spec.ImageRef.Name}, image); err != nil {
		return nil, &ResolutionError{Reason: "SwiftImage not found: " + err.Error(), AffectedResource: guest.Spec.ImageRef.Name}
	}

	// osType cross-check: the image is authoritative (it defines the OS); the
	// guest's spec.osType, when set, must agree. Legacy guests/images leave
	// osType unset and skip the check (and inherit "linux" via Merge). A
	// mismatch (e.g. a linux guest pointed at a windows image) surfaces as a
	// Resolved=False condition rather than a confusing boot failure later.
	if guest.Spec.OSType != "" && image.Spec.OSType != "" &&
		string(guest.Spec.OSType) != string(image.Spec.OSType) {
		return nil, &ResolutionError{
			Reason: fmt.Sprintf("osType mismatch: guest declares %q but image %q is %q",
				guest.Spec.OSType, image.Name, image.Spec.OSType),
			AffectedResource: image.Name,
		}
	}

	// Fetch SeedProfile if referenced
	var seedProfile *seedv1alpha1.SwiftSeedProfile
	if guest.Spec.SeedProfileRef != nil {
		sp := &seedv1alpha1.SwiftSeedProfile{}
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: guest.Namespace, Name: guest.Spec.SeedProfileRef.Name}, sp); err != nil {
			return nil, &ResolutionError{Reason: "SwiftSeedProfile not found: " + err.Error(), AffectedResource: guest.Spec.SeedProfileRef.Name}
		}
		seedProfile = sp
	}

	// Validate existence
	if err := ValidateExistence(guest, guestClass, image, seedProfile); err != nil {
		return nil, err
	}

	// Merge
	rg := Merge(guest, guestClass, image, seedProfile)

	// Cross-object validation
	if err := ValidateCompatibility(rg); err != nil {
		return nil, err
	}

	return rg, nil
}
