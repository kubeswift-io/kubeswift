package gateway

import (
	"fmt"
	"net/http"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
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
// console, it is a raw WebSocket (browsers can't do bidi Connect) with the
// bearer token on the query string; the impersonating client authorizes the
// read.
type SandboxLogsHandler struct {
	pool consoleProvider
	auth Authenticator
	up   websocket.Upgrader
}

func NewSandboxLogsHandler(pool consoleProvider, auth Authenticator) *SandboxLogsHandler {
	return &SandboxLogsHandler{
		pool: pool,
		auth: auth,
		up:   websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
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

	// Resolve the target pod: a warm-pool checkout's run dir lives in the claimed
	// slot pod (status.podRef), not a pod named after the sandbox. Mirrors
	// swiftctl's sandboxTargetPod. Getting the CR also authorizes the read.
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

	// The console is captured to <run>/serial.sock.log; the run dir is keyed by
	// the launcher pod identity (ns-<targetPod>), same as swiftctl.
	logFile := fmt.Sprintf("/var/lib/kubeswift/run/%s-%s/serial.sock.log", namespace, target)
	shellCmd := "cat " + logFile
	if follow {
		// tail from the start then follow; -F keeps waiting if the file appears late.
		shellCmd = "tail -n +1 -F " + logFile
	}

	execReq := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(target).Namespace(namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: launcherContainer,
			Command:   []string{"sh", "-c", shellCmd},
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
