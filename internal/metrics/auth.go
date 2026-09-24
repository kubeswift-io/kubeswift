package metrics

import (
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// AuthFilterProvider is a metrics-server FilterProvider that serves /metrics
// only to callers the apiserver authenticates (TokenReview) and authorizes to
// GET that path (SubjectAccessReview on the non-resource URL). The metrics
// name guests, images and namespaces across every tenant, so an open endpoint
// hands that inventory to anyone who can reach the pod.
//
// controller-runtime's own filters package does the same, but pulls in
// k8s.io/apiserver; this uses only the client-go APIs already in the build.
// Grant scrapers the kubeswift-metrics-reader ClusterRole.
func AuthFilterProvider(cfg *rest.Config, _ *http.Client) (metricsserver.Filter, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return authFilter(cs), nil
}

func authFilter(cs kubernetes.Interface) metricsserver.Filter {
	return func(log logr.Logger, next http.Handler) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			tr, err := cs.AuthenticationV1().TokenReviews().Create(r.Context(),
				&authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token}}, metav1.CreateOptions{})
			if err != nil {
				log.Error(err, "metrics: token review failed")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			if !tr.Status.Authenticated {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			u := tr.Status.User
			extra := make(map[string]authzv1.ExtraValue, len(u.Extra))
			for k, v := range u.Extra {
				extra[k] = authzv1.ExtraValue(v)
			}
			sar, err := cs.AuthorizationV1().SubjectAccessReviews().Create(r.Context(), &authzv1.SubjectAccessReview{
				Spec: authzv1.SubjectAccessReviewSpec{
					User: u.Username, UID: u.UID, Groups: u.Groups, Extra: extra,
					NonResourceAttributes: &authzv1.NonResourceAttributes{Path: r.URL.Path, Verb: "get"},
				},
			}, metav1.CreateOptions{})
			if err != nil {
				log.Error(err, "metrics: access review failed")
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			if !sar.Status.Allowed {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		}), nil
	}
}
