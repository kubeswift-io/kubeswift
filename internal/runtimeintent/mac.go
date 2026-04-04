package runtimeintent

import (
	"crypto/sha256"
	"fmt"
)

// GenerateMAC produces a deterministic locally-administered MAC address
// from a seed string. The same seed always produces the same MAC, ensuring
// stability across pod recreations (important for DHCP lease persistence).
// Uses the 52:54:00 OUI prefix (QEMU/KVM standard locally-administered range).
func GenerateMAC(seed string) string {
	h := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", h[0], h[1], h[2])
}

// InterfaceMACSeed returns the canonical seed string for MAC generation
// given a guest's namespace, name, and interface name.
func InterfaceMACSeed(namespace, name, ifaceName string) string {
	return namespace + "/" + name + "/" + ifaceName
}
