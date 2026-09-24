package gateway

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/kubeswift-io/kubeswift/internal/cli"
)

// The console, the sandbox shell and the sandbox log view reach a launcher the
// same way: they exec a fixed bridge command in its launcher container, socat
// or tail against a file or socket in the launcher's runtime directory.
//
// The exec runs as the gateway's own member credential, never as the user. A
// user is authorized for the console or the sandbox (create
// swiftguests/console, create swiftsandboxes/exec, get swiftsandboxes/log),
// not for pods/exec: the launcher is privileged, so pods/exec on it is root on
// its node, and a user who held it could run anything there, not only the
// bridge.
//
// The member credential does hold pods/exec, so the bridge commands are all it
// may exec: the kubeswift-gateway-exec-gate ValidatingAdmissionPolicy admits
// the credential's exec only in a launcher container and only when the command
// matches one of bridgePatterns. The patterns are repeated in that policy
// (charts/kubeswift/templates/gateway/exec-gate.yaml and
// config/samples/gateway/member-rbac.yaml); TestBridgePatternsMatchThePolicy
// keeps the copies identical.
//
// Every variable part of a command is a runtime directory
// /var/lib/kubeswift/run/<namespace>-<name>, and namespaces and object names
// are DNS names, so no pattern admits a character the shell treats specially.
var bridgePatterns = []string{
	// The guest's serial console.
	`^for i in \$\(seq 1 15\); do test -S /var/lib/kubeswift/run/[a-z0-9][a-z0-9.-]*/serial\.sock && break; sleep 1; done; test -S /var/lib/kubeswift/run/[a-z0-9][a-z0-9.-]*/serial\.sock \|\| \{ echo serial socket not found; exit 1; \}; exec socat -,raw,echo=0 UNIX-CONNECT:/var/lib/kubeswift/run/[a-z0-9][a-z0-9.-]*/serial\.sock$`,
	// A sandbox's in-guest agent, over vsock.
	`^for i in \$\(seq 1 10\); do test -S /var/lib/kubeswift/run/[a-z0-9][a-z0-9.-]*/vsock\.sock && break; sleep 1; done; exec socat -t10 - UNIX-CONNECT:/var/lib/kubeswift/run/[a-z0-9][a-z0-9.-]*/vsock\.sock$`,
	// A sandbox's console and workload logs, followed or read once
	// (cli.SandboxLogsCommand).
	`^tail -q -n \+1 -F /var/lib/kubeswift/run/[a-z0-9][a-z0-9.-]*/serial\.sock\.log /var/lib/kubeswift/run/[a-z0-9][a-z0-9.-]*/workload\.log$`,
	`^cat /var/lib/kubeswift/run/[a-z0-9][a-z0-9.-]*/serial\.sock\.log && \{ cat /var/lib/kubeswift/run/[a-z0-9][a-z0-9.-]*/workload\.log 2>/dev/null \|\| true; \}$`,
}

var bridgeRes = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(bridgePatterns))
	for i, p := range bridgePatterns {
		out[i] = regexp.MustCompile(p)
	}
	return out
}()

