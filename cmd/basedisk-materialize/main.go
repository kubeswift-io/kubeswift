// Command basedisk-materialize prepares a shared-base guest's root disk on the
// node it runs on, and destroys it again. It has three modes, used by three
// different pods:
//
//   - create      — the per-guest materialise Job. Mounts the SwiftImage's
//     prepared PVC, writes the base into the node's thin pool if this node does
//     not already hold it, and snapshots it for the guest. Runs ONCE per guest.
//   - reactivate  — the launcher's init container. Re-maps the guest's existing
//     disk, which is only work after a node reboot. Never needs the image.
//   - release     — the per-guest release Job, when the guest is deleted. Frees
//     the guest's disk. Never creates a pool.
//
// They are separate pods, not one init container, because the image PVC is
// ReadWriteOnce and a PVC stays attached for the life of the pod that mounts
// it. An init container would pin the image to this node for as long as the
// guest ran, and no other guest of that image could be created on any other
// node meanwhile. A Job releases it when it exits — the same reason the
// existing root-disk Copy Job is a Job.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/kubeswift-io/kubeswift/internal/thinpool"
)

const (
	modeCreate     = "create"
	modeReactivate = "reactivate"
	modeRelease    = "release"
)

type config struct {
	mode string

	root          string // node state: registry, locks, backing files
	pool          string
	poolDataBytes uint64

	guestKey   string // namespace/name/uid — the UID is what keeps a recreated guest off its predecessor's disk
	device     string
	guestBytes uint64

	// create mode only
	baseKey string
	image   string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.mode, "mode", "", "create (materialise Job), reactivate (launcher init container) or release (release Job)")
	flag.StringVar(&cfg.root, "root", "/var/lib/kubeswift", "node state directory")
	flag.StringVar(&cfg.pool, "pool", "kubeswift-pool", "device-mapper name of the node's thin pool")
	flag.Uint64Var(&cfg.poolDataBytes, "pool-data-bytes", 0, "size of the pool's preallocated data file")
	flag.StringVar(&cfg.guestKey, "guest-key", "", "namespace/name/uid of the guest")
	flag.StringVar(&cfg.device, "device", "", "device-mapper name to map the guest's disk at")
	flag.Uint64Var(&cfg.guestBytes, "guest-bytes", 0, "size of the guest's root disk")
	flag.StringVar(&cfg.baseKey, "base-key", "", "content identity of the image (create mode)")
	flag.StringVar(&cfg.image, "image", "", "path to the raw image to build the base from (create mode)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "basedisk-materialize:", err)
		os.Exit(1)
	}
}

func (c config) validate() error {
	var missing []string
	req := func(name string, set bool) {
		if !set {
			missing = append(missing, name)
		}
	}
	req("--mode", c.mode != "")
	req("--guest-key", c.guestKey != "")
	req("--device", c.device != "")
	switch c.mode {
	case modeCreate:
		req("--guest-bytes", c.guestBytes != 0)
		req("--pool-data-bytes", c.poolDataBytes != 0)
		req("--base-key", c.baseKey != "")
		req("--image", c.image != "")
	case modeReactivate, "":
		req("--guest-bytes", c.guestBytes != 0)
		req("--pool-data-bytes", c.poolDataBytes != 0)
	case modeRelease:
		// Sizes are for building a disk. Release only takes one away.
	default:
		return fmt.Errorf("unknown --mode %q: want %s, %s or %s", c.mode, modeCreate, modeReactivate, modeRelease)
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required flags: %v", missing)
	}
	return nil
}

