package hostpath

import (
	"fmt"
	"regexp"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// Cloud Hypervisor takes device options as one comma-separated key=value
// string (`--disk vhost_user=on,socket=<path>`, `--fs tag=<t>,socket=<s>`),
// and swiftletd writes spec values into those strings as they are. A value
// holding a comma therefore adds options of its own choosing: a vhost-user
// socket "/srv/vm/x,path=/dev/sda" under an allowed prefix passes the
// host-path check and attaches the node's disk to the guest. These are the
// characters such a value may use.
var (
	chPathValue  = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	chTokenValue = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// ValidateCHOptionValues rejects spec values that are written into Cloud
// Hypervisor option strings and hold characters that could split or extend
// them. Enforced by the SwiftGuest controller (the webhook is optional) and
// by the webhook for earlier feedback; swiftletd re-checks before launch.
func ValidateCHOptionValues(spec *swiftv1alpha1.SwiftGuestSpec) error {
	check := func(field, v string, re *regexp.Regexp) error {
		if v != "" && !re.MatchString(v) {
			return fmt.Errorf("%s %q contains characters Cloud Hypervisor would read as extra device options; allowed: %s", field, v, re.String())
		}
		return nil
	}
	for i, fs := range spec.Filesystems {
		if err := check(fmt.Sprintf("spec.filesystems[%d].tag", i), fs.Tag, chTokenValue); err != nil {
			return err
		}
	}
	for i, iface := range spec.Interfaces {
		if err := check(fmt.Sprintf("spec.interfaces[%d].socket", i), iface.Socket, chPathValue); err != nil {
			return err
		}
	}
	for i, d := range spec.VhostUserDevices {
		if err := check(fmt.Sprintf("spec.vhostUserDevices[%d].socket", i), d.Socket, chPathValue); err != nil {
			return err
		}
		if err := check(fmt.Sprintf("spec.vhostUserDevices[%d].virtioId", i), d.VirtioID, chTokenValue); err != nil {
			return err
		}
		// Also caught by the CRD schema; checked here for objects written
		// before it had the minimum. swiftletd reads the sizes as unsigned.
		for j, q := range d.QueueSizes {
			if q < 1 {
				return fmt.Errorf("spec.vhostUserDevices[%d].queueSizes[%d] = %d; a queue size must be at least 1", i, j, q)
			}
		}
	}
	return nil
}
