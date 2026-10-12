package materialize

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Lifecycle of the node artifact cache.
//
// Size: each entry's size (allocated blocks, so a sparse file cannot claim more
// or less than it occupies) is recorded at insertion in .size/<entry>. An
// entry without one was published by a release that did not record sizes: it
// is counted (in .size-legacy) but never evicted, because a launcher of that
// release takes no use lock either.
//
// Use: <useDir>/<entry> is a lock file per entry, created on first use and
// never unlinked (a holder's lock must stay on the inode an evictor locks).
// A launcher serving the entry holds a shared flock on it for its whole life;
// the kernel drops the lock when the launcher dies, so a crash never leaks a
// reference. Every use (launcher or materializer) sets its mtime: the entry's
// last use.
//
// Eviction runs in the materializer, after it publishes an entry, while the
// cache is over its limit: least recently used first, never an entry used
// within MinAge, never one whose use lock or digest lock is held. It unseals
// the entry first (a launcher checking it then sees a miss and has it fetched
// again), renames it away, and removes its origin and verification records
// when no entry of the digest (in either layout) remains.

const (
	sizeDir       = ".size"
	legacySizeDir = ".size-legacy"
	gcLockName    = ".gc.lock"
	trashPrefix   = ".trash-"
	// staleTemp is how old a .materialize-* directory must be to be a crash
	// leftover: longer than any run (the run bound is 30 minutes).
	staleTemp = 2 * time.Hour
)

// entryNameRE matches cache entry names: sha256-<hex>, optionally .oci.
var entryNameRE = regexp.MustCompile(`^sha256-[0-9a-f]{64}(\.oci)?$`)

// ErrCacheFull is returned when an entry cannot be kept because the cache is
// still over its limit after evicting everything evictable.
var ErrCacheFull = errors.New("node artifact cache is full")

// CacheLimits bound the cache. Zero values disable each bound.
type CacheLimits struct {
	MaxBytes   int64
	MaxPercent int // of the cache filesystem
	// MinAge: an entry used more recently is never evicted.
	MinAge time.Duration
}

// Enabled reports whether any limit is set.
func (l CacheLimits) Enabled() bool { return l.MaxBytes > 0 || l.MaxPercent > 0 }

// CacheReport is what an enforcement pass found and did. It names no entry or
// digest: it goes in a tenant pod's termination message.
type CacheReport struct {
	Bytes        int64 `json:"bytes"`
	LimitBytes   int64 `json:"limitBytes"`
	Evicted      int   `json:"evicted"`
	EvictedBytes int64 `json:"evictedBytes"`
	OverLimit    bool  `json:"overLimit"`
	Legacy       int   `json:"legacy,omitempty"`
}

// UseLockPath is the use lock of entry (a base name) in useDir.
func UseLockPath(useDir, entry string) string { return filepath.Join(useDir, entry) }

// TouchUse records a use of entry now: creates its lock file if needed and
// sets its mtime.
func TouchUse(useDir, entry string, now time.Time) error {
	if !entryNameRE.MatchString(entry) {
		return fmt.Errorf("use: %q is not a cache entry", entry)
	}
	if err := os.MkdirAll(useDir, 0o755); err != nil {
		return err
	}
	p := UseLockPath(useDir, entry)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return err
	}
	f.Close()
	return os.Chtimes(p, now, now)
}

// RecordSize records entry's size, in allocated bytes.
func RecordSize(cacheDir, entry string) (int64, error) {
	n, err := allocatedSize(filepath.Join(cacheDir, entry))
	if err != nil {
		return 0, err
	}
	return n, writeSizeFile(filepath.Join(cacheDir, sizeDir), entry, n)
}

func writeSizeFile(dir, entry string, n int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.WriteString(strconv.FormatInt(n, 10))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(name)
		return errors.Join(werr, cerr)
	}
	return os.Rename(name, filepath.Join(dir, entry))
}

func readSizeFile(dir, entry string) (int64, bool) {
	b, err := os.ReadFile(filepath.Join(dir, entry))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil && n >= 0
}

// allocatedSize sums the blocks allocated under root, symlinks not followed.
func allocatedSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			total += st.Blocks * 512
		}
		return nil
	})
	return total, err
}

// cacheEntry is one entry as accounted.
type cacheEntry struct {
	name    string
	bytes   int64
	legacy  bool
	lastUse time.Time
}

// EnforceLimits brings the cache under lim, sparing keep (the entry just
// published) and every entry in use or used within lim.MinAge. If the cache
// stays over the limit and keep is set, keep is removed too and ErrCacheFull
// returned: a full cache refuses new content rather than fill the node.
func EnforceLimits(cacheDir, useDir, keep string, lim CacheLimits, now time.Time) (CacheReport, error) {
	var rep CacheReport
	unlock, err := flockPath(filepath.Join(cacheDir, gcLockName), syscall.LOCK_EX)
	if err != nil {
		return rep, fmt.Errorf("cache gc lock: %w", err)
	}
	defer unlock()
	sweep(cacheDir, now)

	entries, err := accountEntries(cacheDir, useDir)
	if err != nil {
		return rep, err
	}
	for _, e := range entries {
		rep.Bytes += e.bytes
		if e.legacy {
			rep.Legacy++
		}
	}
	rep.LimitBytes = limitBytes(cacheDir, lim)
	if !lim.Enabled() || rep.Bytes <= rep.LimitBytes {
		return rep, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].lastUse.Before(entries[j].lastUse) })
	for _, e := range entries {
		if rep.Bytes <= rep.LimitBytes {
			break
		}
		if e.legacy || e.name == keep || now.Sub(e.lastUse) < lim.MinAge {
			continue
		}
		if evict(cacheDir, useDir, e.name) {
			rep.Bytes -= e.bytes
			rep.Evicted++
			rep.EvictedBytes += e.bytes
		}
	}
	if rep.Bytes <= rep.LimitBytes {
		return rep, nil
	}
	rep.OverLimit = true
	if keep != "" {
		var size int64
		for _, e := range entries {
			if e.name == keep {
				size = e.bytes
			}
		}
		if evict(cacheDir, useDir, keep) {
			rep.Bytes -= size
			return rep, fmt.Errorf("%w: %d bytes of entries in use or used within %s against a limit of %d",
				ErrCacheFull, rep.Bytes, lim.MinAge, rep.LimitBytes)
		}
	}
	return rep, nil
}

