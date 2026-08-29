package watcher

import (
	"context"
	"fmt"
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

	fetcher    ResourceStateFetcher
	comparator StateComparator
	logger     logr.Logger

	// queue is the controller's workqueue, set when start is called.
	queue workqueue.TypedRateLimitingInterface[reconcile.Request]

	metrics *metricsCollector

	// mu protects the dynamically-updated config (pollInterval,
	// resourceKey) and the lifecycle fields (running, cancel, done).
	mu           sync.Mutex
	pollInterval time.Duration
	resourceKey  any

	// running indicates whether the goroutine is active.
	running bool

	// cancel stops this resource watcher's goroutine.
	cancel context.CancelFunc

	// done is closed by run when the goroutine exits, letting stop block
	// until any in-flight poll has finished.
	done chan struct{}

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

// start launches the poll goroutine. It is idempotent: calling it on an
// already-running watcher is a no-op.
func (rw *resourceWatcher) start(parentCtx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	rw.mu.Lock()
	if rw.running {
		rw.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parentCtx)
	done := make(chan struct{})
	rw.cancel = cancel
	rw.done = done
	rw.queue = queue
	rw.running = true
	rw.mu.Unlock()

	go rw.run(ctx, done)
}

// stop cancels the poll goroutine and blocks until it has exited, so any
// in-flight poll completes before stop returns. It is idempotent and safe
// to call on a watcher that was never started. Callers must not hold the
// owning ExternalWatcher's lock, since stop can block for the duration of
// a poll.
func (rw *resourceWatcher) stop() {
	rw.mu.Lock()
	if !rw.running {
		rw.mu.Unlock()
		return
	}
	rw.running = false
	cancel := rw.cancel
	done := rw.done
	rw.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
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

func (rw *resourceWatcher) updateResourceKey(k any) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.resourceKey = k
}

func (rw *resourceWatcher) currentResourceKey() any {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.resourceKey
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

func (rw *resourceWatcher) run(ctx context.Context, done chan struct{}) {
	defer close(done)

	// When jitter is enabled, delay the initial poll by a random fraction
	// of the interval (up to jitter*interval). Watchers registered together
	// during startup cache sync then spread their first external API call
	// across the window instead of firing in a synchronized burst.
	if rw.jitter > 0 {
		interval := rw.currentPollInterval()
		if initialDelay := wait.Jitter(interval, rw.jitter) - interval; initialDelay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(initialDelay):
			}
		}
	}

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

	// Recover from panics anywhere in the poll cycle — in the comparator,
	// or in user-supplied fetcher code — so a single bad poll is logged and
	// counted as an error instead of taking down the whole controller
	// process. The loop continues with the next cycle.
	defer func() {
		if r := recover(); r != nil {
			rw.logger.Error(fmt.Errorf("%v", r), "recovered from panic during poll")
			rw.metrics.incPollTotal(ns, name, "error")
		}
	}()

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
