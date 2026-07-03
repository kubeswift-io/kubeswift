// Package configjson reads and minimally patches Cloud Hypervisor's
// snapshot config.json before a restore-receive launch.
//
// The snapshot directory layout is opaque per Phase 0 spike Constraint #4
// (config.json + state.json + memory-ranges; only config.json is
// safely modifiable, and only for two narrow operations: appending a
// cmdline marker for clone identity-regeneration, and rewriting MAC
// addresses on virtio-net devices for L2 collision avoidance).
//
// Everything else in config.json — disks, memory layout, CPU topology,
// device IDs — must be preserved byte-for-byte. We use generic
// json.RawMessage / map[string]any traversal so that fields we don't
// know about pass through untouched. CH's config.json shape evolves
// across versions; this code targets v51.x and treats unknown fields
// as opaque.
package configjson

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// CloneCmdlineMarker is appended to the kernel cmdline of cloned VMs
// so the in-guest cloud-init bootcmd module can detect "this boot
// resulted from a clone restore" and trigger machine-id, SSH host
// key, and hostname regeneration. The bootcmd gates on the marker
// AND on a sentinel file (/var/lib/kubeswift/.clone-regenerated) so
// repeated boots don't repeat the regeneration.
const CloneCmdlineMarker = "kubeswift.clone=true"

// ConfigJSONFilename is the canonical filename Cloud Hypervisor
// writes inside a snapshot directory. We open it directly rather than
// going through CH's API because in restore-receive mode the launcher
// hasn't started yet.
const ConfigJSONFilename = "config.json"

// PatchOptions controls what is rewritten in config.json. The zero
// value is a no-op — caller opts into each transformation explicitly.
//
// AppendCmdlineMarker appends [CloneCmdlineMarker] to the kernel
// cmdline if not already present. Idempotent: re-running on an
// already-marked config is a no-op so retried restores don't double-
// append.
//
// RewriteMACs is an index-keyed list of new MAC addresses for the
// virtio-net devices in config.net[]. Position N replaces the MAC
// of config.net[N]; an empty string at N leaves that device's MAC
// unchanged. Slice length need not equal len(config.net) — a shorter
// slice leaves trailing devices alone, a longer one is an error
// (caller has the device count wrong).
//
// The caller (SwiftRestore controller) computes new MACs via
// runtimeintent.GenerateMAC(InterfaceMACSeed(targetNs, targetName,
// ifaceName)). The patcher only writes them; the policy of "what is
// the right MAC?" lives one layer up.
//
// RewriteRuntimeDirPrefix performs a prefix substitution on every
// runtime_dir-shaped path in config.json (currently disks[].path and
// serial.socket). For clone restores the launcher pod's runtime_dir
// has a different name from the source's, so paths the source baked
// into config.json (e.g. /var/lib/kubeswift/run/<source-ns>-<source>/seed.iso)
// must be rewritten to point at the clone's runtime_dir. From and To
// are the source and target prefix strings; both must end in "/" or
// the patcher errors. Empty From or To skips this transformation.
//
// NullifyHostMAC sets net[N].host_mac to null on every virtio-net
// device. CH's open_tap (cloud-hypervisor v51.1 net_util/src/open_tap.rs)
// forces the tap MAC to the saved value when host_mac is Some, which
// would make the clone's freshly-created tap take the source's host
// MAC — colliding with the source if both pods run on the same node.
// Nulling host_mac lets CH auto-discover the clone tap's MAC instead.
type PatchOptions struct {
	AppendCmdlineMarker   bool
	RewriteMACs           []string
	RewriteRuntimeDirFrom string
	RewriteRuntimeDirTo   string
	NullifyHostMAC        bool
}

