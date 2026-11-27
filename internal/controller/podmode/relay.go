package podmode

// The relay is how `kubectl exec` and `kubectl logs` on a stand-in reach its
// microVM. The stand-in's shim calls it with the stand-in's own projected
// ServiceAccount token (audience podmode.TokenAudience, bound to the Pod); the
// relay resolves the token to that Pod with a TokenReview and serves only the
// microVM linked to it. Whoever may exec into the stand-in may therefore exec
// into its microVM, and nothing else: no tenant credential is ever granted
// pods/exec on a privileged launcher.
//
// Exec runs the guest agent bridge in the launcher as the controller-manager
// (an admission policy holds that credential's exec to exactly this command)
// and builds the agent's exec request itself, then carries the agent's frames
// both ways. Logs read the launcher's console container.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/cli"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftsandbox"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

const (
	// agentVsockPort is the guest agent's vsock port.
	agentVsockPort = 1024
	// Extra keys of a bound ServiceAccount token's TokenReview user.
	extraPodName = "authentication.kubernetes.io/pod-name"
	extraPodUID  = "authentication.kubernetes.io/pod-uid"

	maxExecRequest = 64 << 10
	maxArgv        = 1024
)

// AgentConn is a stream to a launcher's guest agent bridge (before the
// CONNECT handshake).
type AgentConn struct {
	R    *bufio.Reader
	W    io.WriteCloser
	Done <-chan error
}

// Relay serves podmode.RelayExecPath and podmode.RelayLogsPath.
type Relay struct {
	Client    client.Reader
	Namespace string // the pod-mode namespace
	ShimImage func() string

	// Review authenticates a relay token (TokenReview).
	Review func(ctx context.Context, token string) (*authnv1.TokenReviewStatus, error)
	// DialAgent runs the agent bridge in a launcher pod.
	DialAgent func(ctx context.Context, namespace, pod string) (*AgentConn, error)
	// Logs streams a launcher's console.
	Logs func(ctx context.Context, namespace, pod string, since *metav1.Time) (io.ReadCloser, error)

	up websocket.Upgrader
}

// NewRelay is a Relay backed by the API server.
func NewRelay(c client.Reader, cfg *rest.Config, namespace string, shimImage func() string) (*Relay, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Relay{
		Client:    c,
		Namespace: namespace,
		ShimImage: shimImage,
		Review: func(ctx context.Context, token string) (*authnv1.TokenReviewStatus, error) {
			tr, err := cs.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{
				Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{podmode.TokenAudience}},
			}, metav1.CreateOptions{})
			if err != nil {
				return nil, err
			}
			return &tr.Status, nil
		},
		DialAgent: func(ctx context.Context, namespace, pod string) (*AgentConn, error) {
			return dialAgent(ctx, cfg, cs, namespace, pod)
		},
		Logs: func(ctx context.Context, namespace, pod string, since *metav1.Time) (io.ReadCloser, error) {
			return cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
				Container: cli.ConsoleContainer, Follow: true, Timestamps: true, SinceTime: since,
			}).Stream(ctx)
		},
	}, nil
}

// relayError is a refusal with the HTTP status to answer it with.
type relayError struct {
	code int
	msg  string
}

func (e *relayError) Error() string { return e.msg }

func refuse(code int, format string, a ...any) error {
	return &relayError{code: code, msg: fmt.Sprintf(format, a...)}
}

func writeError(w http.ResponseWriter, err error) {
	var re *relayError
	if errors.As(err, &re) {
		http.Error(w, re.msg, re.code)
		return
	}
	http.Error(w, err.Error(), http.StatusBadGateway)
}

// target is a stand-in and the launcher pod of its microVM.
type target struct {
	standIn  types.NamespacedName
	sandbox  string
	launcher string
}

