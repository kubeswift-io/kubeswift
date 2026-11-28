// Package nodefile writes snapshot artifacts into a node directory without
// ever changing a file that is already there.
//
// A clone restored with memoryRestoreMode copyonwrite maps its snapshot's
// memory file privately: pages no clone has written are read from the file
// for as long as the clone runs. Rewriting that file in place (O_TRUNC, then
// write) would change those pages under every running clone. A download
// therefore writes each artifact to a part file in the same directory and
// renames it over the final name once it is complete and verified: a running
// clone keeps the inode it mapped, a new one opens the new file, and a
// reader never sees a partial file. The same holds for two downloads into
// one directory at once (a restore and a clone on the same node).
package nodefile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PartPrefix starts the name of a file or directory being written. Uploads
// skip such names, and a download removes stale ones an interrupted run left.
const PartPrefix = ".kubeswift-part-"

// IsPart reports whether name (a base name) is a part file or directory.
func IsPart(name string) bool {
	return strings.HasPrefix(name, PartPrefix)
}

// Create opens a new part file in dst's directory for dst's content.
func Create(dst string) (*os.File, error) {
	dir, base := filepath.Split(dst)
	if dir == "" {
		dir = "."
	}
	return os.CreateTemp(dir, PartPrefix+base+"-*")
}

// Install moves the finished part file onto dst: it syncs the file, renames
// it (atomic on one filesystem) and syncs the directory, so dst is either the
// old file or the whole new one, also after a crash.
func Install(part, dst string) error {
	f, err := os.OpenFile(part, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	serr := f.Sync()
	cerr := f.Close()
	if serr != nil {
		return fmt.Errorf("sync %s: %w", part, serr)
	}
	if cerr != nil {
		return cerr
	}
	if err := os.Chmod(part, 0o644); err != nil {
		return err
	}
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

// StaleAfter is how long a part file or directory goes unwritten before
// RemoveStaleParts treats it as left by a download that died. Another
// download may be writing into the same directory right now, so a recent
// part is never removed.
const StaleAfter = 6 * time.Hour

// RemoveStaleParts deletes the part files and directories directly under dir
// that nothing has written for StaleAfter (a download killed mid-write). A
// missing dir is not an error.
func RemoveStaleParts(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !IsPart(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if newest, err := newestModTime(p); err != nil || now.Sub(newest) < StaleAfter {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// newestModTime is the latest modification time of p or anything under it.
func newestModTime(p string) (time.Time, error) {
	var newest time.Time
	err := filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}
