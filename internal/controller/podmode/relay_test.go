package podmode

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/cli"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftsandbox"
	"github.com/kubeswift-io/kubeswift/internal/guestagent"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

const goodToken = "good"

// linked is a stand-in with its SwiftSandbox and a running launcher.
func linked(t *testing.T) []client.Object {
	pod := standIn(t)
	pod.Labels[podmode.LabelUID] = uid
	return []client.Object{
		pod,
		&sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: sbKey, Namespace: pmNS,
			Labels:      map[string]string{podmode.LabelUID: uid},
			Annotations: map[string]string{podmode.AnnotationStandIn: "tenant/sb-1"}}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: sbKey, Namespace: pmNS, Labels: map[string]string{swiftsandbox.SandboxLabelKey: sbKey}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}
}

// review accepts goodToken as the stand-in's bound token.
func review(user authnv1.UserInfo) func(context.Context, string) (*authnv1.TokenReviewStatus, error) {
	return func(_ context.Context, token string) (*authnv1.TokenReviewStatus, error) {
		if token != goodToken {
			return &authnv1.TokenReviewStatus{}, nil
		}
		return &authnv1.TokenReviewStatus{Authenticated: true, Audiences: []string{podmode.TokenAudience}, User: user}, nil
	}
}

var standInUser = authnv1.UserInfo{
	Username: "system:serviceaccount:tenant:default",
	Extra: map[string]authnv1.ExtraValue{
		extraPodName: {"sb-1"},
		extraPodUID:  {uid},
	},
}

// fakeAgent answers the CONNECT handshake, records the request, and replies
// with "hi" on stdout and exit code 3.
func fakeAgent(t *testing.T, got chan<- map[string]any) func(context.Context, string, string) (*AgentConn, error) {
	return func(_ context.Context, ns, pod string) (*AgentConn, error) {
		if ns != pmNS || pod != sbKey {
			t.Errorf("dialled %s/%s", ns, pod)
		}
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		go func() {
			defer outW.Close()
			br := bufio.NewReader(inR)
			if l, _ := br.ReadString('\n'); l != "CONNECT 1024\n" {
				t.Errorf("handshake %q", l)
			}
			_, _ = io.WriteString(outW, "OK 1024\n")
			line, _ := br.ReadString('\n')
			var req map[string]any
			_ = json.Unmarshal([]byte(line), &req)
			got <- req
			typ, payload, _ := guestagent.ReadFrame(br)
			if typ != guestagent.FrameStdin || string(payload) != "input" {
				t.Errorf("stdin frame %d %q", typ, payload)
			}
			_ = guestagent.WriteFrame(outW, guestagent.FrameStdout, []byte("hi"))
			_ = guestagent.WriteFrame(outW, guestagent.FrameExit, guestagent.ExitCode(3))
		}()
		return &AgentConn{R: bufio.NewReader(outR), W: inW}, nil
	}
}

func newRelay(t *testing.T, user authnv1.UserInfo, objs ...client.Object) *Relay {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	return &Relay{Client: c, Namespace: pmNS, ShimImage: func() string { return shim }, Review: review(user)}
}

func execHeader(argv ...string) string {
	b, _ := json.Marshal(podmode.ExecRequest{Argv: argv})
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestRelayRefuses(t *testing.T) {
	notStandIn := linked(t)
	delete(notStandIn[0].(*corev1.Pod).Labels, podmode.LabelStandIn)
	otherSandbox := linked(t)
	otherSandbox[1].(*sandboxv1alpha1.SwiftSandbox).Annotations[podmode.AnnotationStandIn] = "tenant/other"
	tampered := linked(t)
	tampered[0].(*corev1.Pod).Spec.Containers[0].Image = "docker.io/library/busybox"
	noLauncher := linked(t)[:2]
	unbound := standInUser
	unbound.Extra = nil
	wrongUID := standInUser
	wrongUID.Extra = map[string]authnv1.ExtraValue{extraPodName: {"sb-1"}, extraPodUID: {"another"}}
	notSA := standInUser
	notSA.Username = "alice"

	for name, tc := range map[string]struct {
		token string
		user  authnv1.UserInfo
		objs  []client.Object
		code  int
	}{
		"no token":               {"", standInUser, linked(t), http.StatusUnauthorized},
		"bad token":              {"bad", standInUser, linked(t), http.StatusUnauthorized},
		"token not bound to pod": {goodToken, unbound, linked(t), http.StatusForbidden},
		"not a service account":  {goodToken, notSA, linked(t), http.StatusForbidden},
		"pod UID differs":        {goodToken, wrongUID, linked(t), http.StatusForbidden},
		"not a stand-in":         {goodToken, standInUser, notStandIn, http.StatusForbidden},
		"tampered stand-in":      {goodToken, standInUser, tampered, http.StatusForbidden},
		"sandbox of another pod": {goodToken, standInUser, otherSandbox, http.StatusForbidden},
		"no microVM yet":         {goodToken, standInUser, linked(t)[:1], http.StatusServiceUnavailable},
		"launcher not running":   {goodToken, standInUser, noLauncher, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRelay(t, tc.user, tc.objs...)
			r.DialAgent = func(context.Context, string, string) (*AgentConn, error) {
				t.Fatal("dialled the agent")
				return nil, nil
			}
			for _, h := range []http.Handler{r.ExecHandler(), r.LogsHandler()} {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Header.Set(podmode.ExecRequestHeader, execHeader("sh"))
				if tc.token != "" {
					req.Header.Set("Authorization", "Bearer "+tc.token)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != tc.code {
					t.Errorf("status %d, want %d: %s", w.Code, tc.code, w.Body)
				}
			}
		})
	}
}

func TestRelayExecRefusesEmptyCommand(t *testing.T) {
	r := newRelay(t, standInUser, linked(t)...)
	for _, h := range []string{"", "!!", execHeader()} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+goodToken)
		req.Header.Set(podmode.ExecRequestHeader, h)
		w := httptest.NewRecorder()
		r.ExecHandler().ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d", h, w.Code)
		}
	}
}

