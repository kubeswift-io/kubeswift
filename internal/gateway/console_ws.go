package gateway

import (
	"io"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

// consoleProvider is the subset of ClientPool the console planes need: the
// user's REST config (to ask the member whether the user may open the
// console), and the member credential's dynamic client and REST config (to
// resolve the launcher and exec the bridge in it; client-go's remotecommand
// needs the raw config).
type consoleProvider interface {
	DynamicFor(cluster string, id Identity) (dynamic.Interface, error)
	RestConfigFor(cluster string, id Identity) (*rest.Config, error)
}

// ConsoleHandler bridges a guest's serial console to a browser WebSocket: it
// execs `socat ... UNIX-CONNECT:<serial.sock>` in the launcher pod and pumps
// bytes both ways. The user needs create on swiftguests/console; the exec runs
// as the gateway's member credential, which the kubeswift-gateway-exec-gate
// policy holds to the bridge command (see exec_bridge.go).
type ConsoleHandler struct {
	pool   consoleProvider
	auth   Authenticator
	review accessReviewer
	up     websocket.Upgrader
}

func NewConsoleHandler(pool consoleProvider, auth Authenticator, origin *OriginPolicy) *ConsoleHandler {
	return &ConsoleHandler{
		pool:   pool,
		auth:   auth,
		review: ssarReviewer{pool: pool},
		up:     wsUpgrader(origin.Allow),
	}
}

func (h *ConsoleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cluster, namespace, name := q.Get("cluster"), q.Get("namespace"), q.Get("name")
	if cluster == "" || namespace == "" || name == "" {
		http.Error(w, "cluster, namespace and name are required", http.StatusBadRequest)
		return
	}

	// Browsers cannot set headers on a WebSocket, so the token rides a query
	// param (?token=); insecure mode ignores it. (URL-borne tokens can land in
	// logs — acceptable for the bootstrap path, noted in the operator docs.)
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

	// The user is authorized for the console, not for pods/exec, and the exec
	// runs as the gateway's member credential (see exec_bridge.go).
	cfg, err := h.pool.RestConfigFor(cluster, Identity{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !authorizeUser(r.Context(), w, h.review, cluster, id, authorizationv1.ResourceAttributes{
		Namespace: namespace, Name: name, Verb: "create",
		Group: swiftGuestGVR.Group, Resource: swiftGuestGVR.Resource, Subresource: "console",
	}) {
		return
	}

	// The guest's launcher is the pod its status names: the controller writes
	// it, and follows a live migration's <guest>-mig-<uid> rename.
	dyn, err := h.pool.DynamicFor(cluster, Identity{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	guest, err := dyn.Resource(swiftGuestGVR).Namespace(namespace).Get(r.Context(), name, metav1.GetOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	podName, _, _ := unstructured.NestedString(guest.Object, "status", "podRef", "name")
	if podName == "" {
		http.Error(w, "no launcher pod for guest (is it running?)", http.StatusConflict)
		return
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := launcherPodOf(r.Context(), clientset, namespace, podName, guestPodLabel, name); err != nil {
		http.Error(w, "no launcher pod for guest: "+err.Error(), http.StatusConflict)
		return
	}

	execReq := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(podName).Namespace(namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: launcherContainer, // the launcher pod is multi-container; name the swiftletd one
			Command:   []string{"sh", "-c", consoleBridge(namespace, name)},
			Stdin:     true,
			Stdout:    true,
			Stderr:    false,
			TTY:       true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(cfg, "POST", execReq.URL())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Upgrade only after the pre-flight passes, so a failure is a plain HTTP
	// error the browser can read (not a closed socket).
	conn, err := h.up.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the response
	}
	defer conn.Close()
	conn.SetReadLimit(maxWSMessageBytes)
	defer auditSession("console", id, cluster, namespace, name, "pod", podName)()

	wc := &wsConn{conn: conn}
	if streamErr := executor.StreamWithContext(r.Context(), remotecommand.StreamOptions{
		Stdin:  wc,
		Stdout: wc,
		Tty:    true,
	}); streamErr != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n[console closed: "+streamErr.Error()+"]\r\n"))
	}
}

// wsConn adapts a WebSocket to io.Reader (browser keystrokes → exec stdin) and
// io.Writer (serial output → browser). gorilla allows one concurrent reader and
// one concurrent writer — exactly remotecommand's stdin/stdout access pattern.
type wsConn struct {
	conn *websocket.Conn
	rbuf []byte
	wmu  sync.Mutex
}

func (c *wsConn) Read(p []byte) (int, error) {
	for len(c.rbuf) == 0 {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return 0, io.EOF
		}
		c.rbuf = data
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
