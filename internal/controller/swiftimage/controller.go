package swiftimage

import (
	"context"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/metrics"
)

// SwiftImageReconciler reconciles SwiftImage resources.
type SwiftImageReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Converter ImageConverter
	Clientset kubernetes.Interface
	// VolumeSnapshotEnabled gates the Owns(VolumeSnapshot) watch. When the CSI
	// external-snapshotter CRDs (snapshot.storage.k8s.io/v1) are absent, that watch
	// cannot sync its cache and the manager fatally exits. Set from a one-time
	// discovery check in main. When false, cloneStrategy=snapshot is unavailable;
	// the import pipeline and cloneStrategy=copy are unaffected.
	VolumeSnapshotEnabled bool
}

// Reconcile implements the reconcile loop.
func (r *SwiftImageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var img imagev1alpha1.SwiftImage
	if err := r.Get(ctx, req.NamespacedName, &img); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion path: must run before the Ready early-return so that an
	// already-Ready SwiftImage still has its clone-seed finalizer
	// processed when the operator deletes it.
	if !img.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&img, CloneSeedFinalizer) {
			// No finalizer (legacy copy strategy or never had snapshot)
			// - nothing to do; let GC reap.
			return ctrl.Result{}, nil
		}
		canRemove, blocking, err := r.HandleCloneSeedDeletion(ctx, &img)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !canRemove {
			logger.Info("SwiftImage deletion blocked by dependent SwiftGuests",
				"image", img.Name, "namespace", img.Namespace, "guests", blocking)
			// Reconcile is also triggered by SwiftGuest deletions via
			// swiftguest controller's owns/watch chain; requeue defensively
			// in case something else holds it.
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		controllerutil.RemoveFinalizer(&img, CloneSeedFinalizer)
		if updateErr := r.Update(ctx, &img); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
	}

	// Add clone-seed finalizers when cloneStrategy=snapshot. Idempotent.
	if err := r.EnsureCloneSeedFinalizers(ctx, &img); err != nil {
		return ctrl.Result{}, err
	}

	// Immutability: reject spec update when Ready
	if img.Status.Phase == imagev1alpha1.SwiftImagePhaseReady {
		// Status-only updates allowed; spec changes rejected by controller
		return ctrl.Result{}, nil
	}

	phase := img.Status.Phase
	if phase == "" {
		phase = imagev1alpha1.SwiftImagePhasePending
	}

	status := img.Status.DeepCopy()

	switch phase {
	case imagev1alpha1.SwiftImagePhasePending:
		result, err := r.StartImport(ctx, &img)
		if err != nil {
			logger.Error(err, "import start failed")
			return ctrl.Result{}, err
		}
		if result.Error != "" {
			if result.Phase == imagev1alpha1.SwiftImagePhaseFailed {
				SetPhase(status, imagev1alpha1.SwiftImagePhaseFailed)
				SetFailedCondition(status, result.Error, result.Error)
			} else if result.Error == ReasonUploadNotImpl {
				SetFailedCondition(status, ReasonUploadNotImpl, "upload source not yet implemented")
				// Remain in Pending
			} else {
				SetFailedCondition(status, result.Error, result.Error)
			}
		} else {
			SetPhase(status, result.Phase)
		}

	case imagev1alpha1.SwiftImagePhaseImporting:
		nextPhase, pvcRef, errMsg, err := r.CheckImportStatus(ctx, &img)
		if err != nil {
			return ctrl.Result{}, err
		}
		if errMsg != "" {
			SetPhase(status, imagev1alpha1.SwiftImagePhaseFailed)
			SetFailedCondition(status, ReasonImportFailed, errMsg)
		} else if nextPhase == imagev1alpha1.SwiftImagePhaseValidating && pvcRef != nil {
			SetPhase(status, imagev1alpha1.SwiftImagePhaseValidating)
			_ = pvcRef // pvcRef is importPVCNamePrefix+img.Name, derived in Validate/Prepare
		}

	case imagev1alpha1.SwiftImagePhaseValidating:
		validateRes, err := r.Validate(ctx, &img, "")
		if err != nil {
			return ctrl.Result{}, err
		}
		if validateRes.Error == "measuring" {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		if !validateRes.OK {
			SetPhase(status, imagev1alpha1.SwiftImagePhaseFailed)
			SetFailedCondition(status, ReasonValidateFailed, validateRes.Error)
		} else {
			status.SizeHint = validateRes.Size
			SetPhase(status, imagev1alpha1.SwiftImagePhasePreparing)
		}

	case imagev1alpha1.SwiftImagePhasePreparing:
		prepareRes, err := r.Prepare(ctx, &img, "", nil, status.SizeHint)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !prepareRes.Success {
			SetPhase(status, imagev1alpha1.SwiftImagePhaseFailed)
			SetFailedCondition(status, ReasonPrepareFailed, prepareRes.Error)
			break
		}
		SetPreparedArtifact(status, prepareRes.PVCRef, prepareRes.Format, prepareRes.Size)
		status.SourceFormat = img.Spec.Format
		status.PreparedFormat = imagev1alpha1.DiskFormatRaw
		status.SizeHint = 0
		elapsed := time.Since(img.CreationTimestamp.Time).Seconds()
		metrics.ImageImportSeconds.WithLabelValues(img.Namespace).Observe(elapsed)
		// Branch: snapshot strategy needs a clone-seed VolumeSnapshot before
		// reaching Ready. Copy strategy goes Ready immediately.
		if img.Spec.CloneStrategy == imagev1alpha1.CloneStrategySnapshot {
			SetPhase(status, imagev1alpha1.SwiftImagePhaseSnapshotting)
		} else {
			SetPhase(status, imagev1alpha1.SwiftImagePhaseReady)
			SetReadyCondition(status)
		}

	case imagev1alpha1.SwiftImagePhaseSnapshotting:
		ready, sourceSizeBytes, err := r.EnsureCloneSeed(ctx, &img)
		if err != nil {
			SetPhase(status, imagev1alpha1.SwiftImagePhaseFailed)
			SetFailedCondition(status, ReasonSnapshotFailed, err.Error())
			break
		}
		if !ready {
			// Persist any progress (no-op currently) and requeue to re-poll.
			img.Status = *status
			if updateErr := r.Status().Update(ctx, &img); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		// Snapshot is readyToUse. Populate cloneSeed + Ready.
		status.CloneSeed = &imagev1alpha1.CloneSeed{
			Kind:            imagev1alpha1.CloneSeedKindVolumeSnapshot,
			Name:            CloneSeedSnapshotName(img.Name),
			Namespace:       img.Namespace,
			SourceSizeBytes: sourceSizeBytes,
		}
		SetPhase(status, imagev1alpha1.SwiftImagePhaseReady)
		SetReadyCondition(status)

	case imagev1alpha1.SwiftImagePhaseReady, imagev1alpha1.SwiftImagePhaseFailed:
		return ctrl.Result{}, nil
	}

	img.Status = *status
	if err := r.Status().Update(ctx, &img); err != nil {
		return ctrl.Result{}, err
	}

	// ImageImportTotal (observability O3): count the import's terminal
	// outcome. The Ready/Failed starting-phase cases return early above, so
	// reaching here with a terminal status.Phase is always a fresh
	// non-terminal -> terminal transition — no separate guard needed.
	switch status.Phase {
	case imagev1alpha1.SwiftImagePhaseReady:
		metrics.ImageImportTotal.WithLabelValues(img.Namespace, "ready").Inc()
	case imagev1alpha1.SwiftImagePhaseFailed:
		metrics.ImageImportTotal.WithLabelValues(img.Namespace, "failed").Inc()
	}

	return ctrl.Result{}, nil
}

// swiftGuestToSwiftImage enqueues the SwiftImage referenced by a
// SwiftGuest. Used so SwiftImage deletion blocked on dependent guests
// is unblocked immediately when those guests are deleted, rather than
// waiting for the 30s defensive requeue.
func (r *SwiftImageReconciler) swiftGuestToSwiftImage(_ context.Context, obj client.Object) []reconcile.Request {
	g, ok := obj.(*swiftv1alpha1.SwiftGuest)
	if !ok || g.Spec.ImageRef == nil || g.Spec.ImageRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: g.Spec.ImageRef.Name, Namespace: g.Namespace}}}
}

// SetupWithManager registers the reconciler with the manager.
func (r *SwiftImageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&imagev1alpha1.SwiftImage{}).
		Owns(&batchv1.Job{})
	if r.VolumeSnapshotEnabled {
		b = b.Owns(&volumesnapshotv1.VolumeSnapshot{})
	}
	return b.
		Watches(&swiftv1alpha1.SwiftGuest{}, handler.EnqueueRequestsFromMapFunc(r.swiftGuestToSwiftImage)).
		Complete(r)
}
