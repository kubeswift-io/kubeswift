package resolved

import (
	"context"
	"fmt"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	kernelv1alpha1 "github.com/projectbeskar/kubeswift/api/kernel/v1alpha1"
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
	corev1 "k8s.io/api/core/v1"
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

	// In-guest identity agent (vsock): SOURCE guests only. Clones never reach
	// this resolver (the early return above), so they keep GuestAgentEnabled=false
	// and reopen the captured vsock device from config.json instead of adding a
	// second --vsock. Linux only — the v1 agent is a Linux binary; a Windows
	// guest gets no device (the GuestAgentUnreachable fallback would otherwise
	// fire pointlessly). See docs/design/clone-identity-vsock-agent.md.
	rg.GuestAgentEnabled = guest.Spec.GuestAgent != nil && guest.Spec.GuestAgent.Enabled && !rg.IsWindows()

	// Model A: a guest in a namespace carrying the OVN-Kubernetes primary-UDN
	// label rides the namespace primary UDN (ovn-udn1) on its node-local primary
	// NIC. The eligibility gate (primary is node-local, not on a NAD) is computed
	// HERE — not in GetNICs — because a default guest has no spec.interfaces (so
	// GetNICs returns nil and emits no per-NIC field). PrimaryInterface()==nil for
	// a default guest means a node-local primary -> Model A applies; a primary that
	// rides a NAD (NetworkRef != nil) is Model B (primary-on-NAD) and is skipped.
	// The signal is carried at the intent top level (RuntimeIntent.PrimaryUDNInterface).
	// No-op (one cached Namespace Get) for the vast majority of clusters that have
	// no primary-UDN namespaces.
	hasPrimaryUDN, err := NamespaceHasPrimaryUDN(ctx, r.client, guest.Namespace)
	if err != nil {
		return nil, &ResolutionError{Reason: "checking primary-UDN namespace: " + err.Error()}
	}
	if hasPrimaryUDN {
		if p := guest.PrimaryInterface(); p == nil || p.NetworkRef == nil {
			rg.PrimaryUDNInterface = OVNPrimaryUDNInterface
		}
	}

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

// resolveDataDisks builds the ordered secondary VM-disk list, the single
// source of truth for both the pod builder and the runtime intent (device-
// letter order = this order). The legacy singular spec.dataDiskRef is first
// (image-backed, Filesystem, at the historical /disks/data/image.raw path);
// the plural spec.dataDiskRefs[] follow in declaration order.
//
// A plural entry becomes a VM disk when it is image-backed, blank, or a
// pvcRef WITH attachAsDisk. A plain pvcRef (no attachAsDisk) is the
// SwiftGuestPool per-replica filesystem mount — NOT a VM disk — and is
// handled separately by the pod builder (applyDataDiskRefs); it is
// intentionally skipped here.
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

	for i := range guest.Spec.DataDiskRefs {
		d := &guest.Spec.DataDiskRefs[i]
		switch {
		case d.Blank != nil:
			block := d.Blank.VolumeMode != corev1.PersistentVolumeFilesystem
			rd := ResolvedDataDisk{
				Name:    d.Name,
				PVCName: BlankDataDiskPVCName(guest.Name, d.Name),
				Block:   block,
				Format:  "raw",
				Ready:   true, // PVC binding is gated by EnsureBlankDataDisks.
			}
			if block {
				rd.HostPath = runtimeintent.DataDiskDevicePath(d.Name)
			} else {
				rd.MountPath = runtimeintent.DataDiskDir(d.Name)
				rd.HostPath = rd.MountPath + "/" + runtimeintent.DataDiskImageFile
			}
			out = append(out, rd)

		case d.ImageRef != nil:
			pi, err := r.resolveDataDiskImage(ctx, guest.Namespace, d.ImageRef.Name)
			if err != nil {
				return nil, err
			}
			out = append(out, ResolvedDataDisk{
				Name:      d.Name,
				PVCName:   pi.PVCName,
				Block:     false,
				HostPath:  runtimeintent.DataDiskDir(d.Name) + "/" + runtimeintent.DataDiskImageFile,
				MountPath: runtimeintent.DataDiskDir(d.Name),
				Format:    "raw",
				Ready:     pi.Ready,
			})

		case d.PVCRef != nil && d.AttachAsDisk:
			// Attach an operator-supplied PVC as a raw VM disk. Block PVCs
			// pass through as a device; a Filesystem PVC has no image.raw
			// convention to attach, so reject it honestly (Principle #6)
			// rather than silently mounting it as a directory.
			block, err := r.pvcIsBlock(ctx, guest.Namespace, d.PVCRef.Name)
			if err != nil {
				return nil, err
			}
			if !block {
				return nil, &ResolutionError{
					Reason:           "dataDiskRefs[" + d.Name + "]: attachAsDisk requires a Block-mode PVC (a Filesystem PVC has no raw device to attach)",
					AffectedResource: d.PVCRef.Name,
				}
			}
			out = append(out, ResolvedDataDisk{
				Name:     d.Name,
				PVCName:  d.PVCRef.Name,
				Block:    true,
				HostPath: runtimeintent.DataDiskDevicePath(d.Name),
				Format:   "raw",
				Ready:    true,
			})

			// case d.PVCRef != nil (no attachAsDisk): a filesystem-mounted pool
			// PVC, not a VM disk — handled by pod.go::applyDataDiskRefs.
		}
	}

	return out, nil
}

// BlankDataDiskPVCName is the deterministic name of the guest-owned PVC
// backing a blank data disk. Shared by the resolver (which records it on the
// ResolvedDataDisk) and the SwiftGuest controller (which creates it).
func BlankDataDiskPVCName(guestName, diskName string) string {
	return guestName + "-data-" + diskName
}

// pvcIsBlock reports whether the named PVC has VolumeMode=Block. A missing
// volumeMode (the Kubernetes default) is Filesystem.
func (r *resolver) pvcIsBlock(ctx context.Context, namespace, name string) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pvc); err != nil {
		return false, &ResolutionError{Reason: "dataDiskRefs attachAsDisk PVC not found: " + err.Error(), AffectedResource: name}
	}
	return pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock, nil
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