// Read returns the parsed config.json from the given snapshot
// directory. Returns an error if the file is missing or malformed.
func Read(snapshotDir string) (map[string]any, error) {
	path := configPath(snapshotDir)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// Write serializes cfg back to the config.json in the given snapshot
// directory. Uses 2-space indentation to match CH's own writer (so
// diffing a patched config against the original isolates real changes
// from format-noise).
func Write(snapshotDir string, cfg map[string]any) error {
	path := configPath(snapshotDir)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config.json: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Patch applies the requested transformations to cfg in place.
//
// Returns a list of human-readable change descriptions for logging /
// status surfacing — the controller writes these into a Warning-style
// condition so the operator can see what was modified.
func Patch(cfg map[string]any, opts PatchOptions) ([]string, error) {
	var changes []string
	if opts.AppendCmdlineMarker {
		c, err := appendCloneMarker(cfg)
		if err != nil {
			return nil, err
		}
		if c != "" {
			changes = append(changes, c)
		}
	}
	if len(opts.RewriteMACs) > 0 {
		cs, err := rewriteMACs(cfg, opts.RewriteMACs)
		if err != nil {
			return nil, err
		}
		changes = append(changes, cs...)
	}
	if opts.RewriteRuntimeDirFrom != "" || opts.RewriteRuntimeDirTo != "" {
		cs, err := rewriteRuntimeDirPrefix(cfg, opts.RewriteRuntimeDirFrom, opts.RewriteRuntimeDirTo)
		if err != nil {
			return nil, err
		}
		changes = append(changes, cs...)
	}
	if opts.NullifyHostMAC {
		cs, err := nullifyHostMAC(cfg)
		if err != nil {
			return nil, err
		}
		changes = append(changes, cs...)
	}
	return changes, nil
}

// unwrapConfig returns the inner config map — either cfg["config"]
// (when CH wraps the snapshot fields) or cfg itself (CH 51.1 writes
// the snapshot config flat). Callers traverse from the returned map.
//
// Both layouts have been observed in the wild: CH's HTTP API returns
// the wrapped form (vm.config response), whereas the snapshot file
// CH writes via vm.snapshot is flat. The patcher must work against
// the snapshot-file layout because it runs from the staging init
// container before any HTTP API is up.
func unwrapConfig(cfg map[string]any) map[string]any {
	if inner, ok := cfg["config"].(map[string]any); ok {
		return inner
	}
	return cfg
}

// appendCloneMarker adds CloneCmdlineMarker to the payload's cmdline
// if not already present. Idempotent.
//
// CH's config.json layout (v51) has the kernel cmdline at:
//
//	{
//	  "config": {
//	    "payload": { "cmdline": "console=ttyS0 ..." },
//	    ...
//	  }
//	}
//
// We traverse defensively — missing intermediate maps return an error
// rather than panic.
func appendCloneMarker(cfg map[string]any) (string, error) {
	payload, err := navigateToPayload(cfg)
	if err != nil {
		return "", err
	}
	cmdlineRaw, present := payload["cmdline"]
	// CH 51.1 disk-boot snapshots emit `cmdline: null` — the field is
	// present but the value is JSON null (the boot loader inside the
	// guest sets the kernel cmdline, so CH has nothing to record).
	// Treat that the same as a missing field: install the marker.
	if !present || cmdlineRaw == nil {
		payload["cmdline"] = CloneCmdlineMarker
		return "set cmdline to " + CloneCmdlineMarker + " (no prior cmdline)", nil
	}
	cmdline, ok := cmdlineRaw.(string)
	if !ok {
		return "", fmt.Errorf("config.payload.cmdline is %T, want string", cmdlineRaw)
	}
	// Idempotency check: don't double-append if the marker is already
	// there. We split on whitespace to avoid substring false-positives
	// (e.g. cmdline already contains "kubeswift.clone=true" verbatim).
	for _, tok := range strings.Fields(cmdline) {
		if tok == CloneCmdlineMarker {
			return "", nil
		}
	}
	payload["cmdline"] = strings.TrimRight(cmdline, " ") + " " + CloneCmdlineMarker
	return "appended " + CloneCmdlineMarker + " to kernel cmdline", nil
}

// navigateToPayload returns the payload object containing the kernel
// cmdline. Tolerates both layouts (wrapped under "config" or flat at
// the top level) by going through unwrapConfig.
func navigateToPayload(cfg map[string]any) (map[string]any, error) {
	configMap := unwrapConfig(cfg)
	payloadRaw, ok := configMap["payload"]
	if !ok {
		return nil, fmt.Errorf("config.json: 'payload' missing")
	}
	payload, ok := payloadRaw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config.json: payload is %T, want object", payloadRaw)
	}
	return payload, nil
}

// rewriteMACs replaces MACs on virtio-net devices in config.net[]
// by position. macsByIndex[N] is the new MAC for net[N]; an empty
// string at index N leaves that device's MAC unchanged. macsByIndex
// length must not exceed len(config.net) (catches caller bugs that
// pass too many MACs).
//
// CH's config layout (v51):
//
//	{ "config": { "net": [ {"id": "_net0", "tap": "tap0", "mac": "52:54:00:..."} ] } }
//
// `mac` is the only field this function touches. `tap`, `id`, queue
// counts, MTU, etc. pass through.
//
// We don't validate MAC format here — runtimeintent.GenerateMAC
// always produces valid AA:BB:CC:DD:EE:FF strings, and validating
// in two places risks divergence. Bad input from a future caller
// will surface as a CH startup error during restore-receive.
func rewriteMACs(cfg map[string]any, macsByIndex []string) ([]string, error) {
	configMap := unwrapConfig(cfg)
	netRaw, ok := configMap["net"]
	if !ok {
		// Source VM has no net devices — caller asked us to rewrite
		// MACs for a netless VM, which is meaningless.
		return nil, fmt.Errorf("config.json: 'config.net' missing — caller passed RewriteMACs for a VM with no NICs")
	}
	net, ok := netRaw.([]any)
	if !ok {
		return nil, fmt.Errorf("config.json: config.net is %T, want array", netRaw)
	}
	if len(macsByIndex) > len(net) {
		return nil, fmt.Errorf("config.json: RewriteMACs has %d entries but config.net has only %d devices",
			len(macsByIndex), len(net))
	}
	var changes []string
	for i, newMAC := range macsByIndex {
		if newMAC == "" {
			continue
		}
		device, ok := net[i].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("config.json: config.net[%d] is %T, want object", i, net[i])
		}
		oldMAC, _ := device["mac"].(string)
		if oldMAC == newMAC {
			// Idempotent: caller passed the same MAC we already have.
			continue
		}
		device["mac"] = newMAC
		ifaceID, _ := device["id"].(string)
		if ifaceID == "" {
			ifaceID = fmt.Sprintf("net[%d]", i)
		}
		changes = append(changes, fmt.Sprintf("rewrote MAC on %s: %s -> %s", ifaceID, oldMAC, newMAC))
	}
	return changes, nil
}

