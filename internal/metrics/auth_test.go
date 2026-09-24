package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fakeAPIServer authenticates "good" and "unauthorized" tokens and allows
// only the "prometheus" user to GET /metrics.
func fakeAPIServer() *fake.Clientset {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		tr := a.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview)
		switch tr.Spec.Token {
		case "good":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: "prometheus"}}
		case "unauthorized":
			tr.Status = authnv1.TokenReviewStatus{Authenticated: true, User: authnv1.UserInfo{Username: "tenant"}}
		}
		return true, tr, nil
	})
	cs.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sar := a.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
		na := sar.Spec.NonResourceAttributes
		sar.Status.Allowed = sar.Spec.User == "prometheus" && na != nil && na.Path == "/metrics" && na.Verb == "get"
		return true, sar, nil
	})
	return cs
}

func TestAuthFilter(t *testing.T) {
	h, err := authFilter(fakeAPIServer())(logr.Discard(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("kubeswift_guests 3"))
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"no credentials", "", http.StatusUnauthorized},
		{"not a bearer token", "Basic Zm9vOmJhcg==", http.StatusUnauthorized},
		{"token the apiserver rejects", "Bearer forged", http.StatusUnauthorized},
		{"authenticated but not allowed", "Bearer unauthorized", http.StatusForbidden},
		{"allowed scraper", "Bearer good", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}
