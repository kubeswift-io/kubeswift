package runtimeintent

import (
	"crypto/sha256"
	"encoding/binary"
)

// DeriveVsockCID returns a deterministic vsock context id for a guest, derived
// from (namespace, name) the same way MACs are (see InterfaceMACSeed). The same
// guest always gets the same CID — important because the CID is captured into a
// memory snapshot and inherited (NOT re-assigned) on a cloneFromSnapshot restore.
//
// The result is in [3, 2^30+2]: CIDs 0 (VMADDR_CID_ANY), 1 (local), and 2
// (VMADDR_CID_HOST) are reserved, and we stay well below VMADDR_CID_ANY
// (0xFFFFFFFF). Uniqueness is only needed per host (one CH per launcher pod, in
// its own netns/PID namespace), so a hash collision across pods is harmless —
// vsock is pod-local.
func DeriveVsockCID(namespace, name string) uint32 {
	h := sha256.Sum256([]byte(namespace + "/" + name + "/vsock"))
	return binary.BigEndian.Uint32(h[:4])%(1<<30) + 3
}
