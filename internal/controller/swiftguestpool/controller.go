package swiftguestpool

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// SwiftGuestPoolReconciler reconciles SwiftGuestPool objects.
type SwiftGuestPoolReconciler struct {
	client.Client
}

// Reconcile maintains the desired number of SwiftGuest replicas for a pool.
func (r *SwiftGuestPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool swiftv1alpha1.SwiftGuestPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Pool being deleted -- let garbage collection handle owned SwiftGuests.
	if !pool.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// List all SwiftGuests owned by this pool.
	owned, err := r.listOwnedGuests(ctx, &pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Build index map: index -> SwiftGuest.
	indexMap := buildIndexMap(pool.Name, owned)

	desired := int(pool.Spec.Replicas)

	// Delete failed guests (they'll be recreated as missing indices).
	for idx, guest := range indexMap {
		if idx < desired && guest.Status.Phase == swiftv1alpha1.SwiftGuestPhaseFailed {
			policy := guest.Spec.RunPolicy
			if policy == swiftv1alpha1.RunPolicyRunning ||
				policy == swiftv1alpha1.RunPolicyAlways ||
				policy == swiftv1alpha1.RunPolicyRestartOnFailure {
				klog.InfoS("deleting failed guest for replacement",
					"pool", pool.Name, "guest", guest.Name, "index", idx)
				if err := r.Delete(ctx, &guest); err != nil {
					return ctrl.Result{}, err
				}
				delete(indexMap, idx)
			}
		}
	}

	// Scale down: delete highest indices first.
	excess := findExcessIndices(indexMap, desired)
	for _, idx := range excess {
		guest := indexMap[idx]
		klog.InfoS("scaling down", "pool", pool.Name, "guest", guest.Name, "index", idx)
		if err := r.Delete(ctx, &guest); err != nil {
			return ctrl.Result{}, err
		}
		delete(indexMap, idx)
	}

	// Scale up: create missing indices.
	missing := findMissingIndices(indexMap, desired)
	for _, idx := range missing {
		klog.InfoS("creating replica", "pool", pool.Name, "index", idx)
		if err := r.createSwiftGuest(ctx, &pool, idx); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Re-list after mutations for accurate status.
	owned, err = r.listOwnedGuests(ctx, &pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update status.
	if err := r.updateStatus(ctx, &pool, owned); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// listOwnedGuests returns all SwiftGuests with an ownerReference to this pool.
func (r *SwiftGuestPoolReconciler) listOwnedGuests(ctx context.Context, pool *swiftv1alpha1.SwiftGuestPool) ([]swiftv1alpha1.SwiftGuest, error) {
	var list swiftv1alpha1.SwiftGuestList
	if err := r.List(ctx, &list, client.InNamespace(pool.Namespace)); err != nil {
		return nil, err
	}
	var owned []swiftv1alpha1.SwiftGuest
	for _, g := range list.Items {
		if metav1.IsControlledBy(&g, pool) {
			owned = append(owned, g)
		}
	}
	return owned, nil
}

// buildIndexMap parses the replica index from each SwiftGuest name and builds a map.
func buildIndexMap(poolName string, guests []swiftv1alpha1.SwiftGuest) map[int]swiftv1alpha1.SwiftGuest {
	m := make(map[int]swiftv1alpha1.SwiftGuest)
	prefix := poolName + "-"
	for _, g := range guests {
		if !strings.HasPrefix(g.Name, prefix) {
			continue
		}
		idxStr := strings.TrimPrefix(g.Name, prefix)
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		m[idx] = g
	}
	return m
}

// findMissingIndices returns indices in [0, desired) that have no SwiftGuest.
func findMissingIndices(indexMap map[int]swiftv1alpha1.SwiftGuest, desired int) []int {
	var missing []int
	for i := 0; i < desired; i++ {
		if _, ok := indexMap[i]; !ok {
			missing = append(missing, i)
		}
	}
	return missing
}

// findExcessIndices returns indices >= desired, sorted highest-first for deletion order.
func findExcessIndices(indexMap map[int]swiftv1alpha1.SwiftGuest, desired int) []int {
	var excess []int
	for idx := range indexMap {
		if idx >= desired {
			excess = append(excess, idx)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(excess)))
	return excess
}

// createSwiftGuest builds and creates a SwiftGuest for the given pool and index.
func (r *SwiftGuestPoolReconciler) createSwiftGuest(ctx context.Context, pool *swiftv1alpha1.SwiftGuestPool, index int) error {
	name := fmt.Sprintf("%s-%d", pool.Name, index)

	// Merge template labels with controller-managed labels.
	labels := make(map[string]string)
	for k, v := range pool.Spec.Template.Metadata.Labels {
		labels[k] = v
	}
	labels[swiftv1alpha1.LabelPoolName] = pool.Name
	labels[swiftv1alpha1.LabelPoolIndex] = strconv.Itoa(index)

	// Copy template annotations.
	var annotations map[string]string
	if len(pool.Spec.Template.Metadata.Annotations) > 0 {
		annotations = make(map[string]string)
		for k, v := range pool.Spec.Template.Metadata.Annotations {
			annotations[k] = v
		}
	}

	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   pool.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: *pool.Spec.Template.Spec.DeepCopy(),
	}

	if err := controllerutil.SetControllerReference(pool, guest, r.Client.Scheme()); err != nil {
		return fmt.Errorf("set owner reference: %w", err)
	}

	if err := r.Create(ctx, guest); err != nil {
		return fmt.Errorf("create SwiftGuest %s: %w", name, err)
	}

	return nil
}

// updateStatus computes and patches the pool status from owned SwiftGuests.
func (r *SwiftGuestPoolReconciler) updateStatus(ctx context.Context, pool *swiftv1alpha1.SwiftGuestPool, owned []swiftv1alpha1.SwiftGuest) error {
	var ready, failed int32
	for _, g := range owned {
		switch g.Status.Phase {
		case swiftv1alpha1.SwiftGuestPhaseRunning:
			// Check GuestRunning condition.
			for _, c := range g.Status.Conditions {
				if c.Type == "GuestRunning" && c.Status == metav1.ConditionTrue {
					ready++
					break
				}
			}
		case swiftv1alpha1.SwiftGuestPhaseFailed:
			failed++
		}
	}

	total := int32(len(owned))
	desired := pool.Spec.Replicas

	patch := client.MergeFrom(pool.DeepCopy())
	pool.Status.Replicas = total
	pool.Status.ReadyReplicas = ready
	pool.Status.AvailableReplicas = ready
	pool.Status.FailedReplicas = failed

	// Conditions.
	now := metav1.Now()
	setCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               swiftv1alpha1.PoolConditionAvailable,
		Status:             condBool(ready > 0),
		LastTransitionTime: now,
		Reason:             availableReason(ready),
		Message:            fmt.Sprintf("%d/%d replicas ready", ready, desired),
	})
	setCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               swiftv1alpha1.PoolConditionProgressing,
		Status:             condBool(total != desired || failed > 0),
		LastTransitionTime: now,
		Reason:             progressingReason(total, desired, failed),
		Message:            fmt.Sprintf("replicas=%d desired=%d failed=%d", total, desired, failed),
	})

	return r.Status().Patch(ctx, pool, patch)
}

func condBool(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func availableReason(ready int32) string {
	if ready > 0 {
		return "MinimumReplicasAvailable"
	}
	return "NoReplicasAvailable"
}

func progressingReason(total, desired int32, failed int32) string {
	if failed > 0 {
		return "FailedReplicasExist"
	}
	if total < desired {
		return "NewReplicasCreated"
	}
	if total > desired {
		return "ScalingDown"
	}
	return "ReplicasUpToDate"
}

// setCondition updates or appends a condition.
func setCondition(conditions *[]metav1.Condition, cond metav1.Condition) {
	for i, existing := range *conditions {
		if existing.Type == cond.Type {
			if existing.Status != cond.Status || existing.Reason != cond.Reason {
				(*conditions)[i] = cond
			}
			return
		}
	}
	*conditions = append(*conditions, cond)
}

// SetupWithManager registers the controller.
func (r *SwiftGuestPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("swiftguestpool").
		For(&swiftv1alpha1.SwiftGuestPool{}).
		Owns(&swiftv1alpha1.SwiftGuest{}).
		Complete(r)
}
