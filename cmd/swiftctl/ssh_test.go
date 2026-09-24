package main

import (
	"strings"
	"testing"
)

// The private key must never appear in the ssh exec command — it is staged over
// exec stdin into a temp file, and only the file PATH is referenced here. The
// command must also pass the user and host as separate quoted positional args
// so a hostile primaryIP (an unvalidated pod annotation) cannot inject shell.
func TestSSHExecCommand_NoKeyAndInjectionSafe(t *testing.T) {
	const keyPath = "/tmp/tmp.ABC123"
	// A host that would break an unquoted `user@host` splice.
	host := `10.0.0.5"; touch /pwned; echo "`
	cmd := sshExecCommand(keyPath, "kubeswift", host, nil)

	if len(cmd) < 4 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("unexpected command shape: %v", cmd)
	}
	script := cmd[2]

	// The host and user ride as positional args, not spliced into the script.
	if strings.Contains(script, host) {
		t.Error("host interpolated into the script text; must be a positional arg")
	}
	if strings.Contains(script, "touch /pwned") {
		t.Error("host injection reached the script")
	}
	// The script references only the key PATH via $1, never key material.
	if !strings.Contains(script, `-i "$1"`) {
		t.Errorf("script does not reference the key by positional arg: %q", script)
	}
	// Positional args: $0 label, $1 keyPath, $2 user, $3 host.
	if cmd[4] != keyPath || cmd[5] != "kubeswift" || cmd[6] != host {
		t.Errorf("positional args = %v, want [.. %q kubeswift %q]", cmd[4:], keyPath, host)
	}

	// No trailing remote command for an interactive shell.
	if len(cmd) != 7 {
		t.Errorf("interactive command should have 7 elements, got %d: %v", len(cmd), cmd)
	}

	// A non-interactive run appends the joined remote command as one arg.
	withCmd := sshExecCommand(keyPath, "kubeswift", "10.0.0.5", []string{"uptime", "&&", "id"})
	if got := withCmd[len(withCmd)-1]; got != "uptime && id" {
		t.Errorf("remote command arg = %q, want %q", got, "uptime && id")
	}
}
