package materialize

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Read-only artifact permissions.
//
// A read-only artifact (spec.artifacts) is shared over virtio-fs, which passes
// host ownership and mode through to the guest unchanged. Whatever the cache
// entry's modes are is what the guest workload sees, and the workload is often
// not root. The writers that build an entry leave modes that depend on how each
// file was written: go-containerregistry streams layer blobs through
// os.CreateTemp (0600) and writes index.json and oci-layout with 0777 less the
// umask; tar keeps whatever the archive says. A layer blob owned by root at
// 0600 cannot be read by an unprivileged guest process.
//
// So an entry is normalized before it is published: every directory 0555,
// every regular file 0444, or 0555 when it was executable (only an unpacked
// tree keeps execute bits; an OCI layout's blobs are data). Write, setuid,
// setgid and sticky bits are removed. Symlinks are left as they are: the guest
// resolves them inside its own mount. Anything else (device nodes, FIFOs,
// sockets) has no meaning in an artifact and is removed. Ownership is left
// unchanged: with these modes every user can read, so the owner does not
// decide access, and no writer is ever the guest.
//
// The rootfs cache is never normalized: an image's own modes (/tmp 1777,
// setuid binaries, 0640 files) are part of the image.

const (
	roDirMode  fs.FileMode = 0o555
	roFileMode fs.FileMode = 0o444
	roExecMode fs.FileMode = 0o555
)

// normalizeReadOnly applies the read-only artifact modes to the tree at root,
// except root's own mode: renaming a directory into the cache needs write
// permission on it, so the caller publishes it and then calls sealReadOnly.
// keepExec keeps execute permission on files that had any execute bit. It
// returns how many special files it removed.
func normalizeReadOnly(root string, keepExec bool) (removed int, err error) {
	type entry struct {
		path string
		mode fs.FileMode
	}
	var dirs []string
	var files []entry
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		switch t := d.Type(); {
		case t.IsDir():
			dirs = append(dirs, p)
		case t&fs.ModeSymlink != 0:
		case t.IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			files = append(files, entry{p, info.Mode()})
		default:
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("remove special file %s: %w", p, err)
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return removed, err
	}
	for _, f := range files {
		m := roFileMode
		if keepExec && f.mode.Perm()&0o111 != 0 {
			m = roExecMode
		}
		if err := os.Chmod(f.path, m); err != nil {
			return removed, fmt.Errorf("chmod %s: %w", f.path, err)
		}
	}
	// Deepest first, so a parent is still writable while its children change
	// (only matters to a non-root writer, as in tests).
	for i := len(dirs) - 1; i >= 1; i-- { // dirs[0] is root
		if err := os.Chmod(dirs[i], roDirMode); err != nil {
			return removed, fmt.Errorf("chmod %s: %w", dirs[i], err)
		}
	}
	return removed, nil
}

// sealReadOnly sets root's own mode, last. Until it runs, needsReadOnlyRepair
// reports the entry, so a crash between publishing and sealing is repaired by
// the next cache hit.
func sealReadOnly(root string) error {
	if err := os.Chmod(root, roDirMode); err != nil {
		return fmt.Errorf("chmod %s: %w", root, err)
	}
	return nil
}

// needsReadOnlyRepair reports whether a published entry predates
// normalization. An entry is normalized as a whole before it is published, so
// its top directory's mode is a sufficient marker.
func needsReadOnlyRepair(root string) bool {
	fi, err := os.Stat(root)
	return err == nil && fi.IsDir() && fi.Mode().Perm() != roDirMode
}

// repairReadOnly normalizes an entry published before normalization existed.
// Called under the digest lock. Only makes files more readable and less
// writable, so a sandbox reading the entry meanwhile is unaffected.
func repairReadOnly(root string, keepExec bool) error {
	if !needsReadOnlyRepair(root) {
		return nil
	}
	if _, err := normalizeReadOnly(root, keepExec); err != nil {
		return fmt.Errorf("repair modes of %s: %w", root, err)
	}
	if err := sealReadOnly(root); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "materialize: repaired read-only modes of %s\n", root)
	return nil
}
