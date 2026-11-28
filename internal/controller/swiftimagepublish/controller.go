// Package swiftimagepublish reconciles SwiftImagePublish: it publishes a
// disk (a CSI snapshot's root disk, for now) to an OCI registry in the
// golden-image format, signed when the destination has a key, and can
// create a SwiftImage pinned to the pushed digest.
package swiftimagepublish

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
)

// DefaultMaxConcurrent is the cluster-wide cap on publish Jobs running at
// once when MaxConcurrent is 0.
const DefaultMaxConcurrent = 2

// ConditionReady is the request's one condition.
const ConditionReady = "Ready"

// Reasons on the Ready condition besides the source and storage location
// ones.
const (
	ReasonQueued          = "Queued"
	ReasonPublishing      = "Publishing"
	ReasonPublished       = "Published"
	ReasonTagInUse        = "TagInUse"
	ReasonPublishFailed   = "PublishFailed"
	ReasonNotConfigured   = "NotConfigured"
	ReasonImageCreateFail = "ImageCreateFailed"
)

const waitRequeue = 15 * time.Second

// Reconciler reconciles SwiftImagePublish objects.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// SnapshotORASImage runs the publish Job (snapshot-oras).
	SnapshotORASImage string
	// MaxConcurrent caps publish Jobs running at once, cluster-wide;
	// DefaultMaxConcurrent when 0.
	MaxConcurrent int
}

func (r *Reconciler) maxConcurrent() int {
	if r.MaxConcurrent > 0 {
		return r.MaxConcurrent
	}
	return DefaultMaxConcurrent
}

// Reconcile drives a request Pending -> Publishing -> Ready | Failed.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pub imagev1alpha1.SwiftImagePublish
	if err := r.Get(ctx, req.NamespacedName, &pub); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	switch pub.Status.Phase {
	case imagev1alpha1.SwiftImagePublishReady, imagev1alpha1.SwiftImagePublishFailed:
		return ctrl.Result{}, nil // terminal
	}
	orig := pub.DeepCopy()
	res, err := r.reconcile(ctx, &pub)
	if !equality.Semantic.DeepEqual(orig.Status, pub.Status) {
		if perr := r.Status().Patch(ctx, &pub, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); perr != nil {
			if apierrors.IsConflict(perr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, perr
		}
	}
	return res, err
}

func (r *Reconciler) reconcile(ctx context.Context, pub *imagev1alpha1.SwiftImagePublish) (ctrl.Result, error) {
	if pub.Status.Phase == "" {
		pub.Status.Phase = imagev1alpha1.SwiftImagePublishPending
	}
	if r.SnapshotORASImage == "" {
		r.fail(pub, ReasonNotConfigured, "snapshot-oras image not configured (set KUBESWIFT_SNAPSHOT_ORAS_IMAGE)")
		return ctrl.Result{}, nil
	}

	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: pub.Namespace, Name: jobName(pub)}, &job)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err == nil {
		return r.watchJob(ctx, pub, &job)
	}
	if pub.Status.Phase == imagev1alpha1.SwiftImagePublishPublishing {
		// The Job is gone while publishing (deleted by hand): say so rather
		// than pushing a second time.
		r.fail(pub, ReasonPublishFailed, "publish Job "+jobName(pub)+" disappeared before it finished")
		return ctrl.Result{}, r.cleanup(ctx, pub)
	}

	// Destination: resolved once and kept, so a changed default moves no
	// request that already started.
	if pub.Status.Destination == nil {
		dst, wait, err := resolveDestination(ctx, r.Client, pub)
		if err != nil {
			return ctrl.Result{}, err
		}
		if wait != nil {
			r.wait(pub, wait.Reason, wait.Message)
			return ctrl.Result{RequeueAfter: waitRequeue}, nil
		}
		pub.Status.Destination = dst
		pub.Status.Reference = dst.Repository + ":" + pub.Spec.Destination.Tag
	}
	if wait, err := checkSecrets(ctx, r.Client, pub.Namespace, pub.Status.Destination); err != nil {
		return ctrl.Result{}, err
	} else if wait != nil {
		r.wait(pub, wait.Reason, wait.Message)
		return ctrl.Result{RequeueAfter: waitRequeue}, nil
	}

	// Source: looked at on every pass until the Job starts.
	src, err := resolveSnapshotSource(ctx, r.Client, pub)
	if err != nil {
		return ctrl.Result{}, err
	}
	if src.fail != nil {
		r.fail(pub, src.fail.reason, src.fail.message)
		return ctrl.Result{}, r.cleanup(ctx, pub)
	}
	if src.wait != nil {
		r.wait(pub, src.wait.reason, src.wait.message)
		return ctrl.Result{RequeueAfter: waitRequeue}, nil
	}
	pub.Status.Source = src.status

	// A free publish slot, cluster-wide.
	if full, err := r.slotsFull(ctx, pub); err != nil {
		return ctrl.Result{}, err
	} else if full {
		r.wait(pub, ReasonQueued, fmt.Sprintf("waiting for one of %d publish slots", r.maxConcurrent()))
		return ctrl.Result{RequeueAfter: waitRequeue}, nil
	}

	pvc := diskPVC(pub, src.status, src.storageClass)
	if err := ctrl.SetControllerReference(pub, pvc, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("create disk PVC: %w", err)
	}
	j := buildJob(pub, r.SnapshotORASImage)
	if err := ctrl.SetControllerReference(pub, j, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, j); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("create publish Job: %w", err)
	}
	now := metav1.Now()
	pub.Status.StartTime = &now
	pub.Status.Phase = imagev1alpha1.SwiftImagePublishPublishing
	r.setReady(pub, metav1.ConditionFalse, ReasonPublishing, "pushing "+pub.Status.Reference)
	return ctrl.Result{}, nil
}

