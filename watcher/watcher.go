package watcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// ExternalWatcher manages per-resource watcher goroutines and integrates
// with controller-runtime by implementing source.Source. It is wired into
// a controller via WatchesRawSource(ew), at which point the controller
// calls Start with its workqueue and the watcher enqueues
// reconcile.Requests directly when drift is detected.
//
// It implements:
//   - source.Source (Start)
//   - WatcherManager (Register, Unregister, IsRegistered)
type ExternalWatcher struct {
	fetcher    ResourceStateFetcher
	comparator StateComparator

	defaultPollInterval time.Duration
	logger              logr.Logger
	metrics             *metricsCollector

	mu       sync.RWMutex
	watchers map[types.NamespacedName]*resourceWatcher

	// autoRegister holds optional auto-registration config. When set,
	// Start hooks into the cache informer to auto-register/unregister.
	autoRegister *autoRegisterConfig

	// started indicates whether Start has been called.
	started bool
	ctx     context.Context
	queue   workqueue.TypedRateLimitingInterface[reconcile.Request]
}

// Compile-time interface checks.
var (
	_ WatcherManager = (*ExternalWatcher)(nil)
	_ source.Source  = (*ExternalWatcher)(nil)
)

// NewExternalWatcher creates a new ExternalWatcher with the given fetcher
// and options. The returned watcher should be wired into a controller via
// WatchesRawSource(watcher); controller-runtime will call Start when the
// controller starts.
func NewExternalWatcher(fetcher ResourceStateFetcher, opts ...Option) *ExternalWatcher {
	w := &ExternalWatcher{
		fetcher:             fetcher,
		comparator:          NewDeepEqualComparator(),
		defaultPollInterval: DefaultPollInterval,
		logger:              logr.Discard(),
		watchers:            make(map[types.NamespacedName]*resourceWatcher),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Start implements source.Source. Called by controller-runtime when the
// owning controller starts. Start is non-blocking: it sets up the auto-
// register informer (if configured), spawns pre-registered resource
// watchers, and returns. Drift events are enqueued onto queue as
// reconcile.Requests.
func (w *ExternalWatcher) Start(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	// Make sure users can call start only once
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return errors.New("external watcher: Start called more than once")
	}
	w.started = true
	w.ctx = ctx
	w.queue = queue
	w.mu.Unlock()

	if w.autoRegister != nil {
		if err := setupAutoRegister(ctx, w); err != nil {
			return fmt.Errorf("external watcher start: %w", err)
		}
	}

	w.mu.Lock()
	for key, rw := range w.watchers {
		if !rw.running {
			rw.start(ctx, queue)
			w.logger.V(1).Info("started pre-registered resource watcher",
				"resource", key.String())
		}
	}
	w.mu.Unlock()

	go w.shutdownOnContextDone(ctx)

	return nil
}

// shutdownOnContextDone waits for ctx cancellation and cleans up all
// running resource watchers and pending readiness retries.
func (w *ExternalWatcher) shutdownOnContextDone(ctx context.Context) {
	<-ctx.Done()

	if w.autoRegister != nil {
		w.cancelAllReadinessRetries()
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	for key, rw := range w.watchers {
		rw.stop()
		w.logger.V(1).Info("stopped resource watcher", "resource", key.String())
	}
	w.watchers = make(map[types.NamespacedName]*resourceWatcher)
	w.metrics.resetRegisteredResources()
}

// Register starts watching the external state for the given resource.
// If the resource is already registered, its configuration is updated
// without restarting the watcher goroutine. Goroutine-safe.
//
// When WithAutoRegister is enabled, this method is a no-op — auto-register
// manages the full lifecycle via informer events. Use one approach per
// watcher instance, not both.
func (w *ExternalWatcher) Register(key types.NamespacedName, config ResourceConfig) {
	if w.autoRegister != nil {
		w.logger.Info(
			"Register called while WithAutoRegister is enabled; "+
				"call ignored — use either auto-register or manual Register/Unregister, not both",
			"resource", key.String(),
		)
		return
	}
	w.doRegister(key, config)
}

func (w *ExternalWatcher) doRegister(key types.NamespacedName, config ResourceConfig) {
	w.mu.Lock()
	defer w.mu.Unlock()

	pollInterval := w.defaultPollInterval
	// Use config.PollInterval if set and valid. Otherwise, use the default.
	if config.PollInterval > 0 {
		pollInterval = config.PollInterval
	}

	if existing, ok := w.watchers[key]; ok {
		existing.updatePollInterval(pollInterval)
		w.logger.V(1).Info("updated resource watcher config",
			"resource", key.String(), "pollInterval", pollInterval)
		return
	}

	rw := newResourceWatcher(key, config.ResourceKey, pollInterval, w.fetcher,
		w.comparator, w.logger.WithValues("resource", key.String()),
		w.metrics)

	w.watchers[key] = rw
	w.metrics.incRegisteredResources()

	if w.started {
		rw.start(w.ctx, w.queue)
		w.logger.V(1).Info("started resource watcher",
			"resource", key.String(), "pollInterval", pollInterval)
	}
}

// Unregister stops watching the external state for the given resource.
// No-op if the key is not registered. Goroutine-safe.
//
// When WithAutoRegister is enabled, the resource is unregistered immediately
// but may be re-registered automatically on the next informer event if the
// resource still exists and is ready. This allows controllers to temporarily
// stop watching during operations like deletion or terminal errors.
func (w *ExternalWatcher) Unregister(key types.NamespacedName) {
	w.doUnregister(key)
}

func (w *ExternalWatcher) doUnregister(key types.NamespacedName) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if rw, ok := w.watchers[key]; ok {
		rw.stop()
		delete(w.watchers, key)
		w.metrics.decRegisteredResources()
		w.logger.V(1).Info("unregistered resource watcher", "resource", key.String())
	}
}

// IsRegistered returns whether a resource is currently being watched.
func (w *ExternalWatcher) IsRegistered(key types.NamespacedName) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	_, ok := w.watchers[key]
	return ok
}

// LastDrift returns the most recently observed drift for the given
// resource, if any. It is intended to be called from a reconciler that
// was triggered by this watcher — the returned DriftInfo carries the
// timestamp and (when the comparator implements StateDiffer) a
// human-readable diff. The bool return is false when the resource is
// not registered or its last poll observed no drift.
//
// The watcher auto-clears the entry when a subsequent poll finds the
// states matching again, so steady-state reconciles after a successful
// fix observe (DriftInfo{}, false). Goroutine-safe.
func (w *ExternalWatcher) LastDrift(key types.NamespacedName) (DriftInfo, bool) {
	w.mu.RLock()
	rw, ok := w.watchers[key]
	w.mu.RUnlock()
	if !ok {
		return DriftInfo{}, false
	}
	return rw.getLastDrift()
}