func run(ctx context.Context, cfg config, out io.Writer) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if cfg.mode == modeRelease {
		return release(ctx, cfg, out)
	}
	if cfg.mode == modeReactivate {
		// Never create a pool here. The launcher only runs where the disk was
		// built, so a node with no pool has lost it — and openNode would
		// preallocate a whole pool of this node's disk before the refusal
		// below could say the disk is gone.
		if err := poolMustExist(cfg); err != nil {
			return err
		}
	}
	x, err := openNode(ctx, cfg)
	if err != nil {
		return err
	}

	known, err := x.HasGuest(cfg.guestKey)
	if err != nil {
		return err
	}

	switch cfg.mode {
	case modeReactivate:
		// The controller only starts a launcher once the materialise Job has
		// succeeded, so this node MUST already hold the guest's disk. If it does
		// not, the disk is gone — the registry was lost, or the pool with it —
		// and the guest's VM may have run and written. Creating a fresh one here
		// would boot it as if new and discard all of that without a trace, so
		// this is an error and stays one.
		if !known {
			return fmt.Errorf("guest %s should have a disk on this node but the node has no record of it; "+
				"refusing to create a fresh one, which would silently discard everything the guest wrote. "+
				"If the node's pool or %s was lost, the guest's disk was lost with it: delete and recreate the guest",
				cfg.guestKey, thinpool.DefaultRegistryPath(cfg.root))
		}
		path, err := x.EnsureGuest(ctx, "", cfg.guestKey, cfg.device, cfg.guestBytes)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "reactivated %s at %s\n", cfg.guestKey, path)
		return nil

	case modeCreate:
		imageBytes, err := fileSize(cfg.image)
		if err != nil {
			return err
		}
		if cfg.guestBytes < imageBytes {
			return fmt.Errorf("the guest's root disk (%d bytes) is smaller than its image (%d bytes); a disk cannot be shrunk below the image it starts from",
				cfg.guestBytes, imageBytes)
		}
		// The pool space the write needs is bounded by the image's data, not
		// its size: only non-zero blocks are written.
		needBytes, err := thinpool.PoolBytesForFile(cfg.image, imageBytes)
		if err != nil {
			return err
		}
		if _, err := x.EnsureBaseNeeding(ctx, cfg.baseKey, imageBytes, needBytes, func() (io.ReadCloser, error) { return os.Open(cfg.image) }); err != nil {
			return err
		}
		path, err := x.EnsureGuest(ctx, cfg.baseKey, cfg.guestKey, cfg.device, cfg.guestBytes)
		if err != nil {
			return err
		}
		// A disk presented larger than its image still has the image's backup
		// GPT header in the middle of it. Move it to the end — exactly what the
		// existing clone paths do for a grown disk — so the guest's own tooling
		// can grow the partition into the space.
		//
		// Whenever it grew, including for a guest this node already knows.
		// Create mode only ever runs before the guest's VM has: the controller
		// starts the launcher once this Job has SUCCEEDED, and never runs it
		// again after that. So the partition table is still the image's, and
		// running this is always safe here. Skipping it for a known guest would
		// strand one whose first attempt snapshotted the disk and then died
		// before getting this far: the retry sees the guest, and the header
		// would stay mid-disk forever.
		if cfg.guestBytes > imageBytes {
			if msg, err := exec.CommandContext(ctx, "sgdisk", "-e", path).CombinedOutput(); err != nil {
				return fmt.Errorf("moving the backup GPT header to the end of %s: %w: %s", path, err, msg)
			}
		}
		verb := "created"
		if known {
			verb = "reactivated"
		}
		fmt.Fprintf(out, "%s %s at %s\n", verb, cfg.guestKey, path)
		return nil
	}
	return fmt.Errorf("unreachable: mode %q", cfg.mode)
}

// release frees the guest's disk on this node.
//
// It never creates a pool. The controller runs it on the node recorded for the
// guest, which can be one the guest's materialise Job never reached, and a
// release there must not leave a preallocated pool behind for nothing.
func release(ctx context.Context, cfg config, out io.Writer) error {
	dataPath, metaPath := poolFiles(cfg.root)
	data, err := fileExists(dataPath)
	if err != nil {
		return err
	}
	meta, err := fileExists(metaPath)
	if err != nil {
		return err
	}
	switch {
	case !data && !meta:
		fmt.Fprintf(out, "nothing to release: this node has no thin pool, so it holds no disk for %s\n", cfg.guestKey)
		return nil
	case !data || !meta:
		// openNode would create the missing half, and a fresh metadata device
		// under an existing data device is a pool that has forgotten every
		// disk in it. Refuse rather than make that happen.
		return fmt.Errorf("the thin pool's backing files are incomplete (%s: %t, %s: %t); refusing to touch it",
			dataPath, data, metaPath, meta)
	}
	x, err := openNode(ctx, cfg)
	if err != nil {
		return err
	}
	known, err := x.HasGuest(cfg.guestKey)
	if err != nil {
		return err
	}
	if err := x.ReleaseGuest(ctx, cfg.guestKey, cfg.device); err != nil {
		return err
	}
	if !known {
		fmt.Fprintf(out, "nothing to release: this node holds no disk for %s\n", cfg.guestKey)
		return nil
	}
	fmt.Fprintf(out, "released %s\n", cfg.guestKey)
	return nil
}

