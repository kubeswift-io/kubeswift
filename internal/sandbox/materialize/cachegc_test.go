package materialize

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// entryFixture creates a published entry of about size bytes, with its size
// record (unless legacy), seal and records, last used at lastUse.
func entryFixture(t *testing.T, cache, use, entry string, size int, lastUse time.Time, legacy bool) {
	t.Helper()
	dir := filepath.Join(cache, entry)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob"), bytes.Repeat([]byte{1}, size), 0o444); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o555)
	if !legacy {
		if _, err := RecordSize(cache, entry); err != nil {
			t.Fatal(err)
		}
	}
	base := strings.TrimSuffix(entry, ".oci")
	for _, m := range []string{filepath.Join(sealedDir, entry), filepath.Join(originDir, base, "repo"), filepath.Join(verifiedDir, base, "key")} {
		p := filepath.Join(cache, m)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, nil, 0o444)
	}
	if err := TouchUse(use, entry, lastUse); err != nil {
		t.Fatal(err)
	}
}

func digestEntry(c byte, oci bool) string {
	e := "sha256-" + strings.Repeat(string(c), 64)
	if oci {
		e += ".oci"
	}
	return e
}

func cacheDirs(t *testing.T) (string, string) {
	t.Helper()
	cache, use := t.TempDir(), t.TempDir()
	t.Cleanup(func() { removeTree(cache) })
	return cache, use
}

const kib = 1024

