package thinpool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

// firstDeviceID is where allocation starts. Device id 0 is legal but reserving
// it makes "unset" and "the first base" distinguishable in logs and status,
// which matters when the value shows up in an error about a pool.
const firstDeviceID uint32 = 1

// Registry maps image digests and guests to the dm-thin device ids holding
// their data, persisted on the node.
//
// dm-thin identifies devices inside a pool by a small integer, chosen by the
// caller, and remembers nothing about what a given id was for. Losing this
// mapping does not corrupt the pool — it orphans it: the blocks stay allocated,
// nothing points at them, and a guest that had a disk gets a fresh empty one.
// So the file is written atomically and every mutation is taken under a lock.
//
// It is NOT authoritative about which ids exist. The pool's own metadata is.
// See Allocate for why that distinction is load-bearing.
type Registry struct {
	path string
}

type registryState struct {
	// NextID is the lowest id never handed out. It only ever moves forward:
	// reusing a freed id risks handing a new guest the blocks of a deleted one
	// if a delete did not complete.
	NextID uint32 `json:"nextID"`
	// Bases maps an image digest to the thin device holding its base.
	Bases map[string]uint32 `json:"bases,omitempty"`
	// Guests maps "namespace/name" to the thin snapshot serving that guest.
	Guests map[string]uint32 `json:"guests,omitempty"`
	// Ready records which bases are FULLY written.
	//
	// Allocated and populated are different states, and the gap between them
	// is where a guest would boot from half an image: a base is allocated,
	// its writer is part-way through a multi-gigabyte copy, and a second guest
	// on the same digest sees the id and snapshots it. Snapshots are only ever
	// taken from a base marked here, and a base is only marked after its
	// writer has synced.
	Ready map[string]bool `json:"ready,omitempty"`
}

// NewRegistry returns a registry stored at path. The file is created on first
// mutation.
func NewRegistry(path string) *Registry { return &Registry{path: path} }

// DefaultRegistryPath is where a node keeps its pool bookkeeping.
func DefaultRegistryPath(root string) string {
	return filepath.Join(root, "thinpool", "registry.json")
}

// BaseID returns the device id holding the base for digest, and whether it is
// known.
func (r *Registry) BaseID(digest string) (uint32, bool, error) {
	var id uint32
	var ok bool
	err := r.withLock(func(st *registryState) (bool, error) {
		id, ok = st.Bases[digest]
		return false, nil
	})
	return id, ok, err
}

// GuestID returns the device id serving a guest, and whether it is known.
func (r *Registry) GuestID(key string) (uint32, bool, error) {
	var id uint32
	var ok bool
	err := r.withLock(func(st *registryState) (bool, error) {
		id, ok = st.Guests[key]
		return false, nil
	})
	return id, ok, err
}

// AllocateBase returns the device id for digest, allocating one if this node
// has not seen it. The bool reports whether the id is new — i.e. whether the
// caller must still populate the base.
func (r *Registry) AllocateBase(digest string) (uint32, bool, error) {
	return r.allocate(func(st *registryState) map[string]uint32 { return st.Bases }, digest,
		func(st *registryState, m map[string]uint32) { st.Bases = m })
}

// AllocateGuest returns the device id for a guest, allocating one if absent.
func (r *Registry) AllocateGuest(key string) (uint32, bool, error) {
	return r.allocate(func(st *registryState) map[string]uint32 { return st.Guests }, key,
		func(st *registryState, m map[string]uint32) { st.Guests = m })
}

func (r *Registry) allocate(
	get func(*registryState) map[string]uint32,
	key string,
	set func(*registryState, map[string]uint32),
) (id uint32, fresh bool, err error) {
	if key == "" {
		return 0, false, fmt.Errorf("thinpool registry: empty key")
	}
	err = r.withLock(func(st *registryState) (bool, error) {
		m := get(st)
		if m == nil {
			m = map[string]uint32{}
		}
		if existing, ok := m[key]; ok {
			id, fresh = existing, false
			return false, nil
		}
		id, fresh = st.NextID, true
		st.NextID++
		m[key] = id
		set(st, m)
		return true, nil
	})
	return id, fresh, err
}

// Advance moves NextID past an id the pool already holds.
//
// The registry is not authoritative about which ids exist — the pool's metadata
// is — and the two can disagree: a registry restored from a backup, or lost and
// recreated, will hand out ids the pool is already using, and create_thin then
// fails with "File exists". That is recoverable and this is the recovery: skip
// the id and try the next. Without it a node in that state never creates
// another guest, with an error that points at device-mapper rather than at the
// bookkeeping.
func (r *Registry) Advance(past uint32) error {
	return r.withLock(func(st *registryState) (bool, error) {
		if st.NextID > past {
			return false, nil
		}
		st.NextID = past + 1
		return true, nil
	})
}

