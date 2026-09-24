package hostpath

import (
	"strings"
	"testing"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// The exploit: a vhost-user socket under an allowed prefix passes the
// host-path check, but its comma makes Cloud Hypervisor read "path=/dev/sda"
// as another --disk option and attach the node's disk to the guest.
func TestValidateCHOptionValues_RejectsOptionInjection(t *testing.T) {
	for name, spec := range map[string]swiftv1alpha1.SwiftGuestSpec{
		"vhost-user blk socket": {VhostUserDevices: []swiftv1alpha1.VhostUserDevice{{Name: "d", Type: "blk", Socket: "/srv/vm/x,path=/dev/sda"}}},
		"vhost-user net socket": {Interfaces: []swiftv1alpha1.GuestInterface{{Name: "n", Socket: "/srv/vm/n.sock,mac=00:00:00:00:00:01"}}},
		"virtiofs tag":          {Filesystems: []swiftv1alpha1.Filesystem{{Name: "fs", Tag: "t,socket=/run/other.sock"}}},
		"generic virtioId":      {VhostUserDevices: []swiftv1alpha1.VhostUserDevice{{Name: "g", Type: "generic", Socket: "/srv/vm/g", VirtioID: "fs,queue_sizes=[1]"}}},
		"space in socket":       {VhostUserDevices: []swiftv1alpha1.VhostUserDevice{{Name: "d", Type: "blk", Socket: "/srv/vm/a b"}}},
	} {
		if err := ValidateCHOptionValues(&spec); err == nil || !strings.Contains(err.Error(), "Cloud Hypervisor") {
			t.Errorf("%s: accepted (err=%v)", name, err)
		}
	}
}

func TestValidateCHOptionValues_AcceptsOrdinaryValues(t *testing.T) {
	spec := swiftv1alpha1.SwiftGuestSpec{
		Filesystems:      []swiftv1alpha1.Filesystem{{Name: "fs", Tag: "data_1.v2"}},
		Interfaces:       []swiftv1alpha1.GuestInterface{{Name: "n", Socket: "/var/run/vpp/sock-1.sock"}},
		VhostUserDevices: []swiftv1alpha1.VhostUserDevice{{Name: "g", Type: "generic", Socket: "/var/run/spdk/vhost.0", VirtioID: "block"}},
	}
	if err := ValidateCHOptionValues(&spec); err != nil {
		t.Errorf("ordinary values rejected: %v", err)
	}
}

// swiftletd reads queue sizes as unsigned: one below 1 made the whole runtime
// intent unreadable, and the launcher exited before it could report anything.
func TestValidateCHOptionValues_RejectsANonPositiveQueueSize(t *testing.T) {
	for _, q := range []int32{0, -1} {
		spec := swiftv1alpha1.SwiftGuestSpec{VhostUserDevices: []swiftv1alpha1.VhostUserDevice{{
			Name: "g", Type: "generic", Socket: "/var/run/spdk/vhost.0", VirtioID: "block", QueueSizes: []int32{256, q},
		}}}
		if err := ValidateCHOptionValues(&spec); err == nil || !strings.Contains(err.Error(), "queueSizes[1]") {
			t.Errorf("queue size %d: err=%v", q, err)
		}
	}
	ok := swiftv1alpha1.SwiftGuestSpec{VhostUserDevices: []swiftv1alpha1.VhostUserDevice{{
		Name: "g", Type: "generic", Socket: "/var/run/spdk/vhost.0", VirtioID: "block", QueueSizes: []int32{1, 256},
	}}}
	if err := ValidateCHOptionValues(&ok); err != nil {
		t.Errorf("valid queue sizes rejected: %v", err)
	}
}
