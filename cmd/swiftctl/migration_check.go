package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	swiftmigration "github.com/projectbeskar/kubeswift/internal/controller/swiftmigration"
)

// nfdCPUIDLabelPrefix is the node-feature-discovery label prefix for CPUID
// feature flags (e.g. feature.node.kubernetes.io/cpu-cpuid.AVX512F=true). When
// present on both nodes we can diff the flag sets; otherwise CPU-feature
// uniformity is an operator runbook concern (compare `lscpu` by hand).
const nfdCPUIDLabelPrefix = "feature.node.kubernetes.io/cpu-cpuid."

// runMigratePreflight reports, without creating anything, whether a
// `swiftctl migrate <guest> --to <node>` is likely to succeed: target node
// readiness + capacity, IP preservation, the mode that would be picked, and
// CPU/architecture compatibility. Mirrors the Phase-1 target-node-Ready
// ergonomic — warnings, not hard rejections, except for outright blockers
// (guest/target missing, live requested for a VFIO guest). Returns a non-nil
// error (exit 1) only when a hard blocker is present.
func runMigratePreflight(cmd *cobra.Command, c client.Client, guestName, ns string, mode migrationv1alpha1.SwiftMigrationMode) error {
	ctx := context.Background()
	out := cmd.OutOrStdout()
	target := migrateTargetNode

	const (
		ok   = "[ OK ]"
		warn = "[WARN]"
		info = "[INFO]"
		fail = "[FAIL]"
	)
	blockers := 0
	line := func(status, format string, a ...interface{}) {
		fmt.Fprintf(out, "%s %s\n", status, fmt.Sprintf(format, a...))
	}

	fmt.Fprintf(out, "Preflight: migrate %s/%s --to %q\n\n", ns, guestName, target)

	// 1. Guest exists.
	var guest swiftv1alpha1.SwiftGuest
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: guestName}, &guest); err != nil {
		line(fail, "SwiftGuest %s/%s not found: %v", ns, guestName, err)
		return fmt.Errorf("preflight failed: guest not found")
	}
	line(ok, "SwiftGuest %s/%s found (phase=%s)", ns, guestName, guest.Status.Phase)

	srcNode := guest.Status.NodeName
	switch {
	case srcNode == "":
		line(warn, "source node unknown (guest not scheduled / not Running)")
	case srcNode == target:
		line(warn, "target node %q is the guest's current node — nothing to migrate", target)
	default:
		line(info, "source node: %s", srcNode)
	}

	// 2. Target node: exists, Ready, capacity.
	var tnode corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: target}, &tnode); err != nil {
		if apierrors.IsNotFound(err) {
			line(fail, "target node %q not found", target)
		} else {
			line(fail, "target node %q lookup error: %v", target, err)
		}
		blockers++
	} else {
		if isNodeReady(&tnode) {
			line(ok, "target node %q is Ready", target)
		} else {
			line(warn, "target node %q is NOT Ready (cordoned/NotReady) — scheduling may not succeed", target)
		}
		// Capacity (reuses the controller's check against the guest's class).
		if class, err := guestClass(ctx, c, &guest); err != nil {
			line(info, "capacity not checked: %v", err)
		} else if err := swiftmigration.NodeHasCapacity(ctx, c, &tnode, class); err != nil {
			line(warn, "target node capacity: %v", err)
		} else {
			line(ok, "target node %q has capacity for the guest (%s CPU / %s memory)",
				target, class.Spec.CPU.String(), class.Spec.Memory.String())
		}
	}

	// 3. IP preservation.
	switch {
	case guest.PrimaryIPPreservedCrossNode():
		line(ok, "primary IP rides a multi-node NAD — preserved across the move")
	case migrateAllowIPChange:
		line(warn, "primary IP will change on the target (you passed --allow-ip-change)")
	default:
		line(warn, "primary IP will change cross-node (default node-local networking); "+
			"pass --allow-ip-change, or put the primary on a multi-node NAD (primary: true + networkRef)")
	}

	// 4. Mode / VFIO.
	vfio := guest.HasVFIODevices() || guest.HasSRIOVInterface()
	nodeLocalBackend := len(guest.Spec.Filesystems) > 0 || len(guest.Spec.VhostUserDevices) > 0
	switch {
	case mode == migrationv1alpha1.SwiftMigrationModeLive && vfio:
		line(fail, "live migration requested but the guest has VFIO/SR-IOV devices — not supported; use --preferred-mode offline")
		blockers++
	case vfio:
		line(info, "guest has VFIO/SR-IOV devices — only offline migration is possible (mode will resolve to offline)")
	case mode == migrationv1alpha1.SwiftMigrationModeLive:
		line(info, "mode: live (requested)")
	case mode == migrationv1alpha1.SwiftMigrationModeOffline:
		line(info, "mode: offline (requested)")
	default:
		line(info, "mode: auto — the controller live-migrates when eligible, else offline")
	}
	if nodeLocalBackend {
		line(warn, "guest has virtiofs/vhost-user devices backed by node-local state — the target node must provide the same backends (sockets/shares); these do not move with the guest")
	}

	// 5. Architecture + CPU features (source vs target).
	if srcNode != "" && srcNode != target {
		var snode corev1.Node
		if err := c.Get(ctx, client.ObjectKey{Name: srcNode}, &snode); err != nil {
			line(info, "source node %q not readable for CPU comparison: %v", srcNode, err)
		} else {
			checkCPUCompat(line, ok, warn, info, &snode, &tnode)
		}
	} else {
		line(info, "CPU/arch comparison skipped (no distinct source node); verify source and target expose identical `lscpu` flags")
	}

	fmt.Fprintln(out)
	if blockers > 0 {
		fmt.Fprintf(out, "Preflight: %d blocker(s) — this migration would be rejected.\n", blockers)
		return fmt.Errorf("preflight failed: %d blocker(s)", blockers)
	}
	fmt.Fprintln(out, "Preflight: no blockers. Re-run without --check to create the migration.")
	return nil
}

