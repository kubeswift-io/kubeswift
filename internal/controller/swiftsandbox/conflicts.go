package swiftsandbox

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// requeueOnConflict turns a write conflict (the object changed since it was
// read) into a rate-limited requeue logged at debug level: the next pass
// reads the newer object and converges, exactly as it would after the error,
// but without a "Reconciler error" line for something nothing needs to act
// on (#758). Every other error is left as it is. Use as
//
//	defer requeueOnConflict(ctx, &res, &err)
//
// with named results.
func requeueOnConflict(ctx context.Context, res *ctrl.Result, err *error) {
	if *err == nil || !apierrors.IsConflict(*err) {
		return
	}
	ctrllog.FromContext(ctx).V(1).Info("write conflict; reconciling again", "error", (*err).Error())
	*res, *err = ctrl.Result{Requeue: true}, nil
}