// limitBytes is the effective limit: the smaller of MaxBytes and MaxPercent
// of the cache filesystem.
func limitBytes(cacheDir string, lim CacheLimits) int64 {
	limit := lim.MaxBytes
	if lim.MaxPercent > 0 {
		var st syscall.Statfs_t
		if err := syscall.Statfs(cacheDir, &st); err == nil {
			fs := int64(st.Blocks) * int64(st.Bsize) * int64(lim.MaxPercent) / 100
			if limit <= 0 || fs < limit {
				limit = fs
			}
		}
	}
	return limit
}

// accountEntries lists the cache's entries with their sizes and last use.
func accountEntries(cacheDir, useDir string) ([]cacheEntry, error) {
	des, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil, err
	}
	var out []cacheEntry
	for _, de := range des {
		name := de.Name()
		if !de.IsDir() || !entryNameRE.MatchString(name) {
			continue
		}
		e := cacheEntry{name: name}
		if n, ok := readSizeFile(filepath.Join(cacheDir, sizeDir), name); ok {
			e.bytes = n
		} else {
			e.legacy = true
			if n, ok := readSizeFile(filepath.Join(cacheDir, legacySizeDir), name); ok {
				e.bytes = n
			} else if n, err := allocatedSize(filepath.Join(cacheDir, name)); err == nil {
				e.bytes = n
				_ = writeSizeFile(filepath.Join(cacheDir, legacySizeDir), name, n)
			}
		}
		if fi, err := os.Stat(UseLockPath(useDir, name)); err == nil {
			e.lastUse = fi.ModTime()
		} else if fi, err := os.Stat(filepath.Join(cacheDir, sizeDir, name)); err == nil {
			e.lastUse = fi.ModTime()
		}
		out = append(out, e)
	}
	return out, nil
}

// evict removes entry when no one holds its use lock or its digest lock. It
// reports whether it did.
func evict(cacheDir, useDir, entry string) bool {
	useUnlock, err := tryLockStable(UseLockPath(useDir, entry))
	if err != nil {
		return false // in use (or unlockable): never evicted
	}
	defer useUnlock()
	digest := "sha256:" + strings.TrimSuffix(strings.TrimPrefix(entry, "sha256-"), ".oci")
	digUnlock, err := tryLockStable(filepath.Join(cacheDir, ".lock-"+strings.ReplaceAll(digest, ":", "-")))
	if err != nil {
		return false // being materialized
	}
	defer digUnlock()

	// Unsealed first: from here a launcher's check is a miss.
	_ = os.Remove(filepath.Join(cacheDir, sealedDir, entry))
	trash := filepath.Join(cacheDir, trashPrefix+entry)
	if err := os.Rename(filepath.Join(cacheDir, entry), trash); err != nil {
		return false
	}
	_ = os.Remove(filepath.Join(cacheDir, sizeDir, entry))
	// The digest's records go only when no entry of it (either layout) is left.
	base := strings.TrimSuffix(entry, ".oci")
	if !exists(filepath.Join(cacheDir, base)) && !exists(filepath.Join(cacheDir, base+".oci")) {
		_ = os.RemoveAll(filepath.Join(cacheDir, originDir, base))
		_ = os.RemoveAll(filepath.Join(cacheDir, verifiedDir, base))
	}
	removeTree(trash)
	return true
}

// tryLockStable takes a non-blocking exclusive flock on path (creating it),
// and checks the inode it locked is still the one at path: a lock on a file
// replaced since protects nothing.
func tryLockStable(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	release := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	var held, cur syscall.Stat_t
	if syscall.Fstat(int(f.Fd()), &held) != nil || syscall.Stat(path, &cur) != nil || held.Ino != cur.Ino || held.Dev != cur.Dev {
		release()
		return nil, errors.New("lock file replaced")
	}
	return release, nil
}

// flockPath takes a blocking flock of the given kind on path (creating it).
func flockPath(path string, how int) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// sweep removes what a crashed run left: trash (already unsealed and
// renamed away) and temporary directories older than any run.
func sweep(cacheDir string, now time.Time) {
	des, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, de := range des {
		name := de.Name()
		p := filepath.Join(cacheDir, name)
		switch {
		case strings.HasPrefix(name, trashPrefix):
			removeTree(p)
		case strings.HasPrefix(name, ".materialize-"):
			if fi, err := de.Info(); err == nil && now.Sub(fi.ModTime()) > staleTemp {
				removeTree(p)
			}
		}
	}
}

// removeTree deletes a tree whose directories may be read-only (0555).
func removeTree(root string) {
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	_ = os.RemoveAll(root)
}