// resolve authenticates the request's token and returns the launcher of the
// microVM linked to the stand-in the token is bound to.
func (r *Relay) resolve(req *http.Request) (*target, error) {
	token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return nil, refuse(http.StatusUnauthorized, "no token")
	}
	st, err := r.Review(req.Context(), token)
	if err != nil {
		return nil, fmt.Errorf("token review: %w", err)
	}
	if !st.Authenticated || !contains(st.Audiences, podmode.TokenAudience) {
		return nil, refuse(http.StatusUnauthorized, "token not valid for %s", podmode.TokenAudience)
	}
	ns, _, isSA := strings.Cut(strings.TrimPrefix(st.User.Username, "system:serviceaccount:"), ":")
	names, uids := st.User.Extra[extraPodName], st.User.Extra[extraPodUID]
	if !strings.HasPrefix(st.User.Username, "system:serviceaccount:") || !isSA || len(names) != 1 || len(uids) != 1 {
		return nil, refuse(http.StatusForbidden, "the token is not bound to a Pod")
	}
	key := types.NamespacedName{Namespace: ns, Name: names[0]}
	var pod corev1.Pod
	if err := r.Client.Get(req.Context(), key, &pod); err != nil {
		return nil, refuse(http.StatusForbidden, "stand-in %s: %v", key, err)
	}
	uid := string(pod.UID)
	if uid != uids[0] || !podmode.IsStandIn(&pod) || pod.Labels[podmode.LabelUID] != uid {
		return nil, refuse(http.StatusForbidden, "%s is not a pod-mode stand-in", key)
	}
	if !podmode.ShapeOK(&pod, r.ShimImage()) {
		return nil, refuse(http.StatusForbidden, "%s was changed and is no longer a stand-in", key)
	}
	var sb sandboxv1alpha1.SwiftSandbox
	sbKey := types.NamespacedName{Namespace: r.Namespace, Name: SandboxOf(&pod)}
	if err := r.Client.Get(req.Context(), sbKey, &sb); err != nil {
		return nil, refuse(http.StatusServiceUnavailable, "the microVM of %s does not exist yet", key)
	}
	if !owns(&pod, &sb) {
		return nil, refuse(http.StatusForbidden, "%s does not belong to %s", sbKey, key)
	}
	var pods corev1.PodList
	if err := r.Client.List(req.Context(), &pods, client.InNamespace(r.Namespace),
		client.MatchingLabels{swiftsandbox.SandboxLabelKey: sb.Name}); err != nil {
		return nil, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
			return &target{standIn: key, sandbox: sb.Name, launcher: p.Name}, nil
		}
	}
	return nil, refuse(http.StatusServiceUnavailable, "the microVM of %s is not running yet", key)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ExecHandler serves podmode.RelayExecPath.
func (r *Relay) ExecHandler() http.Handler { return http.HandlerFunc(r.serveExec) }

// LogsHandler serves podmode.RelayLogsPath.
func (r *Relay) LogsHandler() http.Handler { return http.HandlerFunc(r.serveLogs) }

func parseExecRequest(h string) (*podmode.ExecRequest, error) {
	if h == "" || len(h) > maxExecRequest {
		return nil, refuse(http.StatusBadRequest, "missing or oversized %s", podmode.ExecRequestHeader)
	}
	raw, err := base64.RawURLEncoding.DecodeString(h)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, "%s: %v", podmode.ExecRequestHeader, err)
	}
	var er podmode.ExecRequest
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, refuse(http.StatusBadRequest, "%s: %v", podmode.ExecRequestHeader, err)
	}
	if len(er.Argv) == 0 || len(er.Argv) > maxArgv || er.Argv[0] == "" {
		return nil, refuse(http.StatusBadRequest, "no command")
	}
	return &er, nil
}