// Forget drops a guest's mapping, after its thin device has been deleted.
//
// It does NOT lower NextID. A freed id is never reused: if the delete did not
// actually complete, reusing the id would hand a new guest the previous one's
// blocks — the same class of exposure as skipping block zeroing, arrived at
// from the other side.
func (r *Registry) Forget(key string) error {
	return r.withLock(func(st *registryState) (bool, error) {
		if _, ok := st.Guests[key]; !ok {
			return false, nil
		}
		delete(st.Guests, key)
		return true, nil
	})
}

// ForgetBase drops a digest's mapping, after its base device has been deleted.
func (r *Registry) ForgetBase(digest string) error {
	return r.withLock(func(st *registryState) (bool, error) {
		if _, ok := st.Bases[digest]; !ok {
			return false, nil
		}
		delete(st.Bases, digest)
		// Readiness goes with it. Leaving it behind would let a base allocated
		// later for the same digest be snapshotted before it was written.
		delete(st.Ready, digest)
		return true, nil
	})
}

// BaseReady reports whether digest's base has been completely written.
func (r *Registry) BaseReady(digest string) (bool, error) {
	var ready bool
	err := r.withLock(func(st *registryState) (bool, error) {
		ready = st.Ready[digest]
		return false, nil
	})
	return ready, err
}

// MarkBaseReady records that digest's base is completely written and synced.
// Call it only after the writer has synced, never before: a base marked ready
// is one guests will be snapshotted from.
func (r *Registry) MarkBaseReady(digest string) error {
	return r.withLock(func(st *registryState) (bool, error) {
		if _, ok := st.Bases[digest]; !ok {
			return false, fmt.Errorf("marking %s ready: no base allocated for it", digest)
		}
		if st.Ready[digest] {
			return false, nil
		}
		st.Ready[digest] = true
		return true, nil
	})
}

// Bases returns the digests this node holds, sorted, for eviction decisions.
func (r *Registry) Bases() ([]string, error) {
	var out []string
	err := r.withLock(func(st *registryState) (bool, error) {
		for d := range st.Bases {
			out = append(out, d)
		}
		sort.Strings(out)
		return false, nil
	})
	return out, err
}

// withLock runs mutate against the on-disk state under an exclusive lock,
// writing back only when it reports a change.
//
// The lock is held on a separate file rather than on registry.json, because the
// atomic write replaces registry.json by rename — a lock held on the old inode
// would protect nothing once it was swapped out. Several guests can start at
// once on a node, so this is a real race, not a theoretical one.
func (r *Registry) withLock(mutate func(*registryState) (bool, error)) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(r.path), err)
	}
	lock, err := os.OpenFile(r.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening the registry lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("locking the registry: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()

	st, err := r.load()
	if err != nil {
		return err
	}
	changed, err := mutate(st)
	if err != nil || !changed {
		return err
	}
	return r.store(st)
}

func (r *Registry) load() (*registryState, error) {
	st := &registryState{NextID: firstDeviceID, Bases: map[string]uint32{}, Guests: map[string]uint32{}, Ready: map[string]bool{}}
	data, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", r.path, err)
	}
	if err := json.Unmarshal(data, st); err != nil {
		// Refuse rather than start fresh. An unreadable registry beside a pool
		// full of guests is a state to investigate: continuing would allocate
		// from id 1 over devices that already exist, and "recovering" by
		// forgetting which device is whose is how a guest silently gets the
		// wrong disk.
		return nil, fmt.Errorf("registry %s is corrupt (%w); refusing to allocate over an existing pool", r.path, err)
	}
	if st.NextID < firstDeviceID {
		st.NextID = firstDeviceID
	}
	if st.Bases == nil {
		st.Bases = map[string]uint32{}
	}
	if st.Guests == nil {
		st.Guests = map[string]uint32{}
	}
	if st.Ready == nil {
		st.Ready = map[string]bool{}
	}
	return st, nil
}

// store writes atomically: a crash leaves either the old file or the new one,
// never a truncated one. The directory is fsynced too, or the rename itself can
// be lost while the data survives.
func (r *Registry) store(st *registryState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.path)
	tmp, err := os.CreateTemp(dir, "registry-*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temp registry in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the registry: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing the registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		return fmt.Errorf("replacing the registry: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil // the rename happened; an unopenable dir is not worth failing on
	}
	defer d.Close()
	return d.Sync()
}