func TestEnforceLimits_UnderLimitEvictsNothing(t *testing.T) {
	cache, use := cacheDirs(t)
	now := time.Now()
	entryFixture(t, cache, use, digestEntry('a', true), 64*kib, now.Add(-time.Hour), false)
	rep, err := EnforceLimits(cache, use, "", CacheLimits{MaxBytes: 10 << 20}, now)
	if err != nil || rep.Evicted != 0 || rep.OverLimit || rep.Bytes == 0 {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
}

// Least recently used first, until under the limit; the entry just written,
// recently used entries and entries from releases without size records are
// spared. Each evicted entry is unsealed and loses its records; the use lock
// file stays.
func TestEnforceLimits_EvictsLeastRecentlyUsed(t *testing.T) {
	cache, use := cacheDirs(t)
	now := time.Now()
	oldest, older, recent, legacy, keep := digestEntry('a', true), digestEntry('b', true), digestEntry('c', true), digestEntry('d', true), digestEntry('e', true)
	entryFixture(t, cache, use, oldest, 256*kib, now.Add(-3*time.Hour), false)
	entryFixture(t, cache, use, older, 256*kib, now.Add(-2*time.Hour), false)
	entryFixture(t, cache, use, recent, 256*kib, now.Add(-time.Minute), false)
	entryFixture(t, cache, use, legacy, 256*kib, now.Add(-10*time.Hour), true)
	entryFixture(t, cache, use, keep, 256*kib, now.Add(-20*time.Hour), false)
	// About 5 x 256 KiB in the cache; a limit of 3.5 x lets two go.
	rep, err := EnforceLimits(cache, use, keep, CacheLimits{MaxBytes: 900 * kib, MinAge: 10 * time.Minute}, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Evicted != 2 || rep.Legacy != 1 || rep.OverLimit {
		t.Fatalf("rep=%+v", rep)
	}
	for _, gone := range []string{oldest, older} {
		if exists(filepath.Join(cache, gone)) || exists(filepath.Join(cache, sealedDir, gone)) ||
			exists(filepath.Join(cache, originDir, strings.TrimSuffix(gone, ".oci"))) ||
			exists(filepath.Join(cache, verifiedDir, strings.TrimSuffix(gone, ".oci"))) {
			t.Errorf("%s or its records remain", gone)
		}
		if !exists(UseLockPath(use, gone)) {
			t.Errorf("%s: use lock file removed (a holder's lock must stay on its inode)", gone)
		}
	}
	for _, kept := range []string{recent, legacy, keep} {
		if !exists(filepath.Join(cache, kept)) || !exists(filepath.Join(cache, sealedDir, kept)) {
			t.Errorf("%s was evicted", kept)
		}
	}
}

// An entry a launcher holds (shared lock on its use file), or one being
// materialized (its digest lock held), is never evicted.
func TestEnforceLimits_NeverEvictsWhatIsHeld(t *testing.T) {
	cache, use := cacheDirs(t)
	now := time.Now()
	served, fetching := digestEntry('a', true), digestEntry('b', false)
	entryFixture(t, cache, use, served, 256*kib, now.Add(-time.Hour), false)
	entryFixture(t, cache, use, fetching, 256*kib, now.Add(-time.Hour), false)
	f, err := os.Open(UseLockPath(use, served))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockDigest(cache, "sha256:"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	rep, err := EnforceLimits(cache, use, "", CacheLimits{MaxBytes: 1}, now)
	if err != nil || rep.Evicted != 0 || !rep.OverLimit {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if !exists(filepath.Join(cache, served)) || !exists(filepath.Join(cache, fetching)) {
		t.Fatal("a held entry was evicted")
	}
}

// The two layouts of one digest share its records: evicting one keeps them
// while the other remains.
func TestEnforceLimits_SharedRecordsStayWithTheOtherLayout(t *testing.T) {
	cache, use := cacheDirs(t)
	now := time.Now()
	oci, tree := digestEntry('a', true), digestEntry('a', false)
	entryFixture(t, cache, use, oci, 256*kib, now.Add(-3*time.Hour), false)
	entryFixture(t, cache, use, tree, 256*kib, now.Add(-time.Minute), false)
	if _, err := EnforceLimits(cache, use, "", CacheLimits{MaxBytes: 300 * kib, MinAge: 10 * time.Minute}, now); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(cache, oci)) || !exists(filepath.Join(cache, tree)) {
		t.Fatal("wrong entry evicted")
	}
	if !exists(filepath.Join(cache, originDir, strings.TrimSuffix(tree, ".oci"), "repo")) {
		t.Error("the remaining layout lost its origin record")
	}
}

// Still over the limit with everything else spared: the new entry is removed
// and the insertion refused.
func TestEnforceLimits_FullCacheRefusesTheNewEntry(t *testing.T) {
	cache, use := cacheDirs(t)
	now := time.Now()
	recent, fresh := digestEntry('a', true), digestEntry('b', true)
	entryFixture(t, cache, use, recent, 256*kib, now, false)
	entryFixture(t, cache, use, fresh, 256*kib, now, false)
	_, err := EnforceLimits(cache, use, fresh, CacheLimits{MaxBytes: 300 * kib, MinAge: time.Hour}, now)
	if !errors.Is(err, ErrCacheFull) {
		t.Fatalf("err = %v", err)
	}
	if exists(filepath.Join(cache, fresh)) || !exists(filepath.Join(cache, recent)) {
		t.Error("refused entry kept, or the recent one evicted")
	}
}

func TestSweep_RemovesCrashLeftovers(t *testing.T) {
	cache, _ := cacheDirs(t)
	now := time.Now()
	trash := filepath.Join(cache, trashPrefix+digestEntry('a', true))
	oldTmp := filepath.Join(cache, ".materialize-old")
	newTmp := filepath.Join(cache, ".materialize-new")
	for _, d := range []string{trash, oldTmp, newTmp} {
		_ = os.MkdirAll(filepath.Join(d, "x"), 0o755)
	}
	_ = os.Chmod(filepath.Join(trash, "x"), 0o555)
	_ = os.Chtimes(oldTmp, now.Add(-3*time.Hour), now.Add(-3*time.Hour))
	sweep(cache, now)
	if exists(trash) || exists(oldTmp) {
		t.Error("leftovers remain")
	}
	if !exists(newTmp) {
		t.Error("a run in progress was swept")
	}
}

func TestTouchUse_RefusesNonEntries(t *testing.T) {
	use := t.TempDir()
	for _, bad := range []string{"../x", "/etc/passwd", "sha256-zz", ""} {
		if err := TouchUse(use, bad, time.Now()); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A sparse member is refused, in both of GNU tar's encodings (old GNU 'S'
// and PAX GNU.sparse.*): extracted, its apparent size is not what it costs.
func TestEntryLimiter_RefusesSparse(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Truncate(64 << 20) // a hole: 64 MiB apparent, nothing allocated
	_, _ = f.WriteAt([]byte{1}, 32<<20)
	f.Close()
	for _, format := range []string{"gnu", "posix"} {
		out, err := exec.Command("tar", "--sparse", "--format="+format, "-C", dir, "-cf", "-", "sparse").Output()
		if err != nil {
			t.Skipf("tar --sparse: %v", err)
		}
		l := newEntryLimiter(bytes.NewReader(out), 0)
		_, _ = io.Copy(io.Discard, l)
		if !l.sparse.Load() {
			t.Errorf("format %s: sparse member not refused", format)
		}
	}
	// An ordinary member passes.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "f", Typeflag: tar.TypeReg, Size: 1})
	_, _ = tw.Write([]byte{1})
	tw.Close()
	l := newEntryLimiter(&buf, 0)
	_, _ = io.Copy(io.Discard, l)
	if l.sparse.Load() {
		t.Error("ordinary member refused")
	}
}
