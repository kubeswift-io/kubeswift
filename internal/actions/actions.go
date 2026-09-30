// Package actions implements the shared SwiftGuest lifecycle primitives —
// start, stop, migrate — used by BOTH swiftctl and the kubeswift-gateway.
//
// These actions were implemented twice and drifted: the gateway's StopGuest
// once patched runPolicy=Stopped but forgot to delete the launcher pod, so a
// running VM never stopped (fixed in PR #267, caught only by cluster
// validation). Centralizing the logic here gives the two surfaces a single
// code path, so the next such divergence cannot happen.
//
// The primitives operate over a dynamic.Interface. swiftctl historically used a
// typed controller-runtime client and the gateway a dynamic (per-member,
// impersonating) client; the dynamic client is the common denominator (the
// gateway needs impersonation, swiftctl builds one cheaply from its REST
// config), so it is the single model here. Callers keep their own input
// validation and error mapping (connect codes for the gateway, cobra errors for
// swiftctl) — this package returns plain errors and the patched/created object.
package actions

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

const (
	// GuestLabel ties a launcher pod to its SwiftGuest. The label — not the pod
	// name — is the stable handle: a live-migrated guest's pod is renamed to
	// <guest>-mig-<uid>, but this label still selects it.
	GuestLabel = "swift.kubeswift.io/guest"

	// LauncherContainer is the swiftletd container in the (multi-container)
	// launcher pod — the one holding the serial socket and the VM process.
	LauncherContainer = "launcher"
)

// RunPolicy spec values the start/stop primitives patch. Derived from the API
// enum so there is a single source of truth for the strings.
const (
	RunPolicyRunning = string(swiftv1alpha1.RunPolicyRunning)
	RunPolicyStopped = string(swiftv1alpha1.RunPolicyStopped)
)

