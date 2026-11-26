package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kubeswift-io/kubeswift/internal/guestagent"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

// shimFiles points the shim at a relay annotation and token in a temp dir.
func shimFiles(t *testing.T, rel *podmode.Relay) {
	t.Helper()
	dir := t.TempDir()
	content := ""
	if rel != nil {
		b, _ := json.Marshal(rel)
		content = podmode.AnnotationRelay + "=" + strconv.Quote(string(b)) + "\n"
	}
	ann := filepath.Join(dir, "annotations")
	tok := filepath.Join(dir, "token")
	if err := os.WriteFile(ann, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tok, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldA, oldT := annotationsFile, tokenFile
	annotationsFile, tokenFile = ann, tok
	t.Cleanup(func() { annotationsFile, tokenFile = oldA, oldT })
}

// relayFor is the Relay of an httptest TLS server (its certificate is for
// example.com).
func relayFor(srv *httptest.Server) *podmode.Relay {
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return &podmode.Relay{Addr: srv.Listener.Addr().String(), ServerName: "example.com", CA: string(ca)}
}

func TestReadRelay(t *testing.T) {
	shimFiles(t, nil)
	if _, _, err := readRelay(); err != errNoRelay {
		t.Errorf("no annotation: %v", err)
	}
	shimFiles(t, &podmode.Relay{Addr: "10.0.0.1:443", ServerName: "x"})
	if _, _, err := readRelay(); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("no CA: %v", err)
	}
}

func TestExecThroughRelay(t *testing.T) {
	up := websocket.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != podmode.RelayExecPath || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		raw, _ := base64.RawURLEncoding.DecodeString(r.Header.Get(podmode.ExecRequestHeader))
		var er podmode.ExecRequest
		_ = json.Unmarshal(raw, &er)
		if strings.Join(er.Argv, " ") == "refuse me" {
			http.Error(w, "the microVM of tenant/x is not running yet", http.StatusServiceUnavailable)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		// Echo stdin back on stdout until stdin closes, then exit 7.
		var in bytes.Buffer
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			in.Write(data)
			for {
				typ, p, err := guestagent.ReadFrame(bytes.NewReader(in.Bytes()))
				if err != nil {
					break
				}
				in.Next(5 + len(p))
				var out bytes.Buffer
				switch typ {
				case guestagent.FrameStdin:
					_ = guestagent.WriteFrame(&out, guestagent.FrameStdout, p)
				case guestagent.FrameStdinClose:
					_ = guestagent.WriteFrame(&out, guestagent.FrameStderr, []byte(er.Argv[0]))
					_ = guestagent.WriteFrame(&out, guestagent.FrameExit, guestagent.ExitCode(7))
				}
				_ = c.WriteMessage(websocket.BinaryMessage, out.Bytes())
			}
		}
	}))
	defer srv.Close()
	shimFiles(t, relayFor(srv))

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = stdinW.Write([]byte("hello"))
		time.Sleep(50 * time.Millisecond)
		stdinW.Close()
	}()
	var stdout, stderr bytes.Buffer
	code, err := runExec([]string{"sh", "-c", "cat"}, stdinR, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || stdout.String() != "hello" || stderr.String() != "sh" {
		t.Errorf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}

	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	if _, err := runExec([]string{"refuse", "me"}, devnull, &stdout, &stderr); err == nil ||
		err.Error() != "the microVM of tenant/x is not running yet" {
		t.Errorf("refusal: %v", err)
	}
}

func TestCopyLogLinesSkipsWhatWasPrinted(t *testing.T) {
	var last time.Time
	var out bytes.Buffer
	in := "2026-11-26T10:00:00.1Z one\n2026-11-26T10:00:00.25Z two\n"
	if err := copyLogLines(strings.NewReader(in), &out, &last); err != nil {
		t.Fatal(err)
	}
	// A reconnect from the same second repeats both lines plus a new one.
	in += "2026-11-26T10:00:01Z three\nno timestamp\n"
	if err := copyLogLines(strings.NewReader(in), &out, &last); err != nil {
		t.Fatal(err)
	}
	if out.String() != "one\ntwo\nthree\nno timestamp\n" {
		t.Errorf("out %q", out.String())
	}
}
