package swiftimage

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/metrics"
)

// SwiftImageReconciler reconciles SwiftImage resources.
type SwiftImageReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Converter ImageConverter
	Clientset kubernetes.Interface
}

// Reconcile implements the reconcile loop.
func (r *SwiftImageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var img imagev1alpha1.SwiftImage
	if err := r.Get(ctx, req.NamespacedName, &img); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
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
		} else {
			SetPhase(status, imagev1alpha1.SwiftImagePhaseReady)
			SetPreparedArtifact(status, prepareRes.PVCRef, prepareRes.Format, prepareRes.Size)
			status.SourceFormat = img.Spec.Format
			status.PreparedFormat = imagev1alpha1.DiskFormatRaw
			SetReadyCondition(status)
			status.SizeHint = 0
			elapsed := time.Since(img.CreationTimestamp.Time).Seconds()
			metrics.ImageImportSeconds.WithLabelValues(img.Namespace).Observe(elapsed)
		}

	case imagev1alpha1.SwiftImagePhaseReady, imagev1alpha1.SwiftImagePhaseFailed:
		return ctrl.Result{}, nil
	}

	img.Status = *status
	if err := r.Status().Update(ctx, &img); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *SwiftImageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&imagev1alpha1.SwiftImage{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