// checkCPUCompat compares architecture and (when NFD labels are present) the
// CPUID feature-flag sets of the source and target nodes. CPU flags are not in
// the Node API, so without NFD this is an advisory.
func checkCPUCompat(line func(string, string, ...interface{}), ok, warn, info string, src, dst *corev1.Node) {
	if a, b := src.Status.NodeInfo.Architecture, dst.Status.NodeInfo.Architecture; a != "" && a != b {
		line(warn, "architecture mismatch: source=%s target=%s — migration will fail", a, b)
	} else if a != "" {
		line(ok, "architecture matches (%s)", a)
	}

	srcFlags := nfdCPUFlags(src)
	dstFlags := nfdCPUFlags(dst)
	if len(srcFlags) == 0 || len(dstFlags) == 0 {
		line(info, "CPU-feature uniformity not auto-verifiable (node-feature-discovery labels absent); "+
			"compare `lscpu` flags across the nodes — CPU-feature mismatch is the realistic live-migration failure mode")
		return
	}
	var missing []string
	for f := range srcFlags {
		if !dstFlags[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		line(ok, "target CPU exposes all %d source CPUID features (NFD)", len(srcFlags))
	} else {
		sort.Strings(missing)
		line(warn, "target is MISSING %d source CPU feature(s): %s — the guest may fault on resume",
			len(missing), strings.Join(missing, ","))
	}
}

// nfdCPUFlags returns the set of CPUID feature flags a node advertises via
// node-feature-discovery labels.
func nfdCPUFlags(n *corev1.Node) map[string]bool {
	out := map[string]bool{}
	for k, v := range n.Labels {
		if v == "true" && strings.HasPrefix(k, nfdCPUIDLabelPrefix) {
			out[strings.TrimPrefix(k, nfdCPUIDLabelPrefix)] = true
		}
	}
	return out
}

// guestClass fetches the SwiftGuestClass referenced by the guest (cluster-scoped).
func guestClass(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest) (*swiftv1alpha1.SwiftGuestClass, error) {
	if guest.Spec.GuestClassRef.Name == "" {
		return nil, fmt.Errorf("guest has no guestClassRef")
	}
	var class swiftv1alpha1.SwiftGuestClass
	if err := c.Get(ctx, client.ObjectKey{Name: guest.Spec.GuestClassRef.Name}, &class); err != nil {
		return nil, fmt.Errorf("SwiftGuestClass %q: %w", guest.Spec.GuestClassRef.Name, err)
	}
	return &class, nil
}

// isNodeReady reports whether the node's Ready condition is True.
func isNodeReady(n *corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
