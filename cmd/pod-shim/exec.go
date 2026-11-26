package main

// podExec runs a command in the stand-in's microVM: what `kubectl exec
// <stand-in> -- sh` lands on. It authenticates to the relay in the
// controller-manager with the stand-in's own token, which the relay maps to
// this stand-in's microVM and no other; the relay builds the guest agent's
// request and then carries the agent's frames (internal/guestagent) both ways.
// It never runs anything locally: without a microVM it fails.

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"

	"github.com/kubeswift-io/kubeswift/internal/guestagent"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

const (
	execDialTimeout = 15 * time.Second
	// exitShimError is the exit code when the command never ran.
	exitShimError = 126
)

func podExec(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "kubeswift pod mode: no command")
		return 2
	}
	code, err := runExec(argv, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubeswift pod mode:", err)
		return exitShimError
	}
	return code
}

// runExec runs argv in the microVM, wired to the given stdio; the exit code is
// the command's.
func runExec(argv []string, stdin *os.File, stdout, stderr io.Writer) (int, error) {
	rel, tlsCfg, err := readRelay()
	if err != nil {
		return 0, err
	}
	token, err := readToken()
	if err != nil {
		return 0, fmt.Errorf("relay token: %w", err)
	}
	req := podmode.ExecRequest{Argv: argv}
	tty := term.IsTerminal(int(stdin.Fd()))
	if tty {
		req.TTY = true
		if cols, rows, err := term.GetSize(int(stdin.Fd())); err == nil {
			req.Rows, req.Cols = uint16(rows), uint16(cols)
		}
	}
	conn, err := dialRelayWS(rel, tlsCfg, token, req)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if tty {
		if old, err := term.MakeRaw(int(stdin.Fd())); err == nil {
			defer func() { _ = term.Restore(int(stdin.Fd()), old) }()
		}
	}
	fw := guestagent.NewFrameWriter(&wsWriter{c: conn})
	if tty {
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				if cols, rows, err := term.GetSize(int(stdin.Fd())); err == nil {
					_ = fw.Write(guestagent.FrameResize, guestagent.ResizePayload(uint16(rows), uint16(cols)))
				}
			}
		}()
	}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if fw.Write(guestagent.FrameStdin, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				_ = fw.Write(guestagent.FrameStdinClose, nil)
				return
			}
		}
	}()
	return readFrames(&wsReader{c: conn}, stdout, stderr)
}

// readFrames copies the agent's output until its exit frame.
func readFrames(r io.Reader, stdout, stderr io.Writer) (int, error) {
	for {
		typ, payload, err := guestagent.ReadFrame(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, errors.New("the microVM closed the session without an exit code")
			}
			return 0, err
		}
		switch typ {
		case guestagent.FrameStdout:
			_, _ = stdout.Write(payload)
		case guestagent.FrameStderr:
			_, _ = stderr.Write(payload)
		case guestagent.FrameExit:
			return guestagent.DecodeExitCode(payload), nil
		case guestagent.FrameError:
			return 0, fmt.Errorf("the microVM refused the command: %s", payload)
		}
	}
}

// dialRelayWS opens the exec relay. A refusal before the upgrade is returned
// with the relay's message.
func dialRelayWS(rel podmode.Relay, tlsCfg *tls.Config, token string, req podmode.ExecRequest) (*websocket.Conn, error) {
	body, _ := json.Marshal(req)
	d := websocket.Dialer{
		TLSClientConfig:  tlsCfg,
		HandshakeTimeout: execDialTimeout,
		NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var nd net.Dialer
			return nd.DialContext(ctx, network, rel.Addr)
		},
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set(podmode.ExecRequestHeader, base64.RawURLEncoding.EncodeToString(body))
	conn, resp, err := d.Dial("wss://"+rel.ServerName+podmode.RelayExecPath, h)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			if m := strings.TrimSpace(string(msg)); m != "" {
				return nil, errors.New(m)
			}
			return nil, fmt.Errorf("relay: %s", resp.Status)
		}
		return nil, fmt.Errorf("relay: %w", err)
	}
	return conn, nil
}

// wsWriter sends each Write as one binary message. FrameWriter serializes
// callers.
type wsWriter struct{ c *websocket.Conn }

func (w *wsWriter) Write(p []byte) (int, error) {
	if err := w.c.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// wsReader reads the binary messages as one byte stream.
type wsReader struct {
	c   *websocket.Conn
	cur io.Reader
}

func (r *wsReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			typ, rd, err := r.c.NextReader()
			if err != nil {
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					return 0, io.EOF
				}
				return 0, err
			}
			if typ != websocket.BinaryMessage {
				continue
			}
			r.cur = rd
		}
		n, err := r.cur.Read(p)
		if errors.Is(err, io.EOF) {
			r.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}
