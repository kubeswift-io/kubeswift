package thinpool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

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
	// Used records when each base was last built from, RFC3339 with nanoseconds.
	//
	// A base is a cache: keeping one costs pool space, dropping one costs the
	// time to write it again. When the pool has no room for a new base, the one
	// least recently built from is the cheapest to lose — so this is what
	// decides the order. A base with no entry here has never been used since
	// the node learned to record it, and goes first.
	Used map[string]string `json:"used,omitempty"`
	// Created records which guests' snapshots were successfully created —
	// the guest-side twin of Ready.
	//
	// AllocateGuest persists a guest's id BEFORE create_snap runs (the id must
	// be durable before the device exists, or a crash could orphan it). Without
	// this second phase, a create_snap that failed, or a process that died
	// between the two, left the guest "known" with no device behind the id:
	// every later attempt took the reactivate path and failed with "thin device
	// N does not exist in pool", forever, until the guest was deleted. An
	// allocated-but-not-created guest never received its disk, so it holds no
	// data and is safe to create again; a created guest whose device is gone
	// lost real data and must still fail loudly.
	//
	// No omitempty: a registry written before this field existed has no
	// "created" key at all, and load() treats that — and only that — as legacy,
	// marking every guest it names as created (they were, or stranded as before).
	Created map[string]bool `json:"created"`
	// Derived records, for each guest, the device id of the base its snapshot
	// was taken from.
	//
	// Eviction needs it. A base is a cache, and evicting one is meant to buy
	// room, but dm-thin frees only the blocks nothing else references: a base
	// that live guests were snapshotted from shares nearly all its blocks with
	// them, so deleting it frees almost nothing and still costs the next guest
	// of that image a full rebuild. Least-recently-used alone cannot see that,
	// and an old base every running guest shares went first.
	//
	// The base's device id, not its digest: ids are never reused, while a
	// digest whose base was evicted and rebuilt names a new device that the
	// older guests share nothing with. Guests created before this field
	// existed have no entry and are not counted (the node cannot tell which
	// base they came from), which is how eviction behaved before.
	Derived map[string]uint32 `json:"derived,omitempty"`
	// Reserved holds the pool space promised to base builds still writing
	// (see Reserve). Checking free space is not enough on its own: two builds
	// of different images could each see room for itself, both write, and
	// together fill the pool -- which stalls, then fails, every guest on the
	// node.
	Reserved map[string]reservation `json:"reserved,omitempty"`
}

// reservation is pool space a base build has claimed and not yet released.
type reservation struct {
	Bytes uint64 `json:"bytes"`
	// Since is RFC3339 with nanoseconds. A build that died without releasing
	// its claim leaves it behind; it lapses after reservationTTL.
	Since string `json:"since"`
}

// reservationTTL is how long an unreleased reservation holds space. Well past
// the longest base write, so it only ever reclaims a crashed build's claim.
const reservationTTL = 6 * time.Hour

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
		delete(st.Created, key)
		delete(st.Derived, key)
		return true, nil
	})
}

// GuestCreated reports whether key's snapshot was successfully created (see
// registryState.Created). A guest that is allocated but not created never
// received its disk.
func (r *Registry) GuestCreated(key string) (bool, error) {
	var created bool
	err := r.withLock(func(st *registryState) (bool, error) {
		created = st.Created[key]
		return false, nil
	})
	return created, err
}

