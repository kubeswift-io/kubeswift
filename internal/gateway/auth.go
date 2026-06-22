package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"

	connect "connectrpc.com/connect"
	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authnclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
)

// Identity is the end user the gateway impersonates against member clusters
// (decision D1). An empty Identity means "no impersonation" — member queries
// run as the member's own credential (the SA-trusted dev mode).
type Identity struct {
	User   string
	Groups []string
}

func (i Identity) empty() bool { return i.User == "" }

// Authenticator resolves the end-user identity from a request's headers so the
// gateway can impersonate it against member clusters.
type Authenticator interface {
	Authenticate(ctx context.Context, h http.Header) (Identity, error)
}

// insecureAuthenticator performs no authentication and returns an empty
// identity, so member queries run as the gateway's own credential. This is the
// SA-trusted dev stub the P0 cut allows; production uses TokenReview.
type insecureAuthenticator struct{}

// NewInsecureAuthenticator returns the no-impersonation dev authenticator.
func NewInsecureAuthenticator() Authenticator { return insecureAuthenticator{} }

func (insecureAuthenticator) Authenticate(context.Context, http.Header) (Identity, error) {
	return Identity{}, nil
}

// tokenReviewAuthenticator validates the request's bearer token via the hub's
// TokenReview API and impersonates the resulting user against members.
type tokenReviewAuthenticator struct {
	reviews authnclient.TokenReviewInterface
}

// NewTokenReviewAuthenticator builds a TokenReview-backed authenticator from a
// client to the hub's authentication API.
func NewTokenReviewAuthenticator(reviews authnclient.TokenReviewInterface) Authenticator {
	return &tokenReviewAuthenticator{reviews: reviews}
}

func (a *tokenReviewAuthenticator) Authenticate(ctx context.Context, h http.Header) (Identity, error) {
	tok := bearerToken(h)
	if tok == "" {
		return Identity{}, connect.NewError(connect.CodeUnauthenticated, errors.New("missing bearer token"))
	}
	res, err := a.reviews.Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: tok},
	}, metav1.CreateOptions{})
	if err != nil {
		return Identity{}, connect.NewError(connect.CodeInternal, err)
	}
	if !res.Status.Authenticated {
		return Identity{}, connect.NewError(connect.CodeUnauthenticated, errors.New("token not authenticated"))
	}
	return Identity{User: res.Status.User.Username, Groups: res.Status.User.Groups}, nil
}

func bearerToken(h http.Header) string {
	const prefix = "Bearer "
	if v := h.Get("Authorization"); strings.HasPrefix(v, prefix) {
		return strings.TrimPrefix(v, prefix)
	}
	return ""
}
