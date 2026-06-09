package swiftguest

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// Validator validates SwiftGuest resources.
type Validator struct{}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	g, ok := obj.(*swiftv1alpha1.SwiftGuest)
	if !ok {
		return nil, fmt.Errorf("expected SwiftGuest, got %T", obj)
	}
	return nil, validateSwiftGuest(g)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	g, ok := newObj.(*swiftv1alpha1.SwiftGuest)
	if !ok {
		return nil, fmt.Errorf("expected SwiftGuest, got %T", newObj)
	}
	return nil, validateSwiftGuest(g)
}

func (v *Validator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateSwiftGuest(g *swiftv1alpha1.SwiftGuest) error {
	spec := &g.Spec
	hasImage := spec.ImageRef != nil && spec.ImageRef.Name != ""
	hasKernel := spec.KernelRef != nil && spec.KernelRef.Name != ""
	hasClone := spec.CloneFromSnapshot != nil

	// Exactly one boot source: imageRef, kernelRef, or cloneFromSnapshot.
	sources := 0
	for _, s := range []bool{hasImage, hasKernel, hasClone} {
		if s {
			sources++
		}
	}
	if sources != 1 {
		return fmt.Errorf("exactly one of spec.imageRef, spec.kernelRef, or spec.cloneFromSnapshot must be set")
	}

	if hasClone {
		if spec.CloneFromSnapshot.SnapshotRef.Name == "" {
			return fmt.Errorf("spec.cloneFromSnapshot.snapshotRef.name is required")
		}
		// VFIO/GPU state cannot be CH-restored (Phase 0 Constraint #1), the same
		// rule the includeMemory+VFIO snapshot path enforces.
		if spec.GPUProfileRef != nil {
			return fmt.Errorf("spec.cloneFromSnapshot is mutually exclusive with spec.gpuProfileRef (VFIO state cannot be restored)")
		}
	}

	// guestClassRef is required by the CRD schema (a non-pointer struct field),
	// so it is required for every boot source — including clones, which ignore
	// it for resources (the resumed VM's CPU/memory come from the snapshot) but
	// must still set it to satisfy admission. Keeping the webhook aligned with
	// the CRD avoids a confusing "webhook says optional, apiserver rejects" gap.
	if spec.GuestClassRef.Name == "" {
		return fmt.Errorf("spec.guestClassRef.name is required")
	}
	validPolicies := map[swiftv1alpha1.RunPolicy]bool{
		swiftv1alpha1.RunPolicyRunning:          true,
		swiftv1alpha1.RunPolicyStopped:          true,
		swiftv1alpha1.RunPolicyRestartOnFailure: true,
		swiftv1alpha1.RunPolicyAlways:           true,
	}
	if spec.RunPolicy != "" && !validPolicies[spec.RunPolicy] {
		return fmt.Errorf("spec.runPolicy must be Running, Stopped, RestartOnFailure, or Always, got %q", spec.RunPolicy)
	}

	// osType (Windows guest support). The enum is enforced by the CRD schema;
	// re-check defensively (the webhook may run against objects the schema
	// hasn't defaulted), then apply the v1 rules.
	switch spec.OSType {
	case "", swiftv1alpha1.OSTypeLinux, swiftv1alpha1.OSTypeWindows:
	default:
		return fmt.Errorf("spec.osType must be linux or windows, got %q", spec.OSType)
	}
	if spec.OSType == swiftv1alpha1.OSTypeWindows {
		// Windows is disk-boot only — there is no Windows bzImage for kernel boot.
		if hasKernel {
			return fmt.Errorf("spec.osType: windows requires disk boot (spec.imageRef); kernel boot (spec.kernelRef) is Linux-only")
		}
		// GPU passthrough to Windows is out of scope for v1 (a non-goal in
		// docs/design/windows-guest-support.md).
		if spec.GPUProfileRef != nil {
			return fmt.Errorf("spec.osType: windows with spec.gpuProfileRef is not supported in v1 (GPU passthrough to Windows is out of scope)")
		}
	}

	if err := validateFilesystems(spec); err != nil {
		return err
	}
	if err := validateInterfaces(spec); err != nil {
		return err
	}
	if err := validateVhostUserDevices(spec); err != nil {
		return err
	}
	return nil
}

// validateVhostUserDevices enforces the vhost-user device constraints: unique
// names, a socket per device, virtioId required for generic devices, and the
// v1 scope limit (Cloud Hypervisor only — a gpuProfileRef may select the QEMU
// runtime, which v1 does not wire for vhost-user).
func validateVhostUserDevices(spec *swiftv1alpha1.SwiftGuestSpec) error {
	if len(spec.VhostUserDevices) == 0 {
		return nil
	}
	if spec.GPUProfileRef != nil {
		return fmt.Errorf("spec.vhostUserDevices is not supported with spec.gpuProfileRef (Cloud Hypervisor only in v1)")
	}
	names := make(map[string]struct{}, len(spec.VhostUserDevices))
	for i := range spec.VhostUserDevices {
		d := &spec.VhostUserDevices[i]
		if d.Name == "" {
			return fmt.Errorf("spec.vhostUserDevices[%d].name is required", i)
		}
		if _, dup := names[d.Name]; dup {
			return fmt.Errorf("spec.vhostUserDevices[%d].name %q is duplicated", i, d.Name)
		}
		names[d.Name] = struct{}{}

		if d.Socket == "" {
			return fmt.Errorf("spec.vhostUserDevices[%d].socket is required", i)
		}
		switch d.Type {
		case swiftv1alpha1.VhostUserDeviceTypeBlk:
			// nothing extra
		case swiftv1alpha1.VhostUserDeviceTypeGeneric:
			if d.VirtioID == "" {
				return fmt.Errorf("spec.vhostUserDevices[%d] (type generic) requires virtioId", i)
			}
		default:
			return fmt.Errorf("spec.vhostUserDevices[%d].type must be blk or generic, got %q", i, d.Type)
		}
	}
	return nil
}

// validateInterfaces enforces the vhost-user-net constraints: a backend socket
// is required, the bridge/sriov-only fields are not set, and the v1 scope limit
// (Cloud Hypervisor only — a gpuProfileRef may select the QEMU runtime, which
// v1 does not wire for vhost-user). bridge/sriov interfaces are unchanged.
func validateInterfaces(spec *swiftv1alpha1.SwiftGuestSpec) error {
	hasVhostUser := false
	for i := range spec.Interfaces {
		iface := &spec.Interfaces[i]
		if iface.Type != swiftv1alpha1.InterfaceTypeVhostUser {
			continue
		}
		hasVhostUser = true
		if iface.Socket == "" {
			return fmt.Errorf("spec.interfaces[%d] (type vhost-user) requires a socket path", i)
		}
		if iface.NetworkRef != nil {
			return fmt.Errorf("spec.interfaces[%d]: vhost-user does not use networkRef", i)
		}
		if iface.ResourceName != "" {
			return fmt.Errorf("spec.interfaces[%d]: vhost-user does not use resourceName", i)
		}
	}
	if hasVhostUser && spec.GPUProfileRef != nil {
		return fmt.Errorf("spec.interfaces: vhost-user is not supported with spec.gpuProfileRef (Cloud Hypervisor only in v1)")
	}

	// At most one interface may be primary, and only a bridge-type interface
	// (the default) can be primary — SR-IOV and vhost-user are never the
	// guest's DHCP/management NIC.
	primaries := 0
	for i := range spec.Interfaces {
		iface := &spec.Interfaces[i]
		if !iface.Primary {
			continue
		}
		primaries++
		if iface.Type == swiftv1alpha1.InterfaceTypeSRIOV || iface.Type == swiftv1alpha1.InterfaceTypeVhostUser {
			return fmt.Errorf("spec.interfaces[%d]: primary=true is only valid on a bridge interface, not %q", i, iface.Type)
		}
	}
	if primaries > 1 {
		return fmt.Errorf("spec.interfaces: at most one interface may set primary=true (got %d)", primaries)
	}
	return nil
}

// validateFilesystems enforces the virtiofs (vhost-user-fs) constraints:
// unique name + tag per guest, exactly one source, and the v1 scope limits
// (Cloud Hypervisor + Linux only — the QEMU path is a later phase and a
// Windows virtio-fs driver is out of scope).
func validateFilesystems(spec *swiftv1alpha1.SwiftGuestSpec) error {
	if len(spec.Filesystems) == 0 {
		return nil
	}
	// v1: virtiofs ships on the Cloud Hypervisor path only. A gpuProfileRef may
	// select the QEMU runtime (tier hgx-shared/hgx-full), which v1 does not wire
	// for vhost-user; reject the combination rather than silently dropping the
	// shares at runtime.
	if spec.GPUProfileRef != nil {
		return fmt.Errorf("spec.filesystems is not supported with spec.gpuProfileRef (virtiofs is Cloud Hypervisor only in v1)")
	}
	if spec.OSType == swiftv1alpha1.OSTypeWindows {
		return fmt.Errorf("spec.filesystems is not supported with spec.osType: windows (no virtio-fs guest driver in v1)")
	}
	names := make(map[string]struct{}, len(spec.Filesystems))
	tags := make(map[string]struct{}, len(spec.Filesystems))
	for i := range spec.Filesystems {
		fs := &spec.Filesystems[i]
		if fs.Name == "" {
			return fmt.Errorf("spec.filesystems[%d].name is required", i)
		}
		if _, dup := names[fs.Name]; dup {
			return fmt.Errorf("spec.filesystems[%d].name %q is duplicated", i, fs.Name)
		}
		names[fs.Name] = struct{}{}

		tag := fs.Tag
		if tag == "" {
			tag = fs.Name
		}
		if _, dup := tags[tag]; dup {
			return fmt.Errorf("spec.filesystems[%d] tag %q is duplicated (tag defaults to name when unset)", i, tag)
		}
		tags[tag] = struct{}{}

		hasHostPath := fs.Source.HostPath != nil
		hasPVC := fs.Source.PVCRef != nil && fs.Source.PVCRef.Name != ""
		switch {
		case hasHostPath && hasPVC:
			return fmt.Errorf("spec.filesystems[%d].source: set exactly one of hostPath or pvcRef, not both", i)
		case !hasHostPath && !hasPVC:
			return fmt.Errorf("spec.filesystems[%d].source: exactly one of hostPath or pvcRef is required", i)
		}
		if hasHostPath && *fs.Source.HostPath == "" {
			return fmt.Errorf("spec.filesystems[%d].source.hostPath must not be empty", i)
		}
	}
	return nil
}
