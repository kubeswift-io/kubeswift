package swiftguestpool

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// Reconcile maintains the desired number of SwiftGuest replicas for a pool,
// handles rolling updates, and manages per-replica PVCs.
func (r *SwiftGuestPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool swiftv1alpha1.SwiftGuestPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !pool.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	owned, err := r.listOwnedGuests(ctx, &pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	indexMap := buildIndexMap(pool.Name, owned)
	desired := int(pool.Spec.Replicas)
	currentHash := computeTemplateHash(&pool.Spec.Template)
	requeueNeeded := false

	// --- Failed VM replacement ---
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

	// --- Scale down: delete highest indices first ---
	excess := findExcessIndices(indexMap, desired)
	for _, idx := range excess {
		guest := indexMap[idx]
		klog.InfoS("scaling down", "pool", pool.Name, "guest", guest.Name, "index", idx)
		if err := r.Delete(ctx, &guest); err != nil {
			return ctrl.Result{}, err
		}
		delete(indexMap, idx)
	}

	// --- Scale up: create missing indices ---
	missing := findMissingIndices(indexMap, desired)
	for _, idx := range missing {
		klog.InfoS("creating replica", "pool", pool.Name, "index", idx)
		if err := r.createSwiftGuest(ctx, &pool, idx, currentHash); err != nil {
			return ctrl.Result{}, err
		}
	}

	// --- Rolling updates ---
	if r.hasOutdatedGuests(indexMap, currentHash) {
		requeue, err := r.reconcileRollingUpdate(ctx, &pool, indexMap, currentHash, desired)
		if err != nil {
			return ctrl.Result{}, err
		}
		if requeue {
			requeueNeeded = true
		}
	}

	// --- Update status ---
	owned, err = r.listOwnedGuests(ctx, &pool)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.updateStatus(ctx, &pool, owned, currentHash); err != nil {
		return ctrl.Result{}, err
	}

	if requeueNeeded {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// --- Rolling Update ---

func (r *SwiftGuestPoolReconciler) hasOutdatedGuests(indexMap map[int]swiftv1alpha1.SwiftGuest, currentHash string) bool {
	for _, g := range indexMap {
		if g.Annotations[swiftv1alpha1.AnnotationTemplateHash] != currentHash {
			return true
		}
	}
	return false
}

func (r *SwiftGuestPoolReconciler) reconcileRollingUpdate(
	ctx context.Context,
	pool *swiftv1alpha1.SwiftGuestPool,
	indexMap map[int]swiftv1alpha1.SwiftGuest,
	currentHash string,
	desired int,
) (requeue bool, err error) {
	strategy := swiftv1alpha1.UpdateStrategyRollingUpdate
	maxUnavailable := int32(1)
	maxSurge := int32(0)
	if pool.Spec.UpdateStrategy != nil {
		strategy = pool.Spec.UpdateStrategy.Type
		if pool.Spec.UpdateStrategy.RollingUpdate != nil {
			maxUnavailable = pool.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable
			maxSurge = pool.Spec.UpdateStrategy.RollingUpdate.MaxSurge
		}
	}

	// Identify outdated guests (highest index first for deletion order).
	var outdated []int
	for idx, g := range indexMap {
		if g.Annotations[swiftv1alpha1.AnnotationTemplateHash] != currentHash {
			outdated = append(outdated, idx)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(outdated)))

	if strategy == swiftv1alpha1.UpdateStrategyRecreate {
		// Delete all outdated at once.
		for _, idx := range outdated {
			guest := indexMap[idx]
			klog.InfoS("recreate update: deleting outdated guest",
				"pool", pool.Name, "guest", guest.Name)
			if err := r.Delete(ctx, &guest); err != nil {
				return false, err
			}
		}
		return true, nil
	}

	// RollingUpdate: one at a time, respecting maxUnavailable and maxSurge.
	total := int32(len(indexMap))
	readyCount := int32(0)
	for _, g := range indexMap {
		if g.Status.Phase == swiftv1alpha1.SwiftGuestPhaseRunning {
			for _, c := range g.Status.Conditions {
				if c.Type == "GuestRunning" && c.Status == metav1.ConditionTrue {
					readyCount++
					break
				}
			}
		}
	}

	unavailable := total - readyCount
	surge := total - int32(desired)

	// Delete one outdated guest if we can tolerate more unavailability.
	if len(outdated) > 0 && unavailable < maxUnavailable {
		idx := outdated[0]
		guest := indexMap[idx]
		klog.InfoS("rolling update: deleting outdated guest",
			"pool", pool.Name, "guest", guest.Name, "index", idx,
			"unavailable", unavailable, "maxUnavailable", maxUnavailable)
		if err := r.Delete(ctx, &guest); err != nil {
			return false, err
		}
		return true, nil
	}

	// Create a replacement if we can tolerate more surge.
	missing := findMissingIndices(indexMap, desired)
	if len(missing) > 0 && surge < maxSurge {
		idx := missing[0]
		klog.InfoS("rolling update: creating replacement",
			"pool", pool.Name, "index", idx,
			"surge", surge, "maxSurge", maxSurge)
		if err := r.createSwiftGuest(ctx, pool, idx, currentHash); err != nil {
			return false, err
		}
		return true, nil
	}

	// Still have outdated guests but can't act yet -- requeue.
	return true, nil
}

// --- Guest Creation ---

func (r *SwiftGuestPoolReconciler) createSwiftGuest(ctx context.Context, pool *swiftv1alpha1.SwiftGuestPool, index int, templateHash string) error {
	name := fmt.Sprintf("%s-%d", pool.Name, index)

	// Ensure per-replica PVCs exist.
	var dataDiskRefs []swiftv1alpha1.DataDiskRef
	for _, vct := range pool.Spec.VolumeClaimTemplates {
		pvcName, err := r.ensurePVC(ctx, pool, vct, index)
		if err != nil {
			return err
		}
		dataDiskRefs = append(dataDiskRefs, swiftv1alpha1.DataDiskRef{
			Name:   vct.Metadata.Name,
			PVCRef: &corev1.LocalObjectReference{Name: pvcName},
		})
	}

	// Merge template labels with controller-managed labels.
	labels := make(map[string]string)
	for k, v := range pool.Spec.Template.Metadata.Labels {
		labels[k] = v
	}
	labels[swiftv1alpha1.LabelPoolName] = pool.Name
	labels[swiftv1alpha1.LabelPoolIndex] = strconv.Itoa(index)

	// Merge template annotations with template hash.
	annotations := make(map[string]string)
	for k, v := range pool.Spec.Template.Metadata.Annotations {
		annotations[k] = v
	}
	annotations[swiftv1alpha1.AnnotationTemplateHash] = templateHash

	spec := pool.Spec.Template.Spec.DeepCopy()

	// cloneFromSnapshot (Snapshot Phase 4): pre-assign each replica's targetNode
	// (round-robin across schedulable worker nodes) so a Tier C clone's download
	// + restore-receive land on a decided node. No-op for non-clone templates;
	// ignored by the SwiftGuest controller for Tier B clones (capture-node-pinned).
	if err := r.assignCloneTargetNode(ctx, spec, index); err != nil {
		return fmt.Errorf("assign clone target node for %s: %w", name, err)
	}

	// Apply topology spread constraints.
	spec.TopologySpreadConstraints = r.buildTopologyConstraints(pool)

	// Apply per-replica data disk refs.
	if len(dataDiskRefs) > 0 {
		spec.DataDiskRefs = append(spec.DataDiskRefs, dataDiskRefs...)
	}

	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   pool.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: *spec,
	}

	if err := controllerutil.SetControllerReference(pool, guest, r.Client.Scheme()); err != nil {
		return fmt.Errorf("set owner reference: %w", err)
	}

	if err := r.Create(ctx, guest); err != nil {
		return fmt.Errorf("create SwiftGuest %s: %w", name, err)
	}

	return nil
}

// --- Topology Spread ---

func (r *SwiftGuestPoolReconciler) buildTopologyConstraints(pool *swiftv1alpha1.SwiftGuestPool) []corev1.TopologySpreadConstraint {
	// Explicit constraints take precedence.
	if len(pool.Spec.TopologySpreadConstraints) > 0 {
		constraints := make([]corev1.TopologySpreadConstraint, len(pool.Spec.TopologySpreadConstraints))
		copy(constraints, pool.Spec.TopologySpreadConstraints)
		// Add label selector if not already present.
		for i := range constraints {
			if constraints[i].LabelSelector == nil {
				constraints[i].LabelSelector = &metav1.LabelSelector{
					MatchLabels: map[string]string{
						swiftv1alpha1.LabelPoolName: pool.Name,
					},
				}
			}
		}
		return constraints
	}

	// SpreadPolicy shorthand.
	if pool.Spec.SpreadPolicy == swiftv1alpha1.SpreadPolicySpread {
		return []corev1.TopologySpreadConstraint{
			{
				MaxSkew:           1,
				TopologyKey:       "kubernetes.io/hostname",
				WhenUnsatisfiable: corev1.ScheduleAnyway,
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						swiftv1alpha1.LabelPoolName: pool.Name,
					},
				},
			},
		}
	}

	return nil
}

