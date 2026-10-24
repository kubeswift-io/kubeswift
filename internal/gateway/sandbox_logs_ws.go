package gateway

import (
	"io"
	"net/http"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// pod for a warm-pool checkout via status.podRef) and pump its console to the
// socket. A pod with a console container (KubeSwift v0.20+) is read with the
// pod logs API, no exec; an older one by exec'ing `tail -F` on the host log
// file inside the launcher. Like the console, it is a raw WebSocket (browsers
// can't do bidi Connect). The user needs get on swiftsandboxes/log either
// way; the read runs as the gateway's member credential (see exec_bridge.go).
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

	// The console container's logs: no exec into the privileged launcher.
	if pod, err := clientset.CoreV1().Pods(namespace).Get(r.Context(), target, metav1.GetOptions{}); err == nil && hasContainer(pod, consoleContainer) {
		stream, err := clientset.CoreV1().Pods(namespace).GetLogs(target, &corev1.PodLogOptions{
			Container: consoleContainer, Follow: follow,
		}).Stream(r.Context())
		switch {
		case err == nil:
			conn, uerr := h.up.Upgrade(w, r, nil)
			if uerr != nil {
				stream.Close()
				return
			}
			defer conn.Close()
			defer stream.Close()
			if _, cerr := io.Copy(&wsConn{conn: conn}, stream); cerr != nil {
				_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n[logs closed: "+cerr.Error()+"]\r\n"))
			}
			return
		case !apierrors.IsForbidden(err):
			http.Error(w, "read the console: "+err.Error(), http.StatusBadGateway)
			return
		}
		// A member credential without pods/log (RBAC set up before v0.20):
		// the exec bridge, as before.
		klog.V(2).InfoS("gateway credential cannot read pods/log; reading the sandbox console by exec", "namespace", namespace, "pod", target)
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

// consoleContainer is a sandbox launcher pod's console container
// (swiftsandbox.ConsoleContainerName).
const consoleContainer = "console"

func hasContainer(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return true
		}
	}
	return false
}
