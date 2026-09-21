// Package thinpool drives the device-mapper thin-provisioning target that backs
// shared base root disks: one pool per node, one thin volume per image digest
// holding the base, and one thin snapshot per guest sharing its blocks.
//
// Every parameter here is a decision recorded in docs/design/shared-base-root-disk.md
// with a measurement behind it, not a default copied from somewhere. The ones
// that bite if changed casually are called out where they are defined.
package thinpool

import (
	"fmt"
	"strconv"
	"strings"
)

// Mode is the pool's operating mode, as reported by `dmsetup status`.
//
// These are not severity levels and must not be collapsed into one: they fail
// differently, they warn differently, and only one of them gives an operator
// any time to react.
type Mode string

const (
	// ModeReadWrite is the healthy state.
	ModeReadWrite Mode = "rw"

	// ModeOutOfDataSpace means the data device is full. Writes QUEUE rather
	// than fail, and the pool escalates to erroring after no_space_timeout
	// (60 s measured). Growing the data device inside that window is a full
	// recovery with nothing lost, and the guest sees ENOSPC — not EIO — if the
	// window closes first.
	//
	// This is the one mode that announces itself before it bites, which is why
	// it is the alerting signal rather than ModeReadOnly.
	ModeOutOfDataSpace Mode = "out_of_data_space"

	// ModeReadOnly means the METADATA device is full (or the pool needs a
	// check). There is no queueing and no grace window: guests take EIO
	// immediately, and by the time this mode is visible every guest on the node
	// is already failing.
	//
	// Growing the metadata device and reloading the table recovers it online —
	// measured, no offline thin_repair — but nothing warns first. Metadata must
	// therefore be watched by HEADROOM, not by waiting for this mode.
	ModeReadOnly Mode = "ro"
)

// Status is a parsed `dmsetup status` line for a thin-pool target.
type Status struct {
	// TransactionID is the pool's metadata transaction counter.
	TransactionID uint64
	// UsedMetadata and TotalMetadata are in metadata blocks (4 KiB each).
	UsedMetadata, TotalMetadata uint64
	// UsedData and TotalData are in pool blocks (BlockSectors sectors each).
	UsedData, TotalData uint64
	// Mode is rw, out_of_data_space or ro.
	Mode Mode
	// DiscardPassdown reports whether discards reach the underlying device.
	// It is what makes a deleted guest's space actually come back, and it is
	// the property that chose dm-thin over dm-snapshot.
	DiscardPassdown bool
	// QueueIfNoSpace is true for queue_if_no_space, false for
	// error_if_no_space. The default is deliberate — see ModeOutOfDataSpace.
	QueueIfNoSpace bool
	// NeedsCheck is true when the kernel has flagged the metadata as needing
	// thin_check before the pool can be trusted read-write again.
	NeedsCheck bool
}

// MetadataUsedFraction returns metadata utilisation in [0,1].
//
// This is the number to alert on. Unlike data space there is no warning state
// to wait for: the pool goes straight from rw to ro.
func (s Status) MetadataUsedFraction() float64 {
	if s.TotalMetadata == 0 {
		return 0
	}
	return float64(s.UsedMetadata) / float64(s.TotalMetadata)
}

// DataUsedFraction returns data utilisation in [0,1].
func (s Status) DataUsedFraction() float64 {
	if s.TotalData == 0 {
		return 0
	}
	return float64(s.UsedData) / float64(s.TotalData)
}

// Healthy reports whether the pool is serving writes normally.
func (s Status) Healthy() bool { return s.Mode == ModeReadWrite && !s.NeedsCheck }

// ParseStatus parses one `dmsetup status <pool>` line.
//
// The kernel's format (Documentation/admin-guide/device-mapper/thin-provisioning.rst):
//
//	<start> <len> thin-pool <txn id> <used meta>/<total meta> <used data>/<total data> \
//	    <held metadata root> ro|rw|out_of_data_space [no_]discard_passdown \
//	    error_if_no_space|queue_if_no_space needs_check|- <metadata low watermark>
//
// Fields are appended over kernel versions, so this reads positionally only as
// far as it must and treats the flag words as a set. A trailing field this does
// not know about is not an error.
func ParseStatus(line string) (Status, error) {
	f := strings.Fields(strings.TrimSpace(line))
	if len(f) < 10 {
		return Status{}, fmt.Errorf("thin-pool status needs at least 10 fields, got %d: %q", len(f), line)
	}
	if f[2] != "thin-pool" {
		return Status{}, fmt.Errorf("not a thin-pool target (field 3 = %q): %q", f[2], line)
	}

	var s Status
	var err error
	if s.TransactionID, err = strconv.ParseUint(f[3], 10, 64); err != nil {
		return Status{}, fmt.Errorf("transaction id %q: %w", f[3], err)
	}
	if s.UsedMetadata, s.TotalMetadata, err = parseRatio(f[4]); err != nil {
		return Status{}, fmt.Errorf("metadata usage: %w", err)
	}
	if s.UsedData, s.TotalData, err = parseRatio(f[5]); err != nil {
		return Status{}, fmt.Errorf("data usage: %w", err)
	}

	// f[6] is the held metadata root ("-" unless a metadata snapshot is
	// reserved). Not needed yet; thin_delta would use it.

	// The mode word and the flag words are read as a set rather than by index,
	// because their positions have moved between kernel releases and a
	// misparsed MODE is the difference between "healthy" and "every guest on
	// this node is taking EIO".
	sawMode := false
	for _, w := range f[7:] {
		switch w {
		case string(ModeReadWrite), string(ModeReadOnly), string(ModeOutOfDataSpace):
			s.Mode, sawMode = Mode(w), true
		case "discard_passdown":
			s.DiscardPassdown = true
		case "no_discard_passdown":
			s.DiscardPassdown = false
		case "queue_if_no_space":
			s.QueueIfNoSpace = true
		case "error_if_no_space":
			s.QueueIfNoSpace = false
		case "needs_check":
			s.NeedsCheck = true
		}
	}
	if !sawMode {
		// Refusing to guess is the point. Defaulting an unrecognised status to
		// rw would report a broken pool as healthy.
		return Status{}, fmt.Errorf("no pool mode (rw/ro/out_of_data_space) in status: %q", line)
	}
	return s, nil
}

// parseRatio splits "used/total".
func parseRatio(s string) (used, total uint64, err error) {
	a, b, ok := strings.Cut(s, "/")
	if !ok {
		return 0, 0, fmt.Errorf("want used/total, got %q", s)
	}
	if used, err = strconv.ParseUint(a, 10, 64); err != nil {
		return 0, 0, fmt.Errorf("used %q: %w", a, err)
	}
	if total, err = strconv.ParseUint(b, 10, 64); err != nil {
		return 0, 0, fmt.Errorf("total %q: %w", b, err)
	}
	return used, total, nil
}
