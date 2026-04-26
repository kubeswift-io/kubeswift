// snapshot-stager — init container helper for SwiftRestore Tier B clones.
//
// Stages a SwiftSnapshot's read-only on-node directory into a writable
// pod-local copy, applies the narrow set of in-place edits to
// config.json that the SwiftRestore controller asks for (clone cmdline
// marker + per-clone MAC rewrites), and signals completion via a
// sentinel file.
//
// Run flow (idempotent across init-container restarts):
//
//   1. If <dst>/.copy-complete exists, exit 0. emptyDir survives an
//      init-container restart, so a partially-completed prior run
//      that finished before sentinel-write is correctly skipped, and
//      a partially-completed prior run that DID NOT finish appears as
//      "no sentinel" and falls through to step 2.
//   2. Wipe <dst> contents (everything except <dst> itself). This
//      handles the partial-copy recovery path — a previous run that
//      crashed mid-cp leaves a corrupted partial tree we must not
//      build on top of.
//   3. cp -a <src>/. <dst>/  (preserve mode/owner/timestamps).
//   4. Read <dst>/config.json, apply the requested patches via the
//      shared internal/snapshot/configjson package, write back.
//   5. Touch <dst>/.copy-complete LAST.
//
// The sentinel-after-patch ordering is important: a pod that observes
// the sentinel must also observe a fully-patched config.json. Putting
// the sentinel write last means a power loss between cp and patch
// looks like "no sentinel", which the next run treats as "wipe and
// retry" — correct.
//
// In-place restore (target.name == source.name with no identity
// regeneration) does NOT use this stager: the SwiftGuest controller
// mounts the snapshot directory read-only directly, bypassing
// staging entirely. See docs/snapshots/local-snapshots.md.

package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/projectbeskar/kubeswift/internal/snapshot/configjson"
)

const sentinelFilename = ".copy-complete"

func main() {
	src := flag.String("src", "", "source snapshot directory (read-only mount)")
	dst := flag.String("dst", "", "destination directory (writable, typically an emptyDir)")
	appendCmdlineMarker := flag.Bool("append-cmdline-marker", false,
		"append the kubeswift.clone=true marker to the kernel cmdline in config.json")
	rewriteMACsCSV := flag.String("rewrite-macs", "",
		"comma-separated MAC list, indexed by config.net[]; empty entries leave the source MAC unchanged")
	flag.Parse()

	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "snapshot-stager: --src and --dst are required")
		os.Exit(2)
	}

	if err := run(*src, *dst, *appendCmdlineMarker, parseMACsCSV(*rewriteMACsCSV)); err != nil {
		fmt.Fprintln(os.Stderr, "snapshot-stager:", err)
		os.Exit(1)
	}
}

func run(src, dst string, appendCmdlineMarker bool, rewriteMACs []string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("source dir not accessible: %w", err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("ensure dst: %w", err)
	}

	sentinel := filepath.Join(dst, sentinelFilename)
	if _, err := os.Stat(sentinel); err == nil {
		fmt.Println("snapshot-stager: sentinel present, skipping copy (idempotent retry)")
		return nil
	}

	// Wipe partial state from any prior run that didn't reach the
	// sentinel. emptyDir entries survive init-container restarts —
	// without this wipe a half-copied tree could be reused.
	fmt.Printf("snapshot-stager: wiping %s\n", dst)
	if err := wipeContents(dst); err != nil {
		return fmt.Errorf("wipe dst: %w", err)
	}

	fmt.Printf("snapshot-stager: copying %s -> %s\n", src, dst)
	if err := copyTree(src, dst); err != nil {
		return fmt.Errorf("copy: %w", err)
	}

	if appendCmdlineMarker || len(rewriteMACs) > 0 {
		fmt.Println("snapshot-stager: patching config.json")
		cfg, err := configjson.Read(dst)
		if err != nil {
			return fmt.Errorf("read config.json: %w", err)
		}
		changes, err := configjson.Patch(cfg, configjson.PatchOptions{
			AppendCmdlineMarker: appendCmdlineMarker,
			RewriteMACs:         rewriteMACs,
		})
		if err != nil {
			return fmt.Errorf("patch config.json: %w", err)
		}
		for _, c := range changes {
			fmt.Println("snapshot-stager:  -", c)
		}
		if err := configjson.Write(dst, cfg); err != nil {
			return fmt.Errorf("write config.json: %w", err)
		}
	} else {
		fmt.Println("snapshot-stager: no config.json patches requested")
	}

	if err := os.WriteFile(sentinel, []byte("ok\n"), 0o644); err != nil {
		return fmt.Errorf("write sentinel: %w", err)
	}
	fmt.Println("snapshot-stager: complete")
	return nil
}

// parseMACsCSV splits a comma-separated MAC list. Empty input produces
// an empty slice. Empty positions ("a,,b") preserve source MACs at
// those indices, matching configjson.PatchOptions semantics.
func parseMACsCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = strings.TrimSpace(p)
	}
	return out
}

// wipeContents removes every entry inside dir but leaves dir itself.
// emptyDir mounts cannot be recreated by this binary (it doesn't own
// the mount), so we clear contents instead of removing the directory.
func wipeContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return nil
}

// copyTree mirrors the source directory tree into dst. Preserves
// regular files, directories, and symlinks. Snapshots only contain
// regular files (config.json, state.json, memory-ranges) plus
// possibly subdirectories CH may add in future versions, so the
// implementation is deliberately minimal — no device files, no
// hardlink dedup, no xattrs.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm())
		case d.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		default:
			return copyFile(path, target)
		}
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
