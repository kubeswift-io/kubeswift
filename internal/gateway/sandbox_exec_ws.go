package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/kubeswift-io/kubeswift/internal/guestagent"
	"k8s.io/klog/v2"
)

// agentVsockPort is the AF_VSOCK port the in-guest agent listens on (matches
// swiftletd's mux + swiftctl's agentVsockPort).
const agentVsockPort = 1024

// SandboxExecHandler runs an interactive command inside a running SwiftSandbox
// and bridges it to a browser WebSocket, mirroring `swiftctl sandbox exec -it`.
// It pod-execs `socat - UNIX-CONNECT:<vsock.sock>` in the launcher, speaks the
// agent's CONNECT handshake + JSON exec request, then translates between the
// browser terminal and the guestagent frame protocol: browser binary frames →
// FrameStdin, browser text frames (JSON {"resize":{cols,rows}}) → FrameResize,
// and agent FrameStdout/Stderr → browser, FrameExit → close. Same raw-WS +
// impersonating-client posture as the console.
type SandboxExecHandler struct {
	pool   consoleProvider
	auth   Authenticator
	review accessReviewer
	up     websocket.Upgrader
}

func NewSandboxExecHandler(pool consoleProvider, auth Authenticator, origin *OriginPolicy) *SandboxExecHandler {
	return &SandboxExecHandler{
		pool:   pool,
		auth:   auth,
		review: ssarReviewer{pool: pool},
		up:     wsUpgrader(origin.Allow),
	}
}

type resizeMsg struct {
	Resize *struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	} `json:"resize"`
}

func (h *SandboxExecHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cluster, namespace, name := q.Get("cluster"), q.Get("namespace"), q.Get("name")
	if cluster == "" || namespace == "" || name == "" {
		http.Error(w, "cluster, namespace and name are required", http.StatusBadRequest)
		return
	}
	command := q.Get("cmd")
	if command == "" {
		command = "/bin/sh"
	}

	// Bearer via Sec-WebSocket-Protocol; ?token= still accepted but deprecated.
	// See internal/gateway/wsauth.go.
	hdr, viaQuery := wsAuthHeader(r)
	if viaQuery {
		klog.V(2).InfoS("websocket bearer supplied via the deprecated ?token= query parameter; "+
			"it is written to every access log on the path — upgrade the client to the "+
			"Sec-WebSocket-Protocol form", "path", r.URL.Path)
	}
	id, err := h.auth.Authenticate(r.Context(), hdr)
	if err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	// The user is authorized for the sandbox shell, not for pods/exec, and the
	// exec runs as the gateway's member credential (see exec_bridge.go).
	cfg, clientset, target, ok := sandboxLauncher(r.Context(), w, h.pool, h.review, cluster, id, namespace, name, "create", "exec")
	if !ok {
		return
	}

	execReq := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(target).Namespace(namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: launcherContainer,
			Command:   []string{"sh", "-c", sandboxShellBridge(namespace, target)},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(cfg, "POST", execReq.URL())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go pumpExec(func() error {
		return executor.StreamWithContext(r.Context(), remotecommand.StreamOptions{
			Stdin: inR, Stdout: outW, Stderr: outW,
		})
	}, inR, outW)
	br := bufio.NewReader(outR)

	// vsock CONNECT handshake (before the WS upgrade, so a failure is a readable
	// HTTP error rather than a closed socket).
	if _, err := io.WriteString(inW, fmt.Sprintf("CONNECT %d\n", agentVsockPort)); err != nil {
		inW.Close()
		http.Error(w, "vsock connect: "+err.Error(), http.StatusBadGateway)
		return
	}
	okLine, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(okLine, "OK ") {
		inW.Close()
		msg := "vsock handshake failed (is the sandbox running with an agent?)"
		if err != nil && err != io.EOF {
			msg += ": " + err.Error()
		}
		http.Error(w, msg, http.StatusConflict)
		return
	}

	// The interactive exec request; the agent replies with output frames.
	reqObj := map[string]interface{}{
		"v": 1, "op": "exec", "argv": []string{command},
		"stream": true, "stdin": true, "tty": true,
	}
	reqBytes, _ := json.Marshal(reqObj)
	if _, err := inW.Write(append(reqBytes, '\n')); err != nil {
		inW.Close()
		http.Error(w, "send exec request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	conn, err := h.up.Upgrade(w, r, nil)
	if err != nil {
		inW.Close()
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxWSMessageBytes)
	defer auditSession("sandbox-exec", id, cluster, namespace, name,
		"pod", target, "command", auditedCommand(command))()

	fw := guestagent.NewFrameWriter(inW)

	// Browser → agent: binary = stdin, text {"resize":{cols,rows}} = TTY resize.
	go func() {
		defer inW.Close()
		for {
			typ, data, err := conn.ReadMessage()
			if err != nil {
				_ = fw.Write(guestagent.FrameStdinClose, nil)
				return
			}
			if typ == websocket.TextMessage {
				var m resizeMsg
				if json.Unmarshal(data, &m) == nil && m.Resize != nil {
					_ = fw.Write(guestagent.FrameResize, guestagent.ResizePayload(m.Resize.Rows, m.Resize.Cols))
					continue
				}
			}
			_ = fw.Write(guestagent.FrameStdin, data)
		}
	}()

	// Agent → browser: stdout/stderr as binary; exit closes the socket.
	for {
		ftyp, payload, err := guestagent.ReadFrame(br)
		if err != nil {
			if err != io.EOF {
				_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n[exec closed: "+err.Error()+"]\r\n"))
			}
			return
		}
		switch ftyp {
		case guestagent.FrameStdout, guestagent.FrameStderr:
			if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
				return
			}
		case guestagent.FrameExit:
			code := guestagent.DecodeExitCode(payload)
			_ = conn.WriteMessage(websocket.TextMessage, fmt.Appendf(nil, "\r\n[exited %d]\r\n", code))
			return
		}
	}
}

// pumpExec runs a remote-command stream wired to the given pipe ends, then
// fails anything still waiting on either pipe with the stream's outcome (its
// error, or io.EOF when it ended cleanly).
//
// A refused exec (403, pod not running) returns without ever reading stdin,
// and an io.Pipe write blocks until it is read: the handler's CONNECT write
// used to hang forever, holding the handler, its goroutines and the client's
// connection.
func pumpExec(stream func() error, stdin *io.PipeReader, stdout *io.PipeWriter) {
	err := stream()
	if err == nil {
		err = io.EOF
	}
	stdin.CloseWithError(err)
	stdout.CloseWithError(err)
}
