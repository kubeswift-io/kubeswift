package gateway

import (
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// consoleProvider is the subset of ClientPool the console plane needs: the
// impersonating dynamic client (to resolve + authorize the guest's pod) and the
// raw REST config (client-go's remotecommand exec needs it).
type consoleProvider interface {
	DynamicFor(cluster string, id Identity) (dynamic.Interface, error)
	RestConfigFor(cluster string, id Identity) (*rest.Config, error)
}

// ConsoleHandler bridges a guest's serial console to a browser WebSocket. It is
// the D5 bootstrap path: exec `socat ... UNIX-CONNECT:<serial.sock>` inside the
// launcher pod (as the impersonated user) and pump bytes both ways. The
// swiftletd serial-on-a-port transport is the later upgrade.
type ConsoleHandler struct {
	pool consoleProvider
	auth Authenticator
	up   websocket.Upgrader
}

func NewConsoleHandler(pool consoleProvider, auth Authenticator) *ConsoleHandler {
	return &ConsoleHandler{
		pool: pool,
		auth: auth,
		// The bearer token (not a cookie) is the auth, so a cross-origin upgrade
		// is safe to accept — mirrors the Connect CORS=* posture.
		up: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
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

	// Resolve the guest's current launcher pod (authz via the impersonating client).
	dyn, err := h.pool.DynamicFor(cluster, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	pods, err := dyn.Resource(podGVR).Namespace(namespace).
		List(r.Context(), metav1.ListOptions{LabelSelector: guestPodLabel + "=" + name})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	podName := ""
	for i := range pods.Items {
		phase, _, _ := unstructured.NestedString(pods.Items[i].Object, "status", "phase")
		if phase == "Running" { // prefer a Running pod; fall back to the first
			podName = pods.Items[i].GetName()
			break
		}
		if podName == "" {
			podName = pods.Items[i].GetName()
		}
	}
	if podName == "" {
		http.Error(w, "no launcher pod for guest (is it running?)", http.StatusConflict)
		return
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

	// The serial socket is keyed by the GUEST id (ns-name), stable across the
	// <guest>-mig-<uid> pod rename. Mirrors swiftctl console exactly.
	serialSocket := fmt.Sprintf("/var/lib/kubeswift/run/%s-%s/serial.sock", namespace, name)
	bridge := fmt.Sprintf("for i in $(seq 1 15); do test -S %q && break; sleep 1; done; "+
		"test -S %q || { echo 'serial socket not found at %s'; exit 1; }; "+
		"exec socat -,raw,echo=0 UNIX-CONNECT:%s", serialSocket, serialSocket, serialSocket, serialSocket)

	execReq := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(podName).Namespace(namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: launcherContainer, // the launcher pod is multi-container; name the swiftletd one
			Command:   []string{"sh", "-c", bridge},
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
