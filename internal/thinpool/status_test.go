package thinpool

import "testing"

// Every line here is real `dmsetup status` output captured from a live pool
// while the design decisions were being measured, not invented for the test.
func TestParseStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want Status
	}{
		{
			name: "fresh pool",
			line: "0 131072 thin-pool 0 214/2048 0/1024 - rw discard_passdown queue_if_no_space - 512",
			want: Status{
				UsedMetadata: 214, TotalMetadata: 2048,
				UsedData: 0, TotalData: 1024,
				Mode: ModeReadWrite, DiscardPassdown: true, QueueIfNoSpace: true,
			},
		},
		{
			name: "pool in use",
			line: "0 8388608 thin-pool 0 315/1024 49152/65536 - rw discard_passdown queue_if_no_space - 256",
			want: Status{
				UsedMetadata: 315, TotalMetadata: 1024,
				UsedData: 49152, TotalData: 65536,
				Mode: ModeReadWrite, DiscardPassdown: true, QueueIfNoSpace: true,
			},
		},
		{
			// Captured at the moment metadata filled: the pool flips straight
			// to ro, with no intermediate state and no warning.
			name: "metadata exhausted",
			line: "0 8388608 thin-pool 0 1024/1024 4977/65536 - ro discard_passdown queue_if_no_space - 256",
			want: Status{
				UsedMetadata: 1024, TotalMetadata: 1024,
				UsedData: 4977, TotalData: 65536,
				Mode: ModeReadOnly, DiscardPassdown: true, QueueIfNoSpace: true,
			},
		},
		{
			name: "data exhausted, still queueing",
			line: "0 524288 thin-pool 0 120/2048 1024/1024 - out_of_data_space discard_passdown queue_if_no_space - 512",
			want: Status{
				UsedMetadata: 120, TotalMetadata: 2048,
				UsedData: 1024, TotalData: 1024,
				Mode: ModeOutOfDataSpace, DiscardPassdown: true, QueueIfNoSpace: true,
			},
		},
		{
			name: "needs_check set",
			line: "0 524288 thin-pool 0 120/2048 10/1024 - ro no_discard_passdown error_if_no_space needs_check 512",
			want: Status{
				UsedMetadata: 120, TotalMetadata: 2048,
				UsedData: 10, TotalData: 1024,
				Mode: ModeReadOnly, DiscardPassdown: false, QueueIfNoSpace: false, NeedsCheck: true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseStatus(tc.line)
			if err != nil {
				t.Fatalf("ParseStatus: %v", err)
			}
			if got != tc.want {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// A status this code cannot read must be an error, never a default. Reporting
// an unparsed pool as rw would say "healthy" about a node where every guest is
// taking EIO.
func TestParseStatus_RefusesToGuess(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"empty", ""},
		{"truncated", "0 131072 thin-pool 0 214/2048"},
		{"a different target", "0 65536 linear /dev/sda 0"},
		{"no mode word", "0 131072 thin-pool 0 214/2048 0/1024 - discard_passdown queue_if_no_space - 512"},
		{"unparseable ratio", "0 131072 thin-pool 0 abc/2048 0/1024 - rw discard_passdown queue_if_no_space - 512"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseStatus(tc.line)
			if err == nil {
				t.Fatalf("ParseStatus(%q) = %+v, want an error", tc.line, got)
			}
			if got.Mode == ModeReadWrite {
				t.Errorf("a failed parse reported Mode=rw, which reads as healthy")
			}
		})
	}
}

// Read-only is the mode with no warning and no grace period, so it must never
// pass a health check.
func TestHealthy(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Status
		want bool
	}{
		{"rw", Status{Mode: ModeReadWrite}, true},
		{"rw but needs_check", Status{Mode: ModeReadWrite, NeedsCheck: true}, false},
		{"out of data space", Status{Mode: ModeOutOfDataSpace}, false},
		{"read only", Status{Mode: ModeReadOnly}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Healthy(); got != tc.want {
				t.Errorf("Healthy() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUsedFractions(t *testing.T) {
	s := Status{UsedMetadata: 512, TotalMetadata: 2048, UsedData: 3, TotalData: 4}
	if got := s.MetadataUsedFraction(); got != 0.25 {
		t.Errorf("metadata fraction = %v, want 0.25", got)
	}
	if got := s.DataUsedFraction(); got != 0.75 {
		t.Errorf("data fraction = %v, want 0.75", got)
	}
	// A pool reporting a zero total must not divide by it and must not look
	// like 100% used.
	var zero Status
	if zero.MetadataUsedFraction() != 0 || zero.DataUsedFraction() != 0 {
		t.Error("zero totals should yield 0, not NaN or 1")
	}
}
