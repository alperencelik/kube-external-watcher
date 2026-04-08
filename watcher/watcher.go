package watcher

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// ExternalWatcher manages per-resource watcher goroutines and integrates
// with controller-runtime as a manager.Runnable.
//
// It implements:
//   - manager.Runnable (Start)
//   - manager.LeaderElectionRunnable (NeedLeaderElection)
//   - WatcherManager (Register, Unregister, IsRegistered)
type ExternalWatcher struct {
	fetcher    ResourceStateFetcher
	comparator StateComparator

	// eventCh is the channel that source.Channel reads from.
	eventCh chan event.GenericEvent

	defaultPollInterval    time.Duration
	eventChannelBufferSize int
	logger                 logr.Logger
	metrics                *metricsCollector

	mu       sync.RWMutex
	watchers map[types.NamespacedName]*resourceWatcher

	// autoRegister holds optional auto-registration config. When set,
	// Start() hooks into the cache informer to auto-register/unregister.
	autoRegister *autoRegisterConfig

	// started indicates whether Start() has been called.
	started bool
	ctx     context.Context
}

// Compile-time interface checks.
var _ WatcherManager = (*ExternalWatcher)(nil)

// NewExternalWatcher creates a new ExternalWatcher with the given fetcher
// and options. The returned watcher must be:
//  1. Added to the manager via mgr.Add(watcher).
//  2. Wired into the controller via source.Channel(watcher.EventChannel(), ...).
func NewExternalWatcher(fetcher ResourceStateFetcher, opts ...Option) *ExternalWatcher {
	w := &ExternalWatcher{
		fetcher:                fetcher,
		comparator:             NewDeepEqualComparator(),
		defaultPollInterval:    DefaultPollInterval,
		eventChannelBufferSize: DefaultEventChannelBufferSize,
		logger:                 logr.Discard(),
		watchers:               make(map[types.NamespacedName]*resourceWatcher),
	}
	for _, opt := range opts {
		opt(w)
	}
	w.eventCh = make(chan event.GenericEvent, w.eventChannelBufferSize)
	return w
}

// EventChannel returns the read-only channel that should be passed to
// source.Channel when setting up the controller. Must be called before
// the manager starts.
func (w *ExternalWatcher) EventChannel() <-chan event.GenericEvent {
	return w.eventCh
}

// Start implements manager.Runnable. Called by controller-runtime's manager.
// It starts all pre-registered resource watchers and blocks until the
// context is cancelled. If WithAutoRegister was configured, it sets up
// the cache informer event handler before starting watchers.
func (w *ExternalWatcher) Start(ctx context.Context) error {
	if w.autoRegister != nil {
		if err := setupAutoRegister(ctx, w); err != nil {
			return fmt.Errorf("external watcher start: %w", err)
		}
	}

	w.mu.Lock()
	w.ctx = ctx
	w.started = true
	for key, rw := range w.watchers {
		if !rw.running {
			rw.start(ctx)
			w.logger.V(1).Info("started pre-registered resource watcher",
				"resource", key.String())
		}
	}
	w.mu.Unlock()

	// Block until context is cancelled (manager shutdown).
	<-ctx.Done()

	// Cancel pending readiness retries.
	if w.autoRegister != nil {
		w.cancelAllReadinessRetries()
	}

	// Graceful shutdown: stop all resource watchers.
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, rw := range w.watchers {
		rw.stop()
		w.logger.V(1).Info("stopped resource watcher", "resource", key.String())
	}
	w.watchers = make(map[types.NamespacedName]*resourceWatcher)
	w.metrics.resetRegisteredResources()

	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
// Make sure only one replica runs the external watcher.
// TODO: Consider distributing the watcher across replicas in the future, but for now let's stick to leader election for simplicity and to avoid duplicate events.
func (w *ExternalWatcher) NeedLeaderElection() bool {
	return true
}

// Register starts watching the external state for the given resource.
// If the resource is already registered, its configuration is updated
// without restarting the watcher goroutine. Goroutine-safe.
func (w *ExternalWatcher) Register(key types.NamespacedName, config ResourceConfig) {
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
		w.comparator, w.eventCh, w.logger.WithValues("resource", key.String()),
		w.metrics)

	w.watchers[key] = rw
	w.metrics.incRegisteredResources()

	if w.started {
		rw.start(w.ctx)
		w.logger.V(1).Info("started resource watcher",
			"resource", key.String(), "pollInterval", pollInterval)
	}
}

// Unregister stops watching the external state for the given resource.
// No-op if the key is not registered. Goroutine-safe.
func (w *ExternalWatcher) Unregister(key types.NamespacedName) {
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
