// Package swiftstoragelocation reconciles storage.kubeswift.io locations:
// it reports whether each is well formed (Valid), usable (Ready: valid and
// not one of several defaults at its level) and, for a cluster location,
// whether its registry answers (Reachable). Nothing here resolves a location
// for a consumer; that is the snapshot controller's, and it reads Ready.
package swiftstoragelocation

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/storagelocation"
)

// Condition reasons.
const (
	ReasonValid            = "Valid"
	ReasonInvalid          = "Invalid"
	ReasonReady            = "Ready"
	ReasonAmbiguousDefault = "AmbiguousDefault"
	ReasonReachable        = "Reachable"
	ReasonUnreachable      = "Unreachable"
)

// reachabilityRefresh is how often a cluster location's registry is probed
// again while nothing about it changes.
const reachabilityRefresh = 10 * time.Minute

// Prober reports whether the registry of an OCI location answers, with a
// message saying how. Replaced in tests.
type Prober func(ctx context.Context, o *storagev1alpha1.OCILocation) (bool, string)

// ClusterReconciler reconciles SwiftClusterStorageLocations.
type ClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Probe checks a registry; nil means ProbeRegistry.
	Probe Prober
}

// NamespaceReconciler reconciles SwiftStorageLocations. It never probes: the
// controller does not make requests to hosts a namespace chose.
type NamespaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var loc storagev1alpha1.SwiftClusterStorageLocation
	if err := r.Get(ctx, req.NamespacedName, &loc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if loc.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	defaults, err := storagelocation.ClusterDefaults(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	orig := loc.DeepCopy()
	valid := setConditions(&loc.Status, loc.Generation, &loc.Spec, loc.Name, defaults, "SwiftClusterStorageLocation")
	var requeue time.Duration
	if loc.Spec.OCI != nil && valid {
		probe := r.Probe
		if probe == nil {
			probe = ProbeRegistry
		}
		ok, msg := probe(ctx, loc.Spec.OCI)
		reason, status := ReasonReachable, metav1.ConditionTrue
		if !ok {
			reason, status = ReasonUnreachable, metav1.ConditionFalse
		}
		meta.SetStatusCondition(&loc.Status.Conditions, metav1.Condition{
			Type: storagev1alpha1.ConditionReachable, Status: status, Reason: reason,
			Message: msg, ObservedGeneration: loc.Generation,
		})
		requeue = reachabilityRefresh
	} else {
		meta.RemoveStatusCondition(&loc.Status.Conditions, storagev1alpha1.ConditionReachable)
	}
	if !equality.Semantic.DeepEqual(orig.Status, loc.Status) {
		if err := r.Status().Patch(ctx, &loc, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var loc storagev1alpha1.SwiftStorageLocation
	if err := r.Get(ctx, req.NamespacedName, &loc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if loc.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	defaults, err := storagelocation.NamespaceDefaults(ctx, r.Client, loc.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	orig := loc.DeepCopy()
	setConditions(&loc.Status, loc.Generation, &loc.Spec, loc.Name, defaults, "SwiftStorageLocation")
	if !equality.Semantic.DeepEqual(orig.Status, loc.Status) {
		if err := r.Status().Patch(ctx, &loc, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{}, nil
}

// setConditions sets Valid and Ready, and reports whether the spec is valid.
// defaults are the names of every default at this location's level.
func setConditions(st *storagev1alpha1.StorageLocationStatus, gen int64, spec *storagev1alpha1.StorageLocationSpec, name string, defaults []string, kind string) bool {
	st.ObservedGeneration = gen
	if err := storagelocation.Validate(spec); err != nil {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: storagev1alpha1.ConditionValid, Status: metav1.ConditionFalse, Reason: ReasonInvalid, Message: err.Error(), ObservedGeneration: gen,
		})
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: storagev1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonInvalid, Message: err.Error(), ObservedGeneration: gen,
		})
		return false
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: storagev1alpha1.ConditionValid, Status: metav1.ConditionTrue, Reason: ReasonValid, Message: "the spec is well formed", ObservedGeneration: gen,
	})
	if others := storagelocation.Others(defaults, name); spec.Default && len(others) > 0 {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: storagev1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonAmbiguousDefault, ObservedGeneration: gen,
			Message: fmt.Sprintf("%s %s is also the default; objects that need the default wait until only one is left",
				kind, strings.Join(others, ", ")),
		})
		return true
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: storagev1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonReady, Message: "usable", ObservedGeneration: gen,
	})
	return true
}

// ProbeRegistry asks the registry's API root, GET /v2/, as the OCI
// distribution spec has every registry answer. 2xx, or 401 (it wants
// credentials), means it answered. It runs from the controller's pod, whose
// network path to the registry may differ from a node's.
func ProbeRegistry(ctx context.Context, o *storagev1alpha1.OCILocation) (bool, string) {
	host, err := storagelocation.Registry(o.Repository)
	if err != nil {
		return false, err.Error()
	}
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	scheme := "https"
	if o.Insecure {
		scheme = "http"
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CABundle != "" {
		pool, err := storagelocation.CAPool(o.CABundle)
		if err != nil {
			return false, err.Error()
		}
		tlsCfg.RootCAs = pool
	}
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+host+"/v2/", nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false, err.Error()
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusUnauthorized {
		return true, fmt.Sprintf("GET %s://%s/v2/ answered %d", scheme, host, resp.StatusCode)
	}
	return false, fmt.Sprintf("GET %s://%s/v2/ answered %d, not an OCI registry API response", scheme, host, resp.StatusCode)
}

// SetupWithManager registers the cluster reconciler. A change to any cluster
// location re-evaluates them all, so a second default marks both.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("swiftclusterstoragelocation").
		For(&storagev1alpha1.SwiftClusterStorageLocation{}).
		Watches(&storagev1alpha1.SwiftClusterStorageLocation{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
				var list storagev1alpha1.SwiftClusterStorageLocationList
				if err := mgr.GetClient().List(ctx, &list); err != nil {
					return nil
				}
				reqs := make([]reconcile.Request, 0, len(list.Items))
				for i := range list.Items {
					reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name}})
				}
				return reqs
			}),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// SetupWithManager registers the namespace reconciler. A change to a location
// re-evaluates every location in its namespace.
func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("swiftstoragelocation").
		For(&storagev1alpha1.SwiftStorageLocation{}).
		Watches(&storagev1alpha1.SwiftStorageLocation{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				var list storagev1alpha1.SwiftStorageLocationList
				if err := mgr.GetClient().List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
					return nil
				}
				reqs := make([]reconcile.Request, 0, len(list.Items))
				for i := range list.Items {
					reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: list.Items[i].Name}})
				}
				return reqs
			}),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
