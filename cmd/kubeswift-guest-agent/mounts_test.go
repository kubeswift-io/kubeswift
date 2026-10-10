package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordBinds replaces bindReadOnly for the test and returns what was bound.
func recordBinds(t *testing.T) *[][2]string {
	t.Helper()
	var got [][2]string
	old := bindReadOnly
	bindReadOnly = func(source, target string) error {
		got = append(got, [2]string{source, target})
		return nil
	}
	t.Cleanup(func() { bindReadOnly = old })
	return &got
}

func stage(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.Mkdir(filepath.Join(dir, n), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestMountExecMounts_BindsEachStagedArtifact(t *testing.T) {
	binds := recordBinds(t)
	root, st := t.TempDir(), stage(t, "app", "data")
	err := mountExecMounts(root, st, []ExecMount{{Name: "app", Path: "/opt/app"}, {Name: "data", Path: "/srv/data"}})
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{filepath.Join(st, "app"), filepath.Join(root, "opt/app")},
		{filepath.Join(st, "data"), filepath.Join(root, "srv/data")},
	}
	if len(*binds) != 2 || (*binds)[0] != want[0] || (*binds)[1] != want[1] {
		t.Fatalf("binds = %v, want %v", *binds, want)
	}
	if fi, err := os.Stat(filepath.Join(root, "opt/app")); err != nil || !fi.IsDir() {
		t.Errorf("mount point not created: %v", err)
	}
}

// One bad entry mounts nothing, and says which.
func TestMountExecMounts_RefusesBeforeMountingAnything(t *testing.T) {
	cases := map[string]ExecMount{
		"is not a DNS label":           {Name: "../etc", Path: "/x"},
		"is not a clean absolute path": {Name: "app", Path: "/opt/../etc"},
		"outside /proc":                {Name: "app", Path: "/proc/self"},
		"relative":                     {Name: "app", Path: "opt/app"},
		"root":                         {Name: "app", Path: "/"},
		"is not staged":                {Name: "missing", Path: "/m"},
	}
	for what, bad := range cases {
		binds := recordBinds(t)
		err := mountExecMounts(t.TempDir(), stage(t, "app"), []ExecMount{{Name: "app", Path: "/ok"}, bad})
		if err == nil {
			t.Errorf("%s: accepted %+v", what, bad)
			continue
		}
		if len(*binds) != 0 {
			t.Errorf("%s: mounted %v before refusing", what, *binds)
		}
		if what == "is not staged" && !strings.Contains(err.Error(), "artifact missing is not staged") {
			t.Errorf("error = %v", err)
		}
	}
	binds := recordBinds(t)
	if err := mountExecMounts(t.TempDir(), stage(t, "a", "b"), []ExecMount{{Name: "a", Path: "/x"}, {Name: "b", Path: "/x"}}); err == nil || len(*binds) != 0 {
		t.Errorf("duplicate path: err=%v binds=%v", err, *binds)
	}
}

// The exec op mounts before it runs anything.
func TestExec_MountsArrive(t *testing.T) {
	binds := recordBinds(t)
	root, st := t.TempDir(), stage(t, "app")
	h := &handler{sys: realSystem{}, execRoot: root, stageDir: st}
	raw, _ := json.Marshal(Request{V: 1, Op: "exec", Argv: []string{"/no/such/binary"}, Mounts: []ExecMount{{Name: "app", Path: "/opt/app"}}})
	var resp Response
	_ = json.Unmarshal(h.handle(raw), &resp)
	if len(*binds) != 1 {
		t.Fatalf("binds = %v (resp %+v)", *binds, resp)
	}
	// An identity guest (no exec root) refuses exec outright: nothing mounted.
	h = &handler{sys: realSystem{}, stageDir: st}
	_ = json.Unmarshal(h.handle(raw), &resp)
	if resp.OK || len(*binds) != 1 {
		t.Errorf("no exec root: %+v, binds %v", resp, *binds)
	}
}