// watchJob follows the publish Job to its end.
func (r *Reconciler) watchJob(ctx context.Context, pub *imagev1alpha1.SwiftImagePublish, job *batchv1.Job) (ctrl.Result, error) {
	if pub.Status.Phase != imagev1alpha1.SwiftImagePublishPublishing {
		pub.Status.Phase = imagev1alpha1.SwiftImagePublishPublishing
		r.setReady(pub, metav1.ConditionFalse, ReasonPublishing, "pushing "+pub.Status.Reference)
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return r.complete(ctx, pub)
		case batchv1.JobFailed:
			msg := clonecommon.JobFailureMessage(ctx, r.Client, job, c.Message)
			reason := ReasonPublishFailed
			if strings.Contains(msg, "tag in use") {
				reason = ReasonTagInUse
				msg += " (set destination.overwriteTag to move the tag, or publish under another tag)"
			}
			r.fail(pub, reason, "publish Job failed: "+msg)
			return ctrl.Result{}, r.cleanup(ctx, pub)
		}
	}
	return ctrl.Result{}, nil
}

// complete records what the Job pushed, creates the SwiftImage if asked,
// and removes the temporary disk.
func (r *Reconciler) complete(ctx context.Context, pub *imagev1alpha1.SwiftImagePublish) (ctrl.Result, error) {
	if pub.Status.Digest == "" {
		rep, ok, err := clonecommon.JobTransferReport(ctx, r.Client, pub.Namespace, jobName(pub))
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ok || rep.ManifestDigest == "" {
			r.fail(pub, ReasonPublishFailed, "publish Job completed without reporting a digest")
			return ctrl.Result{}, r.cleanup(ctx, pub)
		}
		pub.Status.Digest = rep.ManifestDigest
		pub.Status.SizeBytes = rep.TotalBytes
		pub.Status.TransferredBytes = rep.TransferredBytes
		pub.Status.Signed = rep.Signed
		if pub.Status.Destination.SigningKeySecretName != "" && !rep.Signed {
			r.fail(pub, ReasonPublishFailed, "the destination has a signing key but the image was not signed")
			return ctrl.Result{}, r.cleanup(ctx, pub)
		}
	}
	if pub.Spec.CreateImage != nil {
		cond, err := ensureImage(ctx, r.Client, pub)
		if err != nil {
			return ctrl.Result{}, err
		}
		if cond != nil {
			r.fail(pub, cond.reason, cond.message)
			return ctrl.Result{}, r.cleanup(ctx, pub)
		}
		pub.Status.Image = pub.Spec.CreateImage.Name
	}
	now := metav1.Now()
	pub.Status.CompletionTime = &now
	pub.Status.Phase = imagev1alpha1.SwiftImagePublishReady
	r.setReady(pub, metav1.ConditionTrue, ReasonPublished, "published "+pub.Status.Reference+"@"+pub.Status.Digest)
	return ctrl.Result{}, r.cleanup(ctx, pub)
}

// slotsFull reports whether MaxConcurrent other requests are publishing.
func (r *Reconciler) slotsFull(ctx context.Context, pub *imagev1alpha1.SwiftImagePublish) (bool, error) {
	var list imagev1alpha1.SwiftImagePublishList
	if err := r.List(ctx, &list); err != nil {
		return false, err
	}
	n := 0
	for i := range list.Items {
		p := &list.Items[i]
		if p.UID != pub.UID && p.Status.Phase == imagev1alpha1.SwiftImagePublishPublishing {
			n++
		}
	}
	return n >= r.maxConcurrent(), nil
}

// cleanup deletes the temporary disk PVC, if any. The Job is left for its
// logs; its TTL removes it.
func (r *Reconciler) cleanup(ctx context.Context, pub *imagev1alpha1.SwiftImagePublish) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: pub.Namespace, Name: diskPVCName(pub)}}
	if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *Reconciler) wait(pub *imagev1alpha1.SwiftImagePublish, reason, msg string) {
	pub.Status.Phase = imagev1alpha1.SwiftImagePublishPending
	r.setReady(pub, metav1.ConditionFalse, reason, msg)
}

func (r *Reconciler) fail(pub *imagev1alpha1.SwiftImagePublish, reason, msg string) {
	now := metav1.Now()
	pub.Status.Phase = imagev1alpha1.SwiftImagePublishFailed
	pub.Status.CompletionTime = &now
	r.setReady(pub, metav1.ConditionFalse, reason, msg)
	logf.Log.WithName("swiftimagepublish").Info("publish failed", "namespace", pub.Namespace, "name", pub.Name, "reason", reason, "message", msg)
}

func (r *Reconciler) setReady(pub *imagev1alpha1.SwiftImagePublish, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&pub.Status.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: pub.Generation,
	})
}

// SetupWithManager registers the controller; it watches the Jobs and PVCs
// it owns.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("swiftimagepublish").
		For(&imagev1alpha1.SwiftImagePublish{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
