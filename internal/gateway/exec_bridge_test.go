package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/kubeswift-io/kubeswift/internal/controller/swiftsandbox"
)

// Every command the gateway execs is one the policy admits, and the policy's
// patterns admit nothing but the command with a different runtime directory.
func TestBridgeCommandsAreTheOnlyOnesAdmitted(t *testing.T) {
	for _, cmd := range []string{
		consoleBridge("team-a", "vm-a"),
		consoleBridge("team-a", "db.primary-1"),
		sandboxShellBridge("team-a", "sb-1"),
		sandboxLogsBridge("team-a", "sb-1", true),
		sandboxLogsBridge("team-a", "sb-1", false),
	} {
		if !isBridgeCommand(cmd) {
			t.Errorf("the policy refuses the gateway's own command %q", cmd)
		}
	}
	console := consoleBridge("team-a", "vm-a")
	for name, cmd := range map[string]string{
		"a command appended":    console + "; id",
		"a newline and command": console + "\nid",
		"a path out of run":     strings.ReplaceAll(console, "team-a-vm-a", "x/../../../etc"),
		"a command substituted": strings.ReplaceAll(console, "team-a-vm-a", "a$(id)"),
		"backticks":             strings.ReplaceAll(console, "team-a-vm-a", "a`id`"),
		"a space in the dir":    strings.ReplaceAll(console, "team-a-vm-a", "a b"),
		"a leading dash":        strings.ReplaceAll(console, "team-a-vm-a", "-rf"),
		"another socket":        strings.ReplaceAll(console, "serial.sock", "ch.sock"),
		"a shell":               "sh",
	} {
		if isBridgeCommand(cmd) {
			t.Errorf("%s: the policy would admit %q", name, cmd)
		}
	}
}

// The patterns are repeated in the admission policy, which is what enforces
// them; a copy that drifts from the gateway's either refuses the console or
// admits more than the bridge.
func TestBridgePatternsMatchThePolicy(t *testing.T) {
	inPolicy := regexp.MustCompile(`matches\(r'([^']*)'\)`)
	for _, file := range []string{
		"../../charts/kubeswift/templates/gateway/exec-gate.yaml",
		"../../config/samples/gateway/member-rbac.yaml",
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range inPolicy.FindAllStringSubmatch(string(data), -1) {
			got = append(got, m[1])
		}
		if strings.Join(got, "\n") != strings.Join(bridgePatterns, "\n") {
			t.Errorf("%s: the policy's patterns differ from bridgePatterns\npolicy:\n%s\ngateway:\n%s",
				file, strings.Join(got, "\n"), strings.Join(bridgePatterns, "\n"))
		}
	}
}

func TestSandboxPodLabelIsTheControllers(t *testing.T) {
	if sandboxPodLabel != swiftsandbox.SandboxLabelKey {
		t.Errorf("sandboxPodLabel = %q, controller labels launchers %q", sandboxPodLabel, swiftsandbox.SandboxLabelKey)
	}
}

// The launcher is the pod status names, and only if it is labelled as that
// guest's or sandbox's: the member credential execs in it.
func TestLauncherPodOf(t *testing.T) {
	cs := k8sfake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "vm-a", Labels: map[string]string{guestPodLabel: "vm-a"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "other", Labels: map[string]string{guestPodLabel: "vm-b"}}},
	)
	ctx := context.Background()
	if _, err := launcherPodOf(ctx, cs, "team-a", "vm-a", guestPodLabel, "vm-a"); err != nil {
		t.Errorf("the guest's own launcher: %v", err)
	}
	if _, err := launcherPodOf(ctx, cs, "team-a", "other", guestPodLabel, "vm-a"); err == nil {
		t.Error("another guest's pod accepted as the launcher")
	}
	if _, err := launcherPodOf(ctx, cs, "team-a", "gone", guestPodLabel, "vm-a"); err == nil {
		t.Error("a missing pod accepted as the launcher")
	}
}

type staticAuth struct{ id Identity }

func (a staticAuth) Authenticate(context.Context, http.Header) (Identity, error) { return a.id, nil }

type fakeReviewer struct {
	allowed bool
	asked   []authorizationv1.ResourceAttributes
	as      []Identity
}

func (f *fakeReviewer) review(_ context.Context, _ string, id Identity, attrs authorizationv1.ResourceAttributes) (bool, string, error) {
	f.asked = append(f.asked, attrs)
	f.as = append(f.as, id)
	return f.allowed, "", nil
}

// recordingProvider records which identity each member client is built for.
type recordingProvider struct {
	*fakeProvider
	ids []Identity
}

