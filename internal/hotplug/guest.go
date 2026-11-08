package hotplug

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// GuestAttachment returns the name of a SwiftVolumeAttachment of the guest,
// deleting or not, or "" when it has none. The features that cannot carry a
// hot-plugged disk (live migration, memory snapshots) refuse a guest that has
// one: an attachment being deleted counts, since the guest may still hold the
// disk.
func GuestAttachment(ctx context.Context, c client.Reader, namespace, guest string) (string, error) {
	var svas swiftv1alpha1.SwiftVolumeAttachmentList
	if err := c.List(ctx, &svas, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list SwiftVolumeAttachments: %w", err)
	}
	for i := range svas.Items {
		if svas.Items[i].Spec.GuestRef.Name == guest {
			return svas.Items[i].Name, nil
		}
	}
	return "", nil
}

// MemorySnapshotRefusal is why a memory snapshot of a guest with a
// SwiftVolumeAttachment is refused: Cloud Hypervisor's snapshot lists the
// hot-plugged disk by its path in the launcher, which a restore or a clone
// does not have.
func MemorySnapshotRefusal(guest, attachment string) string {
	return fmt.Sprintf("SwiftGuest %s has a hot-plugged volume (SwiftVolumeAttachment %s); a snapshot that captures memory "+
		"cannot be taken while a volume is hot-plugged, because a restore would not have the disk. "+
		"Delete the attachment first, or use backend.type csi-volume-snapshot (disk only)", guest, attachment)
}