// poolMustExist refuses when this node has no pool to reactivate from.
func poolMustExist(cfg config) error {
	data, meta := poolFiles(cfg.root)
	for _, p := range []string{data, meta} {
		ok, err := fileExists(p)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("this node has no thin pool (%s is missing), so the disk of guest %s is gone "+
				"with it; refusing to build a pool and a fresh disk here, which would boot the guest as if "+
				"new and discard everything it wrote. Delete and recreate the guest", p, cfg.guestKey)
		}
	}
	return nil
}

func poolFiles(root string) (data, meta string) {
	dir := filepath.Join(root, "thinpool")
	return filepath.Join(dir, "data.img"), filepath.Join(dir, "meta.img")
}

// fileExists answers only for a file that is there or is not. Any other error
// is returned: an unreadable pool is not an absent one.
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// openNode brings up this node's pool — attaching its backing files and
// reopening its metadata after a reboot, or creating it on first use.
func openNode(ctx context.Context, cfg config) (*thinpool.Materializer, error) {
	dir := filepath.Join(cfg.root, "thinpool")
	dataPath, metaPath := poolFiles(cfg.root)

	// --pool-data-bytes sizes a pool when it is first CREATED. Once the backing
	// file exists, its own size is the truth. Otherwise raising the configured
	// size would make every later start on this node fail — the file would be
	// "smaller than configured" — and a guest already running here could not
	// restart. Growing a pool is a deliberate act, not a side effect of a
	// setting.
	dataBytes := existingSize(dataPath, cfg.poolDataBytes)
	metaBytes := existingSize(metaPath, thinpool.MetadataSectorsFor(dataBytes)*512)
	data := thinpool.Backing{File: dataPath, FileBytes: dataBytes}
	meta := thinpool.Backing{File: metaPath, FileBytes: metaBytes}

	run := thinpool.ExecRunner()
	dataDev, err := data.Resolve(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("pool data device: %w", err)
	}
	metaDev, err := meta.Resolve(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("pool metadata device: %w", err)
	}
	m := &thinpool.Manager{Pool: cfg.pool, DataDev: dataDev, MetaDev: metaDev}
	if err := m.EnsurePool(ctx, dataBytes/512); err != nil {
		// device-mapper asks the kernel to load dm-thin-pool on first use, and
		// that normally just works. When it does not — a node that forbids
		// module loading — dmsetup reports only a bare ioctl failure, so say
		// what to check rather than leave the operator with that.
		return nil, fmt.Errorf("creating or opening thin pool %s: %w (if the kernel could not load the "+
			"dm-thin-pool target, load it on the node with: modprobe dm-thin-pool)", cfg.pool, err)
	}
	return &thinpool.Materializer{
		M:       m,
		Reg:     thinpool.NewRegistry(thinpool.DefaultRegistryPath(cfg.root)),
		LockDir: filepath.Join(dir, "locks"),
		Log:     func(s string) { fmt.Fprintln(os.Stderr, "basedisk-materialize:", s) },
	}, nil
}

func fileSize(path string) (uint64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("the image: %w", err)
	}
	if st.Size() == 0 {
		return 0, errors.New("the image is empty")
	}
	return uint64(st.Size()), nil
}

// existingSize returns the size of path if it exists, else fallback.
func existingSize(path string, fallback uint64) uint64 {
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		return uint64(st.Size())
	}
	return fallback
}
