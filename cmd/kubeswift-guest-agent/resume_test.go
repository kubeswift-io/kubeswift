package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func resumeReq(t *testing.T, h *handler, req map[string]any) Response {
	t.Helper()
	req["op"] = "resume-sync"
	raw, _ := json.Marshal(req)
	var resp Response
	if err := json.Unmarshal(h.handle(raw), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestResumeSync_ClockAndEntropy(t *testing.T) {
	f := newFakeSys()
	h := &handler{sys: f, version: "t"}
	host := time.Now().Add(10 * time.Minute) // the guest is 10 min behind
	seed := []byte("0123456789abcdef0123456789abcdef")
	resp := resumeReq(t, h, map[string]any{"now": host.UnixNano(), "entropy": seed})
	if !resp.OK || resp.SyncResult == nil {
		t.Fatalf("resp = %+v", resp)
	}
	if !f.clock.Equal(time.Unix(0, host.UnixNano())) {
		t.Errorf("clock set to %v, want %v", f.clock, host)
	}
	if string(f.entropy) != string(seed) {
		t.Errorf("entropy %q, want %q", f.entropy, seed)
	}
	if got := strings.Join(resp.Synced, ","); got != "clock,entropy" {
		t.Errorf("synced = %q", got)
	}
	if off := *resp.ClockOffsetMs; off > -9*60*1000 || off < -11*60*1000 {
		t.Errorf("clockOffsetMs = %d, want about -600000", off)
	}
}

func TestResumeSync_ClockOnly(t *testing.T) {
	f := newFakeSys()
	resp := resumeReq(t, &handler{sys: f}, map[string]any{"now": time.Now().UnixNano()})
	if !resp.OK || strings.Join(resp.Synced, ",") != "clock" || f.entropy != nil {
		t.Fatalf("resp = %+v entropy=%v", resp, f.entropy)
	}
}

func TestResumeSync_Refusals(t *testing.T) {
	for name, req := range map[string]map[string]any{
		"nothing":     {},
		"negative":    {"now": -1},
		"big entropy": {"entropy": make([]byte, maxResumeEntropy+1)},
	} {
		f := newFakeSys()
		resp := resumeReq(t, &handler{sys: f}, req)
		if resp.OK || resp.Error == "" {
			t.Errorf("%s: resp = %+v, want refused", name, resp)
		}
		if !f.clock.IsZero() || f.entropy != nil {
			t.Errorf("%s: side effect despite refusal", name)
		}
	}
}

func TestResumeSync_ClockError(t *testing.T) {
	f := newFakeSys()
	f.clockErr = errors.New("EPERM")
	resp := resumeReq(t, &handler{sys: f}, map[string]any{"now": time.Now().UnixNano(), "entropy": []byte("x")})
	if resp.OK || !strings.Contains(resp.Error, "EPERM") {
		t.Fatalf("resp = %+v", resp)
	}
	if f.entropy != nil {
		t.Error("entropy added after the clock step failed")
	}
}
