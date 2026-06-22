package gateway

import (
	"context"
	"sync"

	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"

	fleetv1alpha1 "github.com/projectbeskar/kubeswift/api/fleet/v1alpha1"
	kubeswiftv1 "github.com/projectbeskar/kubeswift/gen/kubeswift/v1"
)

// ClusterWatcher fans the hub's fleet.Cluster informer out to any number of
// active ClusterService.Watch streams. It is a manager.Runnable: on Start it
// registers ONE shared informer event handler and broadcasts each change to
// subscribers. A slow subscriber drops events (non-blocking send) rather than
// stalling the informer; the UI re-lists on reconnect.
type ClusterWatcher struct {
	cache ctrlcache.Cache

	mu   sync.Mutex
	subs map[chan *kubeswiftv1.ClusterEvent]struct{}
}

// NewClusterWatcher builds a watcher over the given hub cache.
func NewClusterWatcher(c ctrlcache.Cache) *ClusterWatcher {
	return &ClusterWatcher{cache: c, subs: map[chan *kubeswiftv1.ClusterEvent]struct{}{}}
}

// NeedLeaderElection keeps the watcher running on every replica (the gateway
// does not use leader election).
func (w *ClusterWatcher) NeedLeaderElection() bool { return false }

// Start registers the informer event handler and blocks until ctx is done.
// It runs as a manager Runnable, so the hub cache is already starting when
// Start is invoked; GetInformer waits for the informer to be ready.
func (w *ClusterWatcher) Start(ctx context.Context) error {
	inf, err := w.cache.GetInformer(ctx, &fleetv1alpha1.Cluster{})
	if err != nil {
		return err
	}
	reg, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { w.emit(kubeswiftv1.EventType_EVENT_TYPE_ADDED, obj) },
		UpdateFunc: func(_, obj any) { w.emit(kubeswiftv1.EventType_EVENT_TYPE_MODIFIED, obj) },
		DeleteFunc: func(obj any) { w.emit(kubeswiftv1.EventType_EVENT_TYPE_DELETED, obj) },
	})
	if err != nil {
		return err
	}
	defer func() { _ = inf.RemoveEventHandler(reg) }()
	<-ctx.Done()
	return nil
}

func (w *ClusterWatcher) emit(t kubeswiftv1.EventType, obj any) {
	c := extractCluster(obj)
	if c == nil {
		return
	}
	ev := &kubeswiftv1.ClusterEvent{Type: t, Cluster: clusterToProto(c)}
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop; the UI re-lists on reconnect
		}
	}
}

func extractCluster(obj any) *fleetv1alpha1.Cluster {
	switch o := obj.(type) {
	case *fleetv1alpha1.Cluster:
		return o
	case toolscache.DeletedFinalStateUnknown:
		if c, ok := o.Obj.(*fleetv1alpha1.Cluster); ok {
			return c
		}
	}
	return nil
}

// subscribe registers a buffered channel for a Watch stream.
func (w *ClusterWatcher) subscribe() chan *kubeswiftv1.ClusterEvent {
	ch := make(chan *kubeswiftv1.ClusterEvent, 64)
	w.mu.Lock()
	w.subs[ch] = struct{}{}
	w.mu.Unlock()
	return ch
}

// unsubscribe removes and closes a Watch stream's channel. emit holds the same
// mutex while iterating, so it never sends on a closed channel.
func (w *ClusterWatcher) unsubscribe(ch chan *kubeswiftv1.ClusterEvent) {
	w.mu.Lock()
	delete(w.subs, ch)
	w.mu.Unlock()
	close(ch)
}
