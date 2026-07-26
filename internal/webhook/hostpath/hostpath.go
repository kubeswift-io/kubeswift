// Package hostpath confines operator- and tenant-supplied host paths that the
// controller turns into hostPath volumes on the launcher pod.
//
// This exists because the launcher container is privileged (see
// internal/controller/swiftguest/security.go), so a hostPath it mounts is
// effectively node-root access handed to whoever authored the CR. Fields like
// SwiftGuest spec.filesystems[].source.hostPath were validated only as
// non-empty, which let a namespaced tenant mount "/" read-write into their own
// VM via virtiofsd.
//
// The rules are deliberately boring:
//
//   - "..'" is ALWAYS rejected, regardless of configuration. A prefix check is
//     a string comparison, so "/var/lib/kubeswift/../../etc" would otherwise
//     satisfy it. There is no legitimate use of ".." in these fields.
//   - The path must be absolute and clean.
//   - The path must sit under one of the operator-configured allowed prefixes.
//     An EMPTY allowlist denies every path: the fields are niche, so the safe
//     default is that a cluster admin opts in per deployment
//     (swiftGuest.allowedHostPathPrefixes in the chart) rather than every
//     install shipping an escape hatch.
//
// Prefix matching is path-segment aware: "/srv/vm" allows "/srv/vm/a" but NOT
// "/srv/vmsecrets", which a naive strings.HasPrefix would wave through.
package hostpath

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Validate checks p against the allowlist. field names the CR field for the
// error message (e.g. "spec.filesystems[0].source.hostPath").
func Validate(field, p string, allowed []string) error {
	if p == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	// Checked on the raw input, before any cleaning, so a caller cannot smuggle
	// traversal past us by relying on normalization.
	if strings.Contains(p, "..") {
		return fmt.Errorf("%s must not contain '..' (got %q)", field, p)
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%s must be an absolute path (got %q)", field, p)
	}
	if filepath.Clean(p) != strings.TrimRight(p, "/") && filepath.Clean(p) != p {
		return fmt.Errorf("%s must be a clean path (got %q, want %q)", field, p, filepath.Clean(p))
	}
	if len(allowed) == 0 {
		return fmt.Errorf("%s is not permitted: no host-path prefixes are allowed on this cluster "+
			"(set swiftGuest.allowedHostPathPrefixes in the chart to opt in)", field)
	}
	clean := filepath.Clean(p)
	for _, pref := range allowed {
		if underPrefix(clean, filepath.Clean(pref)) {
			return nil
		}
	}
	return fmt.Errorf("%s must be under one of the allowed prefixes %v (got %q)", field, allowed, p)
}

// ValidateDir applies the same rules to the DIRECTORY of a socket path, which
// is what the pod builder actually mounts for vhost-user sockets.
func ValidateDir(field, sock string, allowed []string) error {
	if sock == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	if strings.Contains(sock, "..") {
		return fmt.Errorf("%s must not contain '..' (got %q)", field, sock)
	}
	if !filepath.IsAbs(sock) {
		return fmt.Errorf("%s must be an absolute path (got %q)", field, sock)
	}
	return Validate(field, filepath.Dir(sock), allowed)
}

// underPrefix reports whether p is prefix itself or a path beneath it,
// comparing whole segments so "/srv/vm" does not match "/srv/vmsecrets".
func underPrefix(p, prefix string) bool {
	if prefix == "/" {
		return true
	}
	if p == prefix {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(prefix, "/")+"/")
}