func TestRelayExec(t *testing.T) {
	r := newRelay(t, standInUser, linked(t)...)
	got := make(chan map[string]any, 1)
	r.DialAgent = fakeAgent(t, got)
	srv := httptest.NewTLSServer(r.ExecHandler())
	defer srv.Close()

	d := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test server
	h := http.Header{}
	h.Set("Authorization", "Bearer "+goodToken)
	h.Set(podmode.ExecRequestHeader, execHeader("sh", "-c", "cat"))
	conn, _, err := d.Dial("wss"+strings.TrimPrefix(srv.URL, "https"), h)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var buf strings.Builder
	_ = guestagent.WriteFrame(&buf, guestagent.FrameStdin, []byte("input"))
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte(buf.String())); err != nil {
		t.Fatal(err)
	}
	req := <-got
	if req["op"] != "exec" || req["stream"] != true || req["errorFrames"] != true || req["tty"] != nil {
		t.Errorf("agent request %v", req)
	}
	if argv, _ := json.Marshal(req["argv"]); string(argv) != `["sh","-c","cat"]` {
		t.Errorf("argv %s", argv)
	}
	var out []byte
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if typ == websocket.BinaryMessage {
			out = append(out, data...)
		}
	}
	rd := strings.NewReader(string(out))
	if typ, p, _ := guestagent.ReadFrame(rd); typ != guestagent.FrameStdout || string(p) != "hi" {
		t.Errorf("first frame %d %q", typ, p)
	}
	if typ, p, _ := guestagent.ReadFrame(rd); typ != guestagent.FrameExit || guestagent.DecodeExitCode(p) != 3 {
		t.Errorf("exit frame %d %v", typ, p)
	}
}

func TestRelayLogs(t *testing.T) {
	r := newRelay(t, standInUser, linked(t)...)
	var since *metav1.Time
	r.Logs = func(_ context.Context, ns, pod string, s *metav1.Time) (io.ReadCloser, error) {
		if ns != pmNS || pod != sbKey {
			t.Errorf("logs of %s/%s", ns, pod)
		}
		since = s
		return io.NopCloser(strings.NewReader("2026-11-26T10:00:00.5Z booted\n")), nil
	}
	req := httptest.NewRequest(http.MethodGet, "/?sinceTime=2026-11-26T09:59:59Z", nil)
	req.Header.Set("Authorization", "Bearer "+goodToken)
	w := httptest.NewRecorder()
	r.LogsHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "2026-11-26T10:00:00.5Z booted\n" {
		t.Errorf("status %d body %q", w.Code, w.Body)
	}
	if since == nil || !since.Time.Equal(time.Date(2026, 11, 26, 9, 59, 59, 0, time.UTC)) {
		t.Errorf("since %v", since)
	}
}

// The exec gate in the chart admits the controller-manager's exec only for
// the agent bridge; keep its pattern and the command the relay runs in step.
func TestAgentBridgeMatchesTheExecGate(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "kubeswift", "templates", "sandbox", "podmode.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`object\.command\[2\]\.matches\(r'([^']+)'\)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no bridge pattern in the pod-mode exec gate")
	}
	re := regexp.MustCompile(strings.ReplaceAll(string(m[1]), "{{ $ns }}", pmNS))
	cmd := cli.SandboxAgentBridgeCommand("/var/lib/kubeswift/run/" + cli.GuestID(pmNS, sbKey))
	if !re.MatchString(cmd) {
		t.Errorf("gate %q does not admit %q", m[1], cmd)
	}
	for _, bad := range []string{
		cmd + "; id",
		strings.Replace(cmd, "vsock.sock", "serial.sock", 2),
		strings.Replace(cmd, pmNS, "other-ns", 2),
	} {
		if re.MatchString(bad) {
			t.Errorf("gate admits %q", bad)
		}
	}
}
