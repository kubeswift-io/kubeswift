package swiftsandbox

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

const (
	// reasonRunAsNonRootViolation fails a sandbox whose runAsNonRoot the
	// controller can already see is broken: its user is uid 0 or root.
	reasonRunAsNonRootViolation = "RunAsNonRootViolation"
	// reasonImageUserIgnored is the Warning Event for a sandbox whose image
	// names a USER that its kernel's bridge cannot honour: it runs as root.
	reasonImageUserIgnored = "ImageUserIgnored"
	// runAsUserFeature is the bridge feature that honours a workload user.
	runAsUserFeature = "run-as-user"
)

// workloadUser is who the workload runs as, with the Pod rules: runAsUser
// replaces the image USER's user, runAsGroup its group; whatever is unset
// follows the image. Nil when that is root with no restriction, which is what
// a sandbox ran as before the field existed.
func workloadUser(sc *sandboxv1alpha1.SandboxSecurityContext, imageUser string) *runtimeintent.WorkloadUser {
	u, g, _ := strings.Cut(strings.TrimSpace(imageUser), ":")
	w := &runtimeintent.WorkloadUser{}
	if sc != nil {
		if sc.RunAsUser != nil {
			u = strconv.FormatInt(*sc.RunAsUser, 10)
		}
		if sc.RunAsGroup != nil {
			g = strconv.FormatInt(*sc.RunAsGroup, 10)
		}
		w.NonRoot = sc.RunAsNonRoot != nil && *sc.RunAsNonRoot
		w.NoNewPrivs = sc.AllowPrivilegeEscalation != nil && !*sc.AllowPrivilegeEscalation
		w.Explicit = sc.RunAsUser != nil || sc.RunAsGroup != nil || w.NonRoot || w.NoNewPrivs
	}
	switch {
	case g != "" && u == "":
		w.User = "0:" + g
	case g != "":
		w.User = u + ":" + g
	default:
		w.User = u
	}
	if w.User == "" && !w.NonRoot && !w.NoNewPrivs {
		return nil
	}
	return w
}

// runAsNonRootViolation is the refusal for a runAsNonRoot workload the
// controller can already see runs as root: no user, user 0, or "root". A
// user name is resolved in the guest, which refuses it there.
func runAsNonRootViolation(w *runtimeintent.WorkloadUser) (string, bool) {
	if w == nil || !w.NonRoot {
		return "", false
	}
	u, _, _ := strings.Cut(w.User, ":")
	if u == "" || u == "0" || u == "root" {
		who := u
		if who == "" {
			who = "root (the image names no USER)"
		}
		return fmt.Sprintf("securityContext.runAsNonRoot is set but the workload would run as %s; set runAsUser or use an image whose USER is not root", who), true
	}
	return "", false
}

// userString is what status.user shows.
func userString(w *runtimeintent.WorkloadUser) string {
	if w == nil {
		return ""
	}
	return w.User
}

// bridgeHas reports whether a launcher's kernel bridge lists feature. A pod
// whose launcher advertises nothing (older swiftletd, or not started yet)
// lists nothing.
func bridgeHas(p *corev1.Pod, feature string) bool {
	return slices.Contains(strings.Fields(p.Annotations[bridgeFeaturesAnnotation]), feature)
}

// imageUserIgnored is the Warning for a workload whose user came only from the
// image and whose launcher's bridge cannot honour it, so swiftletd dropped it
// and the workload runs as root. Empty when there is nothing to say: the
// launcher has not advertised its bridge yet, or the bridge honours it.
func imageUserIgnored(w *runtimeintent.WorkloadUser, p *corev1.Pod) string {
	if w == nil || w.Explicit || w.User == "" {
		return ""
	}
	if _, ok := p.Annotations[bridgeFeaturesAnnotation]; !ok || bridgeHas(p, runAsUserFeature) {
		return ""
	}
	return fmt.Sprintf("the image runs as user %q but the sandbox kernel's bridge cannot run a workload as another user, so it runs as root; point the SwiftKernel at kernels/sandbox 6.6.21 or kernels/gpu-sandbox 6.6.11 or newer", w.User)
}

// statusWorkloadUser is the workload user a running sandbox recorded, with
// whether its spec asked for it.
func statusWorkloadUser(sb *sandboxv1alpha1.SwiftSandbox) *runtimeintent.WorkloadUser {
	if sb.Status.User == "" {
		return nil
	}
	w := workloadUser(sb.Spec.SecurityContext, "")
	explicit := w != nil && w.Explicit
	return &runtimeintent.WorkloadUser{User: sb.Status.User, Explicit: explicit}
}