func (r *Relay) serveExec(w http.ResponseWriter, req *http.Request) {
	er, err := parseExecRequest(req.Header.Get(podmode.ExecRequestHeader))
	if err != nil {
		writeError(w, err)
		return
	}
	t, err := r.resolve(req)
	if err != nil {
		writeError(w, err)
		return
	}
	// The exec outlives the request context only through the WebSocket.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ac, err := r.DialAgent(ctx, r.Namespace, t.launcher)
	if err != nil {
		writeError(w, fmt.Errorf("microVM agent: %w", err))
		return
	}
	defer ac.W.Close()
	if err := agentHandshake(ac, er); err != nil {
		writeError(w, refuse(http.StatusServiceUnavailable, "microVM agent: %v", err))
		return
	}
	conn, err := r.up.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	klog.InfoS("pod-mode exec", "standIn", t.standIn, "sandbox", t.sandbox, "command", er.Argv[0], "tty", er.TTY)

	go func() {
		defer ac.W.Close()
		for {
			typ, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if typ != websocket.BinaryMessage {
				continue
			}
			if _, err := ac.W.Write(data); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := ac.R.Read(buf)
		if n > 0 {
			if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			return
		}
	}
}

// agentHandshake connects to the guest agent and sends the exec request.
func agentHandshake(ac *AgentConn, er *podmode.ExecRequest) error {
	if _, err := fmt.Fprintf(ac.W, "CONNECT %d\n", agentVsockPort); err != nil {
		return err
	}
	ok, err := ac.R.ReadString('\n')
	if err != nil {
		return fmt.Errorf("vsock handshake: %w", err)
	}
	if !strings.HasPrefix(ok, "OK ") {
		return fmt.Errorf("vsock handshake: %q", strings.TrimSpace(ok))
	}
	agentReq := map[string]any{
		"v": 1, "op": "exec", "argv": er.Argv, "stream": true, "stdin": true, "errorFrames": true,
	}
	if er.TTY {
		agentReq["tty"] = true
		if er.Rows > 0 && er.Cols > 0 {
			agentReq["rows"], agentReq["cols"] = er.Rows, er.Cols
		}
	}
	b, _ := json.Marshal(agentReq)
	_, err = ac.W.Write(append(b, '\n'))
	return err
}

func (r *Relay) serveLogs(w http.ResponseWriter, req *http.Request) {
	var since *metav1.Time
	if s := req.URL.Query().Get("sinceTime"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			http.Error(w, "sinceTime: "+err.Error(), http.StatusBadRequest)
			return
		}
		since = &metav1.Time{Time: t}
	}
	t, err := r.resolve(req)
	if err != nil {
		writeError(w, err)
		return
	}
	rc, err := r.Logs(req.Context(), r.Namespace, t.launcher, since)
	if err != nil {
		writeError(w, fmt.Errorf("microVM console: %w", err))
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// dialAgent runs the guest agent bridge in the launcher container.
func dialAgent(ctx context.Context, cfg *rest.Config, cs kubernetes.Interface, namespace, pod string) (*AgentConn, error) {
	execReq := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: cli.LauncherContainer,
			Command:   []string{"sh", "-c", cli.SandboxAgentBridgeCommand("/var/lib/kubeswift/run/" + cli.GuestID(namespace, pod))},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(cfg, "POST", execReq.URL())
	if err != nil {
		return nil, err
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdin: inR, Stdout: outW, Stderr: io.Discard,
		})
		ended := err
		if ended == nil {
			ended = io.EOF
		}
		// A refused exec never reads stdin: fail the waiting writes.
		inR.CloseWithError(ended)
		outW.CloseWithError(ended)
		done <- err
	}()
	return &AgentConn{R: bufio.NewReader(outR), W: inW, Done: done}, nil
}

// RelayInfo is what the controller writes into AnnotationRelay: the webhook
// Service's ClusterIP, its DNS name for the certificate, and the CA from the
// serving certificate's directory.
type RelayInfo struct {
	Client           client.Reader
	ServiceNamespace string
	ServiceName      string
	CAFile           string
}

// Get resolves the relay; an error means the stand-in cannot be given one yet.
func (ri *RelayInfo) Get(ctx context.Context) (*podmode.Relay, error) {
	ca, err := os.ReadFile(ri.CAFile)
	if err != nil {
		return nil, fmt.Errorf("relay CA: %w", err)
	}
	var svc corev1.Service
	if err := ri.Client.Get(ctx, types.NamespacedName{Namespace: ri.ServiceNamespace, Name: ri.ServiceName}, &svc); err != nil {
		return nil, fmt.Errorf("relay service: %w", err)
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone || len(svc.Spec.Ports) == 0 {
		return nil, fmt.Errorf("relay service %s/%s has no ClusterIP", ri.ServiceNamespace, ri.ServiceName)
	}
	return &podmode.Relay{
		Addr:       net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(int(svc.Spec.Ports[0].Port))),
		ServerName: ri.ServiceName + "." + ri.ServiceNamespace + ".svc",
		CA:         string(ca),
	}, nil
}
