package gateway

import (
	"net/http"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

// sandboxGVR is the SwiftSandbox resource. Kept local to the gateway so the
// console plane does not import the controller package.
var sandboxGVR = schema.GroupVersionResource{
	Group: "sandbox.kubeswift.io", Version: "v1alpha1", Resource: "swiftsandboxes",
}

// SandboxLogsHandler streams a running SwiftSandbox's captured console log to a
// browser WebSocket (read-only). It mirrors ConsoleHandler / `swiftctl sandbox
// logs`: resolve the sandbox's target pod (its own launcher, or the claimed slot
// pod for a warm-pool checkout via status.podRef), then exec `tail -F` on the
// host log file inside the launcher and pump stdout to the socket. Like the
// console, it is a raw WebSocket (browsers can't do bidi Connect). The user
// needs get on swiftsandboxes/log, and the exec runs as the gateway's member
// credential (see exec_bridge.go).
type SandboxLogsHandler struct {
	pool   consoleProvider
	auth   Authenticator
	review accessReviewer
	up     websocket.Upgrader
}

func NewSandboxLogsHandler(pool consoleProvider, auth Authenticator, origin *OriginPolicy) *SandboxLogsHandler {
	return &SandboxLogsHandler{
		pool:   pool,
		auth:   auth,
		review: ssarReviewer{pool: pool},
		up:     wsUpgrader(origin.Allow),
	}
}

func (h *SandboxLogsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cluster, namespace, name := q.Get("cluster"), q.Get("namespace"), q.Get("name")
	if cluster == "" || namespace == "" || name == "" {
		http.Error(w, "cluster, namespace and name are required", http.StatusBadRequest)
		return
	}
	follow := q.Get("follow") != "false" // default: follow

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

	// The user is authorized to read the sandbox's logs, not for pods/exec, and
	// the exec runs as the gateway's member credential (see exec_bridge.go).
	// The logs live in the target pod's runtime directory, as for swiftctl.
	cfg, clientset, target, ok := sandboxLauncher(r.Context(), w, h.pool, h.review, cluster, id, namespace, name, "get", "log")
	if !ok {
		return
	}

	execReq := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(target).Namespace(namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: launcherContainer,
			Command:   []string{"sh", "-c", sandboxLogsBridge(namespace, target, follow)},
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(cfg, "POST", execReq.URL())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	conn, err := h.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	wc := &wsConn{conn: conn}
	if streamErr := executor.StreamWithContext(r.Context(), remotecommand.StreamOptions{
		Stdout: wc,
		Stderr: wc,
	}); streamErr != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n[logs closed: "+streamErr.Error()+"]\r\n"))
	}
}