// --- Owned Guest Listing ---

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

// --- Index Management ---

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

func findMissingIndices(indexMap map[int]swiftv1alpha1.SwiftGuest, desired int) []int {
	var missing []int
	for i := 0; i < desired; i++ {
		if _, ok := indexMap[i]; !ok {
			missing = append(missing, i)
		}
	}
	return missing
}

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

// --- Status ---

func (r *SwiftGuestPoolReconciler) updateStatus(ctx context.Context, pool *swiftv1alpha1.SwiftGuestPool, owned []swiftv1alpha1.SwiftGuest, currentHash string) error {
	var ready, failed, updated int32
	for _, g := range owned {
		switch g.Status.Phase {
		case swiftv1alpha1.SwiftGuestPhaseRunning:
			for _, c := range g.Status.Conditions {
				if c.Type == "GuestRunning" && c.Status == metav1.ConditionTrue {
					ready++
					break
				}
			}
		case swiftv1alpha1.SwiftGuestPhaseFailed:
			failed++
		}
		if g.Annotations[swiftv1alpha1.AnnotationTemplateHash] == currentHash {
			updated++
		}
	}

	total := int32(len(owned))
	desired := pool.Spec.Replicas

	patch := client.MergeFrom(pool.DeepCopy())
	pool.Status.Replicas = total
	pool.Status.ReadyReplicas = ready
	pool.Status.AvailableReplicas = ready
	pool.Status.FailedReplicas = failed
	pool.Status.UpdatedReplicas = updated
	pool.Status.CurrentTemplateHash = currentHash

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
	setCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               swiftv1alpha1.PoolConditionUpdated,
		Status:             condBool(updated == desired && total == desired),
		LastTransitionTime: now,
		Reason:             updatedReason(updated, desired),
		Message:            fmt.Sprintf("%d/%d replicas updated", updated, desired),
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

func updatedReason(updated, desired int32) string {
	if updated == desired {
		return "AllReplicasUpdated"
	}
	return "RollingUpdateInProgress"
}

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
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
