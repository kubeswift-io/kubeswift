package swiftsandbox

import (
	"os"
	"testing"
	"time"
)

// Tests reconcile against an in-process registry and expect a launch to
// complete in one pass. The production inline wait (250 ms) is long enough for
// that, but not on a loaded CI runner; resolver_test.go covers the wait itself.
func TestMain(m *testing.M) {
	sharedResolver.inlineWait = 30 * time.Second
	os.Exit(m.Run())
}
