package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The console comes first, then the workload output from its own file; a
// sandbox without workload output (the cold path) prints just the console.
func TestSandboxLogsCommand_PrintsConsoleThenWorkload(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "serial.sock.log"), []byte("boot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		out, err := exec.Command("sh", "-c", SandboxLogsCommand(dir, false)).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return string(out)
	}
	if got := run(); got != "boot\n" {
		t.Errorf("console only: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "workload.log"), []byte("result\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(); got != "boot\nresult\n" {
		t.Errorf("console + workload: %q", got)
	}
}

func TestSandboxLogsCommand_FailsWithoutAConsoleLog(t *testing.T) {
	if err := exec.Command("sh", "-c", SandboxLogsCommand(t.TempDir(), false)).Run(); err == nil {
		t.Error("no console log should be an error, as before")
	}
}