// rewriteRuntimeDirPrefix replaces every occurrence of `from` with `to`
// at the start of path-shaped fields in config.json that reference the
// source pod's runtime_dir. Currently scoped to:
//
//   - disks[].path  (e.g. .../run/<source>/seed.iso → .../run/<clone>/seed.iso)
//   - serial.socket (e.g. .../run/<source>/serial.sock → .../run/<clone>/serial.sock)
//   - vsock.socket  (e.g. .../run/<source>/vsock.sock → .../run/<clone>/vsock.sock) —
//     the in-guest identity agent's host-side socket. The CID is captured guest
//     state and is NOT rewritten (the guest kernel's vsock is bound to it); only
//     the host-side socket path moves to the clone's runtime dir.
//
// disks[].path that does NOT start with `from` is left alone — the
// root disk PVC ("/var/lib/kubeswift/disks/root/image.raw") is mounted
// at the same in-pod path on every launcher pod and shouldn't be
// rewritten.
//
// Both `from` and `to` must end in "/" so a substring match doesn't
// accidentally clip e.g. "<source>" out of a path that happens to
// contain the source name as part of a longer segment.
func rewriteRuntimeDirPrefix(cfg map[string]any, from, to string) ([]string, error) {
	if from == "" || to == "" {
		return nil, fmt.Errorf("config.json: rewriteRuntimeDirPrefix requires non-empty from and to")
	}
	if !strings.HasSuffix(from, "/") || !strings.HasSuffix(to, "/") {
		return nil, fmt.Errorf("config.json: rewriteRuntimeDirPrefix from/to must end in '/' (got %q -> %q)", from, to)
	}
	configMap := unwrapConfig(cfg)
	var changes []string

	// disks[].path
	if disksRaw, ok := configMap["disks"]; ok && disksRaw != nil {
		disks, ok := disksRaw.([]any)
		if !ok {
			return nil, fmt.Errorf("config.json: disks is %T, want array", disksRaw)
		}
		for i, d := range disks {
			disk, ok := d.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("config.json: disks[%d] is %T, want object", i, d)
			}
			pathRaw, ok := disk["path"]
			if !ok {
				continue
			}
			oldPath, ok := pathRaw.(string)
			if !ok {
				return nil, fmt.Errorf("config.json: disks[%d].path is %T, want string", i, pathRaw)
			}
			if !strings.HasPrefix(oldPath, from) {
				continue
			}
			newPath := to + strings.TrimPrefix(oldPath, from)
			disk["path"] = newPath
			diskID, _ := disk["id"].(string)
			if diskID == "" {
				diskID = fmt.Sprintf("disks[%d]", i)
			}
			changes = append(changes, fmt.Sprintf("rewrote %s.path: %s -> %s", diskID, oldPath, newPath))
		}
	}

	// serial.socket
	if serialRaw, ok := configMap["serial"]; ok && serialRaw != nil {
		serial, ok := serialRaw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("config.json: serial is %T, want object", serialRaw)
		}
		if sockRaw, ok := serial["socket"]; ok && sockRaw != nil {
			oldSock, ok := sockRaw.(string)
			if !ok {
				return nil, fmt.Errorf("config.json: serial.socket is %T, want string", sockRaw)
			}
			if strings.HasPrefix(oldSock, from) {
				newSock := to + strings.TrimPrefix(oldSock, from)
				serial["socket"] = newSock
				changes = append(changes, fmt.Sprintf("rewrote serial.socket: %s -> %s", oldSock, newSock))
			}
		}
	}

	// vsock.socket (in-guest identity agent host-side socket). The cid is
	// captured guest state and stays as-is; only the host socket path moves.
	if vsockRaw, ok := configMap["vsock"]; ok && vsockRaw != nil {
		vsock, ok := vsockRaw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("config.json: vsock is %T, want object", vsockRaw)
		}
		if sockRaw, ok := vsock["socket"]; ok && sockRaw != nil {
			oldSock, ok := sockRaw.(string)
			if !ok {
				return nil, fmt.Errorf("config.json: vsock.socket is %T, want string", sockRaw)
			}
			if strings.HasPrefix(oldSock, from) {
				newSock := to + strings.TrimPrefix(oldSock, from)
				vsock["socket"] = newSock
				changes = append(changes, fmt.Sprintf("rewrote vsock.socket: %s -> %s", oldSock, newSock))
			}
		}
	}

	return changes, nil
}

