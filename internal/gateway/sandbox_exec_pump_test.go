package gateway

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// A refused exec never reads stdin. The handshake write into the stdin pipe
// used to block forever; it must fail promptly with the refusal, and the
// stdout side must report it too.
func TestPumpExec_RefusedStreamUnblocksThePipes(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	refused := errors.New(`pods "sb" is forbidden: cannot create resource "pods/exec"`)
	go pumpExec(func() error { return refused }, inR, outW)

	wrote := make(chan error, 1)
	go func() {
		_, err := io.WriteString(inW, "CONNECT 1024\n")
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if !errors.Is(err, refused) {
			t.Errorf("write err = %v, want the refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stdin write is still blocked after the exec was refused (goroutine + connection leak)")
	}
	if _, err := bufio.NewReader(outR).ReadString('\n'); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("stdout read err = %v, want the refusal", err)
	}
}