func (p *recordingProvider) DynamicFor(cluster string, id Identity) (dynamic.Interface, error) {
	p.ids = append(p.ids, id)
	return p.fakeProvider.DynamicFor(cluster, id)
}

func (p *recordingProvider) RestConfigFor(cluster string, id Identity) (*rest.Config, error) {
	p.ids = append(p.ids, id)
	return p.fakeProvider.RestConfigFor(cluster, id)
}

// The user is asked about the console or the sandbox, never about pods/exec,
// and a refusal stops before anything is resolved or exec'd. What is
// resolved and exec'd is the member credential's, not the user's.
func TestExecPlanes_AuthorizeTheUserAndActAsTheGateway(t *testing.T) {
	user := Identity{User: "alice", Groups: []string{"ops"}}
	cases := []struct {
		name, target string
		handler      func(consoleProvider, Authenticator, accessReviewer) http.Handler
		want         authorizationv1.ResourceAttributes
	}{
		{
			name:   "console",
			target: "/console?cluster=edge-1&namespace=team-a&name=vm-a",
			handler: func(p consoleProvider, a Authenticator, r accessReviewer) http.Handler {
				h := NewConsoleHandler(p, a, NewOriginPolicy("*", "oidc"))
				h.review = r
				return h
			},
			want: authorizationv1.ResourceAttributes{Namespace: "team-a", Name: "vm-a", Verb: "create",
				Group: "swift.kubeswift.io", Resource: "swiftguests", Subresource: "console"},
		},
		{
			name:   "sandbox shell",
			target: "/sandbox-exec?cluster=edge-1&namespace=team-a&name=sb-1",
			handler: func(p consoleProvider, a Authenticator, r accessReviewer) http.Handler {
				h := NewSandboxExecHandler(p, a, NewOriginPolicy("*", "oidc"))
				h.review = r
				return h
			},
			want: authorizationv1.ResourceAttributes{Namespace: "team-a", Name: "sb-1", Verb: "create",
				Group: "sandbox.kubeswift.io", Resource: "swiftsandboxes", Subresource: "exec"},
		},
		{
			name:   "sandbox logs",
			target: "/sandbox-logs?cluster=edge-1&namespace=team-a&name=sb-1",
			handler: func(p consoleProvider, a Authenticator, r accessReviewer) http.Handler {
				h := NewSandboxLogsHandler(p, a, NewOriginPolicy("*", "oidc"))
				h.review = r
				return h
			},
			want: authorizationv1.ResourceAttributes{Namespace: "team-a", Name: "sb-1", Verb: "get",
				Group: "sandbox.kubeswift.io", Resource: "swiftsandboxes", Subresource: "log"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+" refused", func(t *testing.T) {
			prov := &recordingProvider{fakeProvider: &fakeProvider{clients: map[string]dynamic.Interface{
				"edge-1": fakeDyn(uGuest("team-a", "vm-a", "Running")),
			}}}
			rv := &fakeReviewer{allowed: false}
			w := httptest.NewRecorder()
			tc.handler(prov, staticAuth{user}, rv).ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.target, nil))
			if w.Code != http.StatusForbidden {
				t.Fatalf("want 403, got %d (%s)", w.Code, w.Body.String())
			}
			if len(rv.asked) != 1 || rv.asked[0] != tc.want {
				t.Errorf("asked %+v, want %+v", rv.asked, tc.want)
			}
			if len(rv.as) != 1 || rv.as[0].User != "alice" {
				t.Errorf("asked as %+v, want the user", rv.as)
			}
			for _, id := range prov.ids {
				if !id.empty() {
					t.Errorf("a member client was built for the user %+v", id)
				}
			}
			if strings.Contains(w.Body.String(), "pods/exec") {
				t.Errorf("the refusal mentions pods/exec: %s", w.Body.String())
			}
		})
		t.Run(tc.name+" allowed", func(t *testing.T) {
			prov := &recordingProvider{fakeProvider: &fakeProvider{clients: map[string]dynamic.Interface{
				"edge-1": fakeDyn(uGuest("team-a", "vm-a", "Running")),
			}}}
			rv := &fakeReviewer{allowed: true}
			w := httptest.NewRecorder()
			tc.handler(prov, staticAuth{user}, rv).ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.target, nil))
			// No launcher in the fakes: past authorization, the plane resolves
			// and stops there.
			if w.Code == http.StatusForbidden || w.Code == http.StatusOK {
				t.Fatalf("want a resolution failure past authorization, got %d (%s)", w.Code, w.Body.String())
			}
			for _, id := range prov.ids {
				if !id.empty() {
					t.Errorf("a member client was built for the user %+v", id)
				}
			}
		})
	}
}