// isBridgeCommand reports whether the policy admits cmd as the argument of
// `sh -c`.
func isBridgeCommand(cmd string) bool {
	for _, re := range bridgeRes {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

// runtimeDir is the launcher's per-pod runtime directory: swift-runtime keys it
// by <namespace>-<name> (the guest for a SwiftGuest, the launcher pod for a
// sandbox).
func runtimeDir(namespace, name string) string {
	return fmt.Sprintf("/var/lib/kubeswift/run/%s-%s", namespace, name)
}

// consoleBridge bridges the guest's serial socket, waiting for it while the
// guest starts. The socket is keyed by the guest, which is stable across the
// <guest>-mig-<uid> pod rename.
func consoleBridge(namespace, guest string) string {
	sock := runtimeDir(namespace, guest) + "/serial.sock"
	return fmt.Sprintf("for i in $(seq 1 15); do test -S %s && break; sleep 1; done; "+
		"test -S %s || { echo serial socket not found; exit 1; }; "+
		"exec socat -,raw,echo=0 UNIX-CONNECT:%s", sock, sock, sock)
}

// sandboxShellBridge bridges the sandbox's vsock socket to its in-guest agent.
func sandboxShellBridge(namespace, pod string) string {
	sock := runtimeDir(namespace, pod) + "/vsock.sock"
	return fmt.Sprintf("for i in $(seq 1 10); do test -S %s && break; sleep 1; done; "+
		"exec socat -t10 - UNIX-CONNECT:%s", sock, sock)
}

// sandboxLogsBridge reads the sandbox's console and workload logs.
func sandboxLogsBridge(namespace, pod string, follow bool) string {
	return cli.SandboxLogsCommand(runtimeDir(namespace, pod), follow)
}

// accessReviewer answers whether the user may perform an action on a member,
// as the member's API server decides it.
type accessReviewer interface {
	review(ctx context.Context, cluster string, id Identity, attrs authorizationv1.ResourceAttributes) (allowed bool, reason string, err error)
}

// ssarReviewer asks with a SelfSubjectAccessReview made as the user
// (impersonated), so the member's own authorizers decide.
type ssarReviewer struct{ pool consoleProvider }

func (s ssarReviewer) review(ctx context.Context, cluster string, id Identity, attrs authorizationv1.ResourceAttributes) (bool, string, error) {
	cfg, err := s.pool.RestConfigFor(cluster, id)
	if err != nil {
		return false, "", err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return false, "", err
	}
	res, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, "", err
	}
	return res.Status.Allowed, res.Status.Reason, nil
}

// authorizeUser writes the refusal and returns false unless the user may
// perform attrs on the cluster. The dev-mode insecure authenticator has no
// user to ask about and authorizes everything, as it does for every other
// call.
func authorizeUser(ctx context.Context, w http.ResponseWriter, rv accessReviewer, cluster string, id Identity, attrs authorizationv1.ResourceAttributes) bool {
	if id.empty() {
		return true
	}
	allowed, reason, err := rv.review(ctx, cluster, id, attrs)
	if err != nil {
		http.Error(w, "access review failed: "+err.Error(), http.StatusBadGateway)
		return false
	}
	if !allowed {
		msg := fmt.Sprintf("forbidden: %s cannot %s %s/%s %q in namespace %q",
			id.User, attrs.Verb, attrs.Resource, attrs.Subresource, attrs.Name, attrs.Namespace)
		if reason != "" {
			msg += ": " + reason
		}
		http.Error(w, msg, http.StatusForbidden)
		return false
	}
	return true
}

// launcherPodOf returns the named pod if it carries label=value, the label the
// controller sets on the launcher it creates for that guest or sandbox. The
// pod name comes from the object's status, which only the controller writes;
// the label check keeps the gateway credential from being pointed at any other
// pod in the namespace should the status be written by someone else.
func launcherPodOf(ctx context.Context, cs kubernetes.Interface, namespace, pod, label, value string) (*corev1.Pod, error) {
	p, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if p.Labels[label] != value {
		return nil, fmt.Errorf("pod %s/%s is not the launcher of %s", namespace, pod, value)
	}
	return p, nil
}

// sandboxPodLabel ties a launcher pod to its SwiftSandbox
// (swiftsandbox.SandboxLabelKey); checkout rewrites it on a claimed warm-pool
// slot, so it names the sandbox that holds the pod now.
const sandboxPodLabel = "sandbox.kubeswift.io/sandbox"

// sandboxLauncher authorizes the user for verb on the sandbox's subresource
// and resolves the sandbox's launcher pod as the member credential: its own
// launcher, or the slot pod a warm-pool checkout claimed (status.podRef). It
// returns the member credential's config and clientset to exec with, or writes
// the refusal and returns ok=false.
func sandboxLauncher(ctx context.Context, w http.ResponseWriter, pool consoleProvider, rv accessReviewer,
	cluster string, id Identity, namespace, name, verb, subresource string,
) (cfg *rest.Config, cs kubernetes.Interface, pod string, ok bool) {
	cfg, err := pool.RestConfigFor(cluster, Identity{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return nil, nil, "", false
	}
	if !authorizeUser(ctx, w, rv, cluster, id, authorizationv1.ResourceAttributes{
		Namespace: namespace, Name: name, Verb: verb,
		Group: sandboxGVR.Group, Resource: sandboxGVR.Resource, Subresource: subresource,
	}) {
		return nil, nil, "", false
	}
	dyn, err := pool.DynamicFor(cluster, Identity{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return nil, nil, "", false
	}
	sb, err := dyn.Resource(sandboxGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return nil, nil, "", false
	}
	pod = name
	if ref, _, _ := unstructured.NestedString(sb.Object, "status", "podRef"); ref != "" {
		pod = ref
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, nil, "", false
	}
	if _, err := launcherPodOf(ctx, clientset, namespace, pod, sandboxPodLabel, name); err != nil {
		http.Error(w, "no launcher pod for sandbox: "+err.Error(), http.StatusConflict)
		return nil, nil, "", false
	}
	return cfg, clientset, pod, true
}