// Resource GVRs the primitives act on. Centralized here so the gateway and any
// other dynamic-client caller reference one definition.
var (
	SwiftGuestGVR     = schema.GroupVersionResource{Group: "swift.kubeswift.io", Version: "v1alpha1", Resource: "swiftguests"}
	SwiftMigrationGVR = schema.GroupVersionResource{Group: "migration.kubeswift.io", Version: "v1alpha1", Resource: "swiftmigrations"}
	PodGVR            = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

// SetRunPolicy patches spec.runPolicy on the named SwiftGuest via a merge patch
// and returns the patched object.
func SetRunPolicy(ctx context.Context, dyn dynamic.Interface, namespace, name, policy string) (*unstructured.Unstructured, error) {
	patch := []byte(fmt.Sprintf(`{"spec":{"runPolicy":%q}}`, policy))
	return dyn.Resource(SwiftGuestGVR).Namespace(namespace).
		Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
}

// Start sets runPolicy=Running and removes a launcher pod that has already
// exited, so the SwiftGuest controller creates a new one.
//
// The controller creates a launcher only when the guest has none. A guest that
// was shut down from inside (its pod Succeeded) or whose launcher failed (pod
// Failed) keeps that exited pod, so with runPolicy already Running the patch
// alone changed nothing and the guest stayed down. A running launcher is left
// alone (recreating it is "restart", a distinct operation), and so is one
// that handed its VM to a live migration: the VM runs in the destination pod,
// and the migration removes the source at cutover.
func Start(ctx context.Context, dyn dynamic.Interface, namespace, name string) (*unstructured.Unstructured, error) {
	u, err := SetRunPolicy(ctx, dyn, namespace, name, RunPolicyRunning)
	if err != nil {
		return nil, err
	}
	if err := deleteExitedLauncherPods(ctx, dyn, namespace, name); err != nil {
		return nil, fmt.Errorf("runPolicy patched to Running but removing the exited launcher pod failed: %w", err)
	}
	return u, nil
}

// podAnnotationMigrationStatus is swiftletd's report on a migration launcher,
// the same key as swiftguest.PodAnnotationMigrationStatus. "complete" on a
// source pod means its VM now runs in the destination.
const podAnnotationMigrationStatus = "kubeswift.io/migration-status"

// deleteExitedLauncherPods deletes the guest's launcher pods that have exited
// and did not hand their VM to a migration. A pod already gone is success.
//
// A caller that may not list pods gets what start did before it removed
// exited launchers: the patch alone. The UI's Manage VMs capability grants
// the pod access, as Stop needs it too.
func deleteExitedLauncherPods(ctx context.Context, dyn dynamic.Interface, namespace, guestName string) error {
	pods, err := dyn.Resource(PodGVR).Namespace(namespace).
		List(ctx, metav1.ListOptions{LabelSelector: GuestLabel + "=" + guestName})
	if apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
		if phase != string(corev1.PodSucceeded) && phase != string(corev1.PodFailed) {
			continue
		}
		if pod.GetDeletionTimestamp() != nil || pod.GetAnnotations()[podAnnotationMigrationStatus] == "complete" {
			continue
		}
		if err := dyn.Resource(PodGVR).Namespace(namespace).
			Delete(ctx, pod.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// Stop sets runPolicy=Stopped AND deletes the launcher pod(s). The SwiftGuest
// controller stops a running guest whose runPolicy is Stopped on its own, by
// deleting its launcher the same way, so the patch alone stops the guest too;
// deleting the pod here stops it at once rather than on the controller's next
// reconcile. Unlike the controller, Stop does not wait for a migration,
// restore or snapshot capture of the guest to finish. Deleting the launcher
// pod triggers swiftletd's graceful SIGTERM shutdown (an ACPI power-off within
// the pod's termination grace period); the controller then keeps it stopped.
//
// Stop is idempotent: the patch is the same each time, a pod already gone is
// success, and deleting a terminating pod again does not change its shutdown.
//
// The returned object is the runPolicy=Stopped patch result.
func Stop(ctx context.Context, dyn dynamic.Interface, namespace, name string) (*unstructured.Unstructured, error) {
	u, err := SetRunPolicy(ctx, dyn, namespace, name, RunPolicyStopped)
	if err != nil {
		return nil, err
	}
	if err := DeleteLauncherPods(ctx, dyn, namespace, name); err != nil {
		return nil, fmt.Errorf("runPolicy patched to Stopped but stopping the VM (pod delete) failed: %w", err)
	}
	return u, nil
}

// DeleteLauncherPods deletes the guest's launcher pod(s), selected by GuestLabel
// (robust to the <guest>-mig-<uid> rename after a live migration). A pod that is
// already gone is success — the VM is already stopped.
func DeleteLauncherPods(ctx context.Context, dyn dynamic.Interface, namespace, guestName string) error {
	pods, err := dyn.Resource(PodGVR).Namespace(namespace).
		List(ctx, metav1.ListOptions{LabelSelector: GuestLabel + "=" + guestName})
	if err != nil {
		return err
	}
	for i := range pods.Items {
		name := pods.Items[i].GetName()
		if err := dyn.Resource(PodGVR).Namespace(namespace).
			Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// MigrateParams describes a SwiftMigration to create. The variable bits — the
// resource name, optional timeout/ttl, the reason string — let swiftctl and the
// UI shape the record while sharing the spec construction.
type MigrateParams struct {
	Namespace     string
	GuestName     string
	TargetNode    string
	Mode          string // auto | live | offline; empty resolves to auto
	AllowIPChange bool
	Reason        string // free-text spec.reason; omitted when empty

	// Name vs GenerateName: set at most one. If both are empty, GenerateName
	// defaults to "<guest>-migrate-".
	Name         string
	GenerateName string

	// Optional spec.timeout / spec.ttl. Zero omits the field, letting the CRD
	// defaults apply (spec.timeout defaults to 30m; spec.ttl unset = keep).
	Timeout time.Duration
	TTL     time.Duration
}

// Migrate creates a SwiftMigration for the guest named in p and returns the
// created object (its generated name is on metadata.name). Callers validate
// inputs (e.g. a required target node) and map errors to their own surface — a
// webhook admission denial (allowIPChange required for a cross-node move, or
// live+VFIO) propagates here, never a silent failure.
func Migrate(ctx context.Context, dyn dynamic.Interface, p MigrateParams) (*unstructured.Unstructured, error) {
	mode := p.Mode
	if mode == "" {
		mode = string(migrationv1alpha1.SwiftMigrationModeAuto)
	}

	meta := map[string]interface{}{"namespace": p.Namespace}
	switch {
	case p.Name != "":
		meta["name"] = p.Name
	case p.GenerateName != "":
		meta["generateName"] = p.GenerateName
	default:
		meta["generateName"] = p.GuestName + "-migrate-"
	}

	spec := map[string]interface{}{
		"guestRef":      map[string]interface{}{"name": p.GuestName},
		"target":        map[string]interface{}{"nodeName": p.TargetNode},
		"mode":          mode,
		"allowIPChange": p.AllowIPChange,
	}
	if p.Reason != "" {
		spec["reason"] = p.Reason
	}
	// metav1.Duration marshals as a Go duration string (e.g. "10m0s"); the
	// apiserver parses it back into the CRD's *metav1.Duration field.
	if p.Timeout > 0 {
		spec["timeout"] = p.Timeout.String()
	}
	if p.TTL > 0 {
		spec["ttl"] = p.TTL.String()
	}

	mig := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "migration.kubeswift.io/v1alpha1",
		"kind":       "SwiftMigration",
		"metadata":   meta,
		"spec":       spec,
	}}
	return dyn.Resource(SwiftMigrationGVR).Namespace(p.Namespace).
		Create(ctx, mig, metav1.CreateOptions{})
}