// nullifyHostMAC sets net[].host_mac to nil on every virtio-net device.
// CH's open_tap (cloud-hypervisor v51.1 net_util/src/open_tap.rs)
// forces the tap MAC to the saved value when host_mac is Some, which
// would force the clone's tap to take the source's host MAC. Nulling
// it lets CH auto-discover the clone tap's MAC at restore time.
func nullifyHostMAC(cfg map[string]any) ([]string, error) {
	configMap := unwrapConfig(cfg)
	netRaw, ok := configMap["net"]
	if !ok || netRaw == nil {
		return nil, nil
	}
	net, ok := netRaw.([]any)
	if !ok {
		return nil, fmt.Errorf("config.json: net is %T, want array", netRaw)
	}
	var changes []string
	for i, n := range net {
		device, ok := n.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("config.json: net[%d] is %T, want object", i, n)
		}
		if hostRaw, present := device["host_mac"]; present && hostRaw != nil {
			device["host_mac"] = nil
			ifaceID, _ := device["id"].(string)
			if ifaceID == "" {
				ifaceID = fmt.Sprintf("net[%d]", i)
			}
			changes = append(changes, fmt.Sprintf("nulled host_mac on %s (was %v)", ifaceID, hostRaw))
		}
	}
	return changes, nil
}

func configPath(dir string) string {
	if dir == "" {
		return ConfigJSONFilename
	}
	if strings.HasSuffix(dir, "/") {
		return dir + ConfigJSONFilename
	}
	return dir + "/" + ConfigJSONFilename
}
