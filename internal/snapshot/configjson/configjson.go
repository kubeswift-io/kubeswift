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
// RewriteMACs is wired in commit 13. Holds (per-NIC-name) the new
// MAC the controller computed via deterministic hash. Empty map is
// a no-op.
type PatchOptions struct {
	AppendCmdlineMarker bool
	RewriteMACs         map[string]string
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
	return changes, nil
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
	cmdlineRaw, ok := payload["cmdline"]
	if !ok {
		// No cmdline at all — that's unusual for CH but possible for
		// initramfs-only boots. Set it directly to the marker.
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

// navigateToPayload returns the inner config.payload object. It exists
// to centralize the "this is what CH's config.json looks like"
// assumption — when CH bumps its config version this is the function
// that needs updating, not every call site.
func navigateToPayload(cfg map[string]any) (map[string]any, error) {
	configRaw, ok := cfg["config"]
	if !ok {
		return nil, fmt.Errorf("config.json: top-level 'config' missing")
	}
	configMap, ok := configRaw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config.json: top-level 'config' is %T, want object", configRaw)
	}
	payloadRaw, ok := configMap["payload"]
	if !ok {
		// CH's config.json sometimes has "payload" inline at top level;
		// allow either nesting.
		return configMap, nil
	}
	payload, ok := payloadRaw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config.json: config.payload is %T, want object", payloadRaw)
	}
	return payload, nil
}

// rewriteMACs is wired in commit 13. Stub here so commit 12's tests
// can lock the public API shape now and commit 13 fills in the body
// without breaking callers.
func rewriteMACs(cfg map[string]any, byName map[string]string) ([]string, error) {
	_ = cfg
	_ = byName
	return nil, fmt.Errorf("rewriteMACs not yet implemented (commit 13)")
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
