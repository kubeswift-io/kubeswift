package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"

	connect "connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1alpha1 "github.com/kubeswift-io/kubeswift/api/fleet/v1alpha1"
	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func TestClusterService_ListClusters_NamespaceScoped(t *testing.T) {
	hub := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		&fleetv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "boba", Namespace: "kubeswift-system"}},
		&fleetv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "miles", Namespace: "kubeswift-system"}},
		&fleetv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "other"}},
	).Build()

	// ListClusters uses only the hub cache, but it DOES authenticate — passing a
	// nil Authenticator here is what let the unauthenticated-fleet-enumeration
	// bug ship (a nil auth would panic, not pass).
	svc := NewClusterService(hub, "kubeswift-system", nil, nil, NewInsecureAuthenticator())
	resp, err := svc.ListClusters(context.Background(), connect.NewRequest(&kubeswiftv1.ListClustersRequest{}))
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(resp.Msg.Clusters) != 2 {
		t.Fatalf("got %d clusters, want 2 (the gateway namespace only)", len(resp.Msg.Clusters))
	}
	names := map[string]bool{}
	for _, c := range resp.Msg.Clusters {
		names[c.Name] = true
	}
	if !names["boba"] || !names["miles"] || names["elsewhere"] {
		t.Errorf("unexpected cluster set: %v", names)
	}
}

func TestClusterWatcher_EmitSubscribeUnsubscribe(t *testing.T) {
	w := NewClusterWatcher(nil) // emit/subscribe don't touch the cache
	sub := w.subscribe()

	w.emit(kubeswiftv1.EventType_EVENT_TYPE_ADDED,
		&fleetv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "boba"}})

	select {
	case ev := <-sub:
		if ev.Type != kubeswiftv1.EventType_EVENT_TYPE_ADDED || ev.Cluster.GetName() != "boba" {
			t.Errorf("unexpected event: %+v", ev)
		}
	default:
		t.Fatal("expected a broadcast event")
	}

	w.unsubscribe(sub)
	if _, ok := <-sub; ok {
		t.Error("channel should be closed after unsubscribe")
	}
}

// TestClusterWatcher_NonBlockingOnSlowSubscriber proves emit never blocks even
// when a subscriber's buffer is full — the slow stream drops events rather than
// stalling the shared informer.
func TestClusterWatcher_NonBlockingOnSlowSubscriber(t *testing.T) {
	w := NewClusterWatcher(nil)
	sub := w.subscribe()
	defer w.unsubscribe(sub)
	for i := 0; i < 200; i++ { // far past the 64-deep buffer
		w.emit(kubeswiftv1.EventType_EVENT_TYPE_MODIFIED,
			&fleetv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "x"}})
	}
	// Reaching here without deadlock is the assertion.
}

// rejectingAuthenticator stands in for a real authenticator refusing a caller
// with no/!valid credentials.
type rejectingAuthenticator struct{}

func (rejectingAuthenticator) Authenticate(context.Context, http.Header) (Identity, error) {
	return Identity{}, connect.NewError(connect.CodeUnauthenticated, errors.New("no credentials"))
}

// The fleet list carries every member's apiserver URL and version, so an
// unauthenticated caller must not be able to enumerate it. Both RPCs shipped
// without an Authenticate call; these pin them.
func TestClusterService_ListClusters_RequiresAuth(t *testing.T) {
	hub := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		&fleetv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "boba", Namespace: "kubeswift-system"}},
	).Build()

	svc := NewClusterService(hub, "kubeswift-system", nil, nil, rejectingAuthenticator{})
	resp, err := svc.ListClusters(context.Background(), connect.NewRequest(&kubeswiftv1.ListClustersRequest{}))
	if err == nil {
		t.Fatalf("ListClusters accepted an unauthenticated caller and returned %d clusters", len(resp.Msg.GetClusters()))
	}
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want %v", got, connect.CodeUnauthenticated)
	}
}

func TestClusterService_WatchClusters_RequiresAuth(t *testing.T) {
	hub := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		&fleetv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "boba", Namespace: "kubeswift-system"}},
	).Build()

	// nil watcher + nil stream on purpose: the auth check must reject BEFORE
	// subscribe() or the first Send, so neither is ever reached.
	svc := NewClusterService(hub, "kubeswift-system", nil, nil, rejectingAuthenticator{})
	err := svc.WatchClusters(context.Background(), connect.NewRequest(&kubeswiftv1.WatchClustersRequest{}), nil)
	if err == nil {
		t.Fatal("WatchClusters accepted an unauthenticated caller")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Errorf("code = %v, want %v", got, connect.CodeUnauthenticated)
	}
}
