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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/kubeswift-io/kubeswift/internal/guestagent"
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
	pool consoleProvider
	auth Authenticator
	up   websocket.Upgrader
}

func NewSandboxExecHandler(pool consoleProvider, auth Authenticator) *SandboxExecHandler {
	return &SandboxExecHandler{
		pool: pool,
		auth: auth,
		up:   websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
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

	hdr := http.Header{}
	if tok := q.Get("token"); tok != "" {
		hdr.Set("Authorization", "Bearer "+tok)
	} else if a := r.Header.Get("Authorization"); a != "" {
		hdr.Set("Authorization", a)
	}
	id, err := h.auth.Authenticate(r.Context(), hdr)
	if err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}

	dyn, err := h.pool.DynamicFor(cluster, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	sb, err := dyn.Resource(sandboxGVR).Namespace(namespace).Get(r.Context(), name, metav1.GetOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	target := name
	if podRef, _, _ := unstructured.NestedString(sb.Object, "status", "podRef"); podRef != "" {
		target = podRef
	}

	cfg, err := h.pool.RestConfigFor(cluster, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	vsockSock := fmt.Sprintf("/var/lib/kubeswift/run/%s-%s/vsock.sock", namespace, target)
	waitAndSocat := fmt.Sprintf(
		"for i in $(seq 1 10); do test -S %q && break; sleep 1; done; exec socat -t10 - UNIX-CONNECT:%s",
		vsockSock, vsockSock)

	execReq := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(target).Namespace(namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: launcherContainer,
			Command:   []string{"sh", "-c", waitAndSocat},
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
	go func() {
		_ = executor.StreamWithContext(r.Context(), remotecommand.StreamOptions{
			Stdin: inR, Stdout: outW, Stderr: outW,
		})
		outW.Close()
	}()
	br := bufio.NewReader(outR)

	// vsock CONNECT handshake (before the WS upgrade, so a failure is a readable
	// HTTP error rather than a closed socket).
	if _, err := io.WriteString(inW, fmt.Sprintf("CONNECT %d\n", agentVsockPort)); err != nil {
		inW.Close()
		http.Error(w, "vsock connect: "+err.Error(), http.StatusInternalServerError)
		return
	}
	okLine, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(okLine, "OK ") {
		inW.Close()
		http.Error(w, "vsock handshake failed (is the sandbox running with an agent?)", http.StatusConflict)
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