// MarkGuestCreated records that key's snapshot exists, taken from the base
// with device id baseID (see registryState.Derived). Call it only after
// create_snap has succeeded.
func (r *Registry) MarkGuestCreated(key string, baseID uint32) error {
	return r.withLock(func(st *registryState) (bool, error) {
		if _, ok := st.Guests[key]; !ok {
			return false, fmt.Errorf("marking guest %s created: no id allocated for it", key)
		}
		if st.Created[key] && st.Derived[key] == baseID {
			return false, nil
		}
		st.Created[key] = true
		if st.Derived == nil {
			st.Derived = map[string]uint32{}
		}
		st.Derived[key] = baseID
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
		delete(st.Used, digest)
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

// Reserve claims bytes of pool space for key's build if the pool has them
// free beyond what other builds hold, and reports the space available to key
// either way. free is read under the registry lock, so two builds cannot both
// pass on the same free space. A claim key already held is replaced.
func (r *Registry) Reserve(key string, bytes uint64, free func() (uint64, error)) (ok bool, available uint64, err error) {
	err = r.withLock(func(st *registryState) (bool, error) {
		f, err := free()
		if err != nil {
			return false, err
		}
		now := time.Now().UTC()
		changed := false
		var held uint64
		for k, res := range st.Reserved {
			if k == key {
				continue
			}
			if since, err := time.Parse(time.RFC3339Nano, res.Since); err != nil || now.Sub(since) > reservationTTL {
				delete(st.Reserved, k)
				changed = true
				continue
			}
			held += res.Bytes
		}
		if f > held {
			available = f - held
		}
		if available < bytes {
			return changed, nil
		}
		if st.Reserved == nil {
			st.Reserved = map[string]reservation{}
		}
		st.Reserved[key] = reservation{Bytes: bytes, Since: now.Format(time.RFC3339Nano)}
		ok = true
		return true, nil
	})
	return ok, available, err
}

// Unreserve releases key's claim, once its build has written what it will.
func (r *Registry) Unreserve(key string) error {
	return r.withLock(func(st *registryState) (bool, error) {
		if _, ok := st.Reserved[key]; !ok {
			return false, nil
		}
		delete(st.Reserved, key)
		return true, nil
	})
}

// Bases returns the digests this node holds, sorted, for eviction decisions.
// TouchBase records that a base has just been used.
func (r *Registry) TouchBase(digest string) error {
	return r.withLock(func(st *registryState) (bool, error) {
		if _, ok := st.Bases[digest]; !ok {
			return false, nil
		}
		if st.Used == nil {
			st.Used = map[string]string{}
		}
		st.Used[digest] = time.Now().UTC().Format(time.RFC3339Nano)
		return true, nil
	})
}

// BasesByLeastRecentUse returns every base, least recently used first. A base
// with no recorded use sorts first: nothing is known to have wanted it.
func (r *Registry) BasesByLeastRecentUse() ([]string, error) {
	var out []string
	err := r.withLock(func(st *registryState) (bool, error) {
		all := make([]string, 0, len(st.Bases))
		for d := range st.Bases {
			all = append(all, d)
		}
		out = lruOrder(st, all)
		return false, nil
	})
	return out, err
}

// EvictionOrder returns every base in the order to evict them: those no
// guest on this node was snapshotted from first, then those live guests
// share, least recently used first within each. shared counts the second
// group.
//
// Evicting a shared base still happens when nothing else frees enough, since
// it may free the blocks its guests have all overwritten, but it is the last
// resort: it frees little and costs a rebuild (see registryState.Derived).
func (r *Registry) EvictionOrder() (order []string, shared int, err error) {
	err = r.withLock(func(st *registryState) (bool, error) {
		inUse := map[uint32]bool{}
		for g, base := range st.Derived {
			if _, live := st.Guests[g]; live {
				inUse[base] = true
			}
		}
		var free, held []string
		for d, id := range st.Bases {
			if inUse[id] {
				held = append(held, d)
			} else {
				free = append(free, d)
			}
		}
		order = append(lruOrder(st, free), lruOrder(st, held)...)
		shared = len(held)
		return false, nil
	})
	return order, shared, err
}

// lruOrder sorts digests least recently used first; one with no recorded use
// sorts first.
func lruOrder(st *registryState, digests []string) []string {
	out := append([]string(nil), digests...)
	when := map[string]time.Time{}
	for _, d := range out {
		if t, err := time.Parse(time.RFC3339, st.Used[d]); err == nil {
			when[d] = t
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := when[out[i]], when[out[j]]
		if a.Equal(b) {
			return out[i] < out[j] // stable, and deterministic in tests
		}
		return a.Before(b)
	})
	return out
}

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
	// Created is deliberately left nil here: json.Unmarshal leaves a field
	// untouched when its key is absent, and an absent "created" key is how a
	// legacy registry is recognised below. A brand-new registry gets it here.
	st := &registryState{NextID: firstDeviceID, Bases: map[string]uint32{}, Guests: map[string]uint32{}, Ready: map[string]bool{}}
	data, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		st.Created = map[string]bool{}
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
	if st.Created == nil {
		// Absent key: a registry from before guests had a created phase. Every
		// guest it names went through the old single-phase path, so treat them
		// as created — reactivation keeps its previous behaviour for them.
		st.Created = make(map[string]bool, len(st.Guests))
		for k := range st.Guests {
			st.Created[k] = true
		}
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
