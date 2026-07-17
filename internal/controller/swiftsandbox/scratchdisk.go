package swiftsandbox

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

// scratchDiskName is the fixed disk name; it drives the launcher-side device
// path /dev/kubeswift-data-scratch (shared with the SwiftGuest data-disk path).
const scratchDiskName = "scratch"

func scratchDiskPVCName(sb *sandboxv1alpha1.SwiftSandbox) string {
	return sb.Name + "-scratch"
}

// scratchDiskDevicePath is the host device path the launcher exposes the Block
// PVC at; inside the guest it is a raw virtio-blk device (typically /dev/vdc).
func scratchDiskDevicePath() string {
	return runtimeintent.DataDiskDevicePath(scratchDiskName)
}

// reconcileScratchDisk provisions (blank) or resolves (pvcRef) the sandbox's
// scratch-disk PVC and gates pod creation on it being Bound, then stamps
// status.scratchDisk. ready=false means "requeued, not yet bound". No-op
// (ready=true) when spec.scratchDisk is unset.
func (r *SwiftSandboxReconciler) reconcileScratchDisk(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ready bool, res ctrl.Result, err error) {
	sd := sb.Spec.ScratchDisk
	if sd == nil {
		return true, ctrl.Result{}, nil
	}

	var pvcName string
	if sd.Blank != nil {
		pvcName = scratchDiskPVCName(sb)
		if err := r.ensureBlankScratchPVC(ctx, sb, pvcName); err != nil {
			return false, ctrl.Result{}, err
		}
	} else {
		pvcName = sd.PVCRef.Name
	}

	var pvc corev1.PersistentVolumeClaim
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: sb.Namespace, Name: pvcName}, &pvc); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return false, ctrl.Result{RequeueAfter: 3 * time.Second}, r.setScratchCondition(ctx, sb, metav1.ConditionFalse,
				"PVCNotFound", fmt.Sprintf("scratch PVC %q not found", pvcName))
		}
		return false, ctrl.Result{}, getErr
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		return false, ctrl.Result{RequeueAfter: 3 * time.Second}, r.setScratchCondition(ctx, sb, metav1.ConditionFalse,
			"Binding", fmt.Sprintf("scratch PVC %q not Bound (phase=%s)", pvcName, pvc.Status.Phase))
	}

	sb.Status.ScratchDisk = &sandboxv1alpha1.SandboxScratchDiskStatus{
		PVCName: pvcName, DevicePath: scratchDiskDevicePath(), Bound: true,
	}
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionScratchDiskReady, Status: metav1.ConditionTrue,
		Reason: "Bound", Message: "scratch PVC " + pvcName + " bound", ObservedGeneration: sb.Generation,
	})
	if err := r.Status().Update(ctx, sb); err != nil {
		return false, ctrl.Result{}, err
	}
	return true, ctrl.Result{}, nil
}

// ensureBlankScratchPVC creates the sandbox-owned blank Block PVC if missing
// (ownerRef → GC'd with the sandbox). Returns nil once it exists (the caller's
// Bound check drives the wait); errors only on a real create failure or an
// ownership conflict (a same-named PVC the sandbox doesn't own may hold
// unrelated data — never touch it).
func (r *SwiftSandboxReconciler) ensureBlankScratchPVC(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, pvcName string) error {
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Namespace: sb.Namespace, Name: pvcName}, &pvc)
	switch {
	case err == nil:
		if !metav1.IsControlledBy(&pvc, sb) {
			return fmt.Errorf("scratch PVC %q exists but is not owned by sandbox %q", pvcName, sb.Name)
		}
		return nil
	case !apierrors.IsNotFound(err):
		return err
	}

	block := corev1.PersistentVolumeBlock
	newPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: sb.Namespace,
			Labels:    map[string]string{SandboxLabelKey: sb.Name, "sandbox.kubeswift.io/role": "scratch"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:       &block,
			StorageClassName: sb.Spec.ScratchDisk.Blank.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: sb.Spec.ScratchDisk.Blank.Size},
			},
		},
	}
	if err := controllerutil.SetControllerReference(sb, newPVC, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, newPVC); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create scratch PVC %q: %w", pvcName, err)
	}
	return nil
}

func (r *SwiftSandboxReconciler) setScratchCondition(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, status metav1.ConditionStatus, reason, msg string) error {
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionScratchDiskReady, Status: status,
		Reason: reason, Message: msg, ObservedGeneration: sb.Generation,
	})
	return r.Status().Update(ctx, sb)
}
