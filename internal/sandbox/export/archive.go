// Package export pushes a packaged sandbox output (a canonical tar left in the
// node spool by the launcher) as an OCI artifact. It runs in the export Job,
// which alone holds the push credential. It trusts nothing about the archive:
// the launcher that wrote it serves a guest, so every rule is checked again.
package export

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"strings"
)

// Reasons an export fails (status.outputs[].reason).
const (
	ReasonInvalidEntry  = "OutputInvalidEntry"
	ReasonTooLarge      = "OutputTooLarge"
	ReasonTooMany       = "OutputTooManyEntries"
	ReasonUnauthorized  = "RegistryUnauthorized"
	ReasonUnavailable   = "RegistryUnavailable"
	ReasonArchiveAbsent = "OutputLost"
)

// Error is a definitive export failure with its reason: retrying cannot help.
type Error struct {
	Reason  string
	Message string
}

func (e *Error) Error() string { return e.Reason + ": " + e.Message }

func refuse(reason, format string, args ...any) error {
	return &Error{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Summary describes a verified archive.
type Summary struct {
	// Digest is the sha256 of the archive bytes ("sha256:<hex>").
	Digest       string
	Bytes        int64
	ContentBytes int64
	Entries      int64
}

// VerifyArchive reads a whole archive and accepts only the canonical form
// swiftletd writes: regular files and directories, clean relative paths,
// each directory before its contents, no duplicates, no names starting .wh.,
// uid/gid 0 without names, mtime 0, modes 0755 or 0644, PAX records only for
// path and size, nothing after the end.
func VerifyArchive(r io.Reader, maxBytes, maxEntries int64) (Summary, error) {
	h := sha256.New()
	cr := &countingReader{r: io.TeeReader(r, h)}
	tr := tar.NewReader(cr)
	seen := map[string]bool{}
	var s Summary
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s, refuse(ReasonInvalidEntry, "not a valid archive: %v", err)
		}
		name := hdr.Name
		dir := hdr.Typeflag == tar.TypeDir
		switch {
		case hdr.Typeflag == tar.TypeReg:
		case dir:
			if !strings.HasSuffix(name, "/") {
				return s, refuse(ReasonInvalidEntry, "directory %q without a trailing slash", name)
			}
			name = strings.TrimSuffix(name, "/")
		default:
			return s, refuse(ReasonInvalidEntry, "%q has type %q: only regular files and directories", name, hdr.Typeflag)
		}
		for k := range hdr.PAXRecords {
			if k != "path" && k != "size" {
				return s, refuse(ReasonInvalidEntry, "%q carries PAX record %q", name, k)
			}
		}
		if name == "" || strings.HasPrefix(name, "/") || path.Clean(name) != name || name == "." {
			return s, refuse(ReasonInvalidEntry, "%q is not a clean relative path", hdr.Name)
		}
		for _, c := range strings.Split(name, "/") {
			if c == ".." || strings.HasPrefix(c, ".wh.") {
				return s, refuse(ReasonInvalidEntry, "%q has a forbidden component %q", name, c)
			}
		}
		if seen[name] {
			return s, refuse(ReasonInvalidEntry, "%q appears twice", name)
		}
		if p := path.Dir(name); p != "." && !seen[p] {
			return s, refuse(ReasonInvalidEntry, "%q comes before its directory", name)
		}
		seen[name] = true
		if hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" || !hdr.ModTime.Equal(zeroTime) {
			return s, refuse(ReasonInvalidEntry, "%q is not in canonical form (owner or time)", name)
		}
		mode := hdr.Mode & 0o7777
		if (dir && mode != 0o755) || (!dir && mode != 0o755 && mode != 0o644) {
			return s, refuse(ReasonInvalidEntry, "%q has mode %o", name, mode)
		}
		s.Entries++
		if s.Entries > maxEntries {
			return s, refuse(ReasonTooMany, "more than %d entries", maxEntries)
		}
		if !dir {
			s.ContentBytes += hdr.Size
			if s.ContentBytes > maxBytes {
				return s, refuse(ReasonTooLarge, "more than %d bytes of files", maxBytes)
			}
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return s, refuse(ReasonInvalidEntry, "%q: %v", name, err)
			}
		}
	}
	// After the end: only zero padding.
	var buf [4096]byte
	for {
		n, err := cr.Read(buf[:])
		for _, b := range buf[:n] {
			if b != 0 {
				return s, refuse(ReasonInvalidEntry, "data after the end of the archive")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s, err
		}
	}
	s.Bytes = cr.n
	s.Digest = digestOf(h)
	return s, nil
}

func digestOf(h hash.Hash) string { return "sha256:" + hex.EncodeToString(h.Sum(nil)) }

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
