package gateway

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"k8s.io/klog/v2"
)

// captureKlog routes klog to a buffer for the test.
func captureKlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	_ = fs.Set("logtostderr", "false")
	_ = fs.Set("alsologtostderr", "false")
	var buf bytes.Buffer
	klog.SetOutput(&buf)
	t.Cleanup(func() {
		klog.Flush()
		_ = fs.Set("logtostderr", "true")
	})
	return &buf
}

// A console into a privileged launcher, or a command run in a sandbox, must
// leave a record naming who did it and where: these routes bypass the Connect
// audit interceptor.
func TestAuditSession_RecordsWhoWhereAndWhat(t *testing.T) {
	buf := captureKlog(t)
	done := auditSession("sandbox-exec", Identity{User: "alice@example.com"}, "edge-1", "team-a", "sb1",
		"command", auditedCommand("cat /etc/shadow"))
	done()
	klog.Flush()
	out := buf.String()
	for _, want := range []string{
		`"ws session opened"`, `"ws session closed"`, `route="sandbox-exec"`, `user="alice@example.com"`,
		`cluster="edge-1"`, `namespace="team-a"`, `name="sb1"`, `command="cat /etc/shadow"`, `durationMs=`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit output lacks %s:\n%s", want, out)
		}
	}
}

func TestAuditSession_NamesTheGatewayCredentialInInsecureMode(t *testing.T) {
	buf := captureKlog(t)
	auditSession("console", Identity{}, "c", "ns", "g")()
	klog.Flush()
	if !strings.Contains(buf.String(), `user="<gateway-credential>"`) {
		t.Errorf("insecure-mode session not attributed:\n%s", buf.String())
	}
}

func TestAuditedCommand_IsBounded(t *testing.T) {
	long := strings.Repeat("x", 2*maxAuditedCommand)
	if got := auditedCommand(long); len(got) > maxAuditedCommand+len("...(truncated)") {
		t.Errorf("audited command is %d bytes", len(got))
	}
	if got := auditedCommand("ls"); got != "ls" {
		t.Errorf("short command changed: %q", got)
	}
}
