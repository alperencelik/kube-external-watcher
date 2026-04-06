package watcher

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// resourceWatcher is a per-resource polling goroutine that gets
// the desired state from Kubernetes and actual state from an external API, detects drift, and emits GenericEvents.
type resourceWatcher struct {
	key         types.NamespacedName
	resourceKey any

	fetcher    ResourceStateFetcher
	comparator StateComparator
	eventCh    chan<- event.GenericEvent
	logger     logr.Logger

	// cancel stops this resource watcher's goroutine.
	cancel context.CancelFunc

	// running indicates whether the goroutine is active.
	running bool

	metrics *metricsCollector

	// mu protects pollInterval for dynamic updates.
	mu           sync.Mutex
	pollInterval time.Duration
}

func newResourceWatcher(
	key types.NamespacedName,
	resourceKey any,
	pollInterval time.Duration,
	fetcher ResourceStateFetcher,
	comparator StateComparator,
	eventCh chan<- event.GenericEvent,
	logger logr.Logger,
	metrics *metricsCollector,
) *resourceWatcher {
	return &resourceWatcher{
		key:          key,
		resourceKey:  resourceKey,
		pollInterval: pollInterval,
		fetcher:      fetcher,
		comparator:   comparator,
		eventCh:      eventCh,
		logger:       logger,
		metrics:      metrics,
	}
}

func (rw *resourceWatcher) start(parentCtx context.Context) {
	ctx, cancel := context.WithCancel(parentCtx)
	rw.cancel = cancel
	rw.running = true
	go rw.run(ctx)
}

func (rw *resourceWatcher) stop() {
	if rw.cancel != nil {
		rw.cancel()
	}
	rw.running = false
}

func (rw *resourceWatcher) updatePollInterval(d time.Duration) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.pollInterval = d
}

func (rw *resourceWatcher) currentPollInterval() time.Duration {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.pollInterval
}

func (rw *resourceWatcher) run(ctx context.Context) {
	// Perform an initial fetch immediately on start.
	rw.poll(ctx)

	for {
		interval := rw.currentPollInterval()
		select {
		case <-ctx.Done():
			return
		// time.After instead of ticker for dynamic intervals
		case <-time.After(interval):
			rw.poll(ctx)
		}
	}
}

func (rw *resourceWatcher) poll(ctx context.Context) {
	ns, name := rw.key.Namespace, rw.key.Name

	desired, err := rw.fetcher.GetDesiredState(ctx, rw.key)
	if err != nil {
		rw.logger.Error(err, "failed to fetch desired state")
		rw.metrics.incPollTotal(ns, name, "error")
		return
	}

	fetchStart := time.Now()
	rawExternal, err := rw.fetcher.FetchExternalResource(ctx, rw.resourceKey)
	rw.metrics.observeFetchDuration(ns, name, time.Since(fetchStart))
	if err != nil {
		rw.logger.Error(err, "failed to fetch external resource state")
		rw.metrics.incFetchExternalErrors(ns, name)
		rw.metrics.incPollTotal(ns, name, "error")
		return
	}

	actual, err := rw.fetcher.TransformExternalState(rawExternal)
	if err != nil {
		rw.logger.Error(err, "failed to transform external state")
		rw.metrics.incPollTotal(ns, name, "error")
		return
	}

	drifted, err := rw.comparator.HasDrifted(desired, actual)
	if err != nil {
		rw.logger.Error(err, "failed to compare states")
		rw.metrics.incPollTotal(ns, name, "error")
		return
	}

	if drifted {
		rw.logger.V(1).Info("drift detected, triggering reconciliation")
		rw.metrics.incDriftDetected(ns, name)

		obj := &objectReference{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rw.key.Name,
				Namespace: rw.key.Namespace,
			},
		}

		select {
		case rw.eventCh <- event.GenericEvent{Object: obj}:
			rw.logger.V(2).Info("reconcile event sent")
		case <-ctx.Done():
			return
		}
	}

	rw.metrics.incPollTotal(ns, name, "success")
}
