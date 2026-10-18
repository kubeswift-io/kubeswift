package swiftsandbox

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
)

// A write conflict is a requeue, not an error; anything else stays an error.
func TestRequeueOnConflict(t *testing.T) {
	conflict := apierrors.NewConflict(schema.GroupResource{Group: "sandbox.kubeswift.io", Resource: "swiftsandboxes"}, "sb", errors.New("modified"))
	res, err := ctrl.Result{}, error(conflict)
	requeueOnConflict(context.Background(), &res, &err)
	if err != nil || !res.Requeue {
		t.Errorf("conflict: %+v, %v", res, err)
	}
	wrapped := error(errors.Join(errors.New("status"), conflict))
	res = ctrl.Result{}
	requeueOnConflict(context.Background(), &res, &wrapped)
	if wrapped != nil || !res.Requeue {
		t.Errorf("wrapped conflict: %+v, %v", res, wrapped)
	}
	other, res := error(errors.New("boom")), ctrl.Result{RequeueAfter: 5}
	requeueOnConflict(context.Background(), &res, &other)
	if other == nil || res.RequeueAfter != 5 {
		t.Errorf("other error changed: %+v, %v", res, other)
	}
	var none error
	res = ctrl.Result{RequeueAfter: 7}
	requeueOnConflict(context.Background(), &res, &none)
	if none != nil || res.RequeueAfter != 7 || res.Requeue {
		t.Errorf("no error changed: %+v", res)
	}
}
