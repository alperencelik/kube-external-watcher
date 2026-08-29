package watcher

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// resourceWatcher is a per-resource polling goroutine that gets the
// desired state from Kubernetes, the actual state from an external API,
// detects drift, and enqueues a reconcile.Request when drift is found.
type resourceWatcher struct {
	key types.NamespacedName

	// resourceKey is the external resource ID. It can change while the
	// watcher runs, so mu guards it.
	resourceKey any

	fetcher    ResourceStateFetcher
	comparator StateComparator
	logger     logr.Logger

	// queue is the controller's workqueue, set when start is called.
	queue workqueue.TypedRateLimitingInterface[reconcile.Request]

	// cancel stops this resource watcher's goroutine.
	cancel context.CancelFunc

	// running indicates whether the goroutine is active.
	running bool

	metrics *metricsCollector

	// mu protects resourceKey and pollInterval for dynamic updates.
	mu           sync.Mutex
	pollInterval time.Duration

	// jitter is the poll jitter factor. Each sleep is stretched by a
	// random amount in [0, jitter*interval).
	jitter float64

	// driftMu protects lastDrift. It is separate from mu so that callers
	// of LastDrift do not contend with poll-interval updates.
	driftMu   sync.RWMutex
	lastDrift *DriftInfo
}

func newResourceWatcher(
	key types.NamespacedName,
	resourceKey any,
	pollInterval time.Duration,
	jitter float64,
	fetcher ResourceStateFetcher,
	comparator StateComparator,
	logger logr.Logger,
	metrics *metricsCollector,
) *resourceWatcher {
	return &resourceWatcher{
		key:          key,
		resourceKey:  resourceKey,
		pollInterval: pollInterval,
		jitter:       jitter,
		fetcher:      fetcher,
		comparator:   comparator,
		logger:       logger,
		metrics:      metrics,
	}
}

func (rw *resourceWatcher) start(parentCtx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	ctx, cancel := context.WithCancel(parentCtx)
	rw.cancel = cancel
	rw.queue = queue
	rw.running = true
	go rw.run(ctx)
}

func (rw *resourceWatcher) stop() {
	if rw.cancel != nil {
		rw.cancel()
	}
	rw.running = false
}

// updateConfig re-configures a running watcher. The next poll picks up
// the new values.
func (rw *resourceWatcher) updateConfig(resourceKey any, pollInterval time.Duration) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.resourceKey = resourceKey
	rw.pollInterval = pollInterval
}

func (rw *resourceWatcher) currentResourceKey() any {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.resourceKey
}

func (rw *resourceWatcher) currentPollInterval() time.Duration {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.pollInterval
}

func (rw *resourceWatcher) getLastDrift() (DriftInfo, bool) {
	rw.driftMu.RLock()
	defer rw.driftMu.RUnlock()
	if rw.lastDrift == nil {
		return DriftInfo{}, false
	}
	return *rw.lastDrift, true
}

func (rw *resourceWatcher) setLastDrift(info DriftInfo) {
	rw.driftMu.Lock()
	defer rw.driftMu.Unlock()
	rw.lastDrift = &info
}

func (rw *resourceWatcher) clearLastDrift() {
	rw.driftMu.Lock()
	defer rw.driftMu.Unlock()
	rw.lastDrift = nil
}

func (rw *resourceWatcher) run(ctx context.Context) {
	// Perform an initial fetch immediately on start.
	rw.poll(ctx)

	for {
		interval := rw.currentPollInterval()
		if rw.jitter > 0 {
			// wait.Jitter treats a non-positive factor as 1.0, so only
			// call it when jitter is actually enabled.
			interval = wait.Jitter(interval, rw.jitter)
		}
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

	// Contain panics from user code (fetcher, comparator) to this cycle
	// instead of letting them kill the process.
	defer recoverPanic(rw.logger, "recovered from panic during poll cycle", func() {
		rw.metrics.incPollTotal(ns, name, "panic")
	})

	desired, err := rw.fetcher.GetDesiredState(ctx, rw.key)
	if err != nil {
		rw.logger.Error(err, "failed to fetch desired state")
		rw.metrics.incPollTotal(ns, name, "error")
		return
	}

	fetchStart := time.Now()
	rawExternal, err := rw.fetcher.FetchExternalResource(ctx, rw.currentResourceKey())
	rw.metrics.observeFetchDuration(ns, name, time.Since(fetchStart))
	if err != nil {
		rw.logger.Error(err, "failed to fetch external resource state")
		rw.metrics.incFetchExternalErrors(ns, name)
		rw.metrics.incPollTotal(ns, name, "error")
		return
	}

	if updater, ok := rw.fetcher.(ResourceStatusUpdater); ok {
		if err := updater.UpdateResourceStatus(ctx, rw.key, rawExternal); err != nil {
			rw.logger.Error(err, "failed to update resource status")
		}
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

		rw.setLastDrift(DriftInfo{
			Key:        rw.key,
			DetectedAt: time.Now(),
			Diff:       rw.comparator.Diff(desired, actual),
		})

		if rw.queue != nil {
			rw.queue.Add(reconcile.Request{NamespacedName: rw.key})
			rw.logger.V(2).Info("reconcile request enqueued")
		}
	} else {
		rw.clearLastDrift()
	}

	rw.metrics.incPollTotal(ns, name, "success")
}
