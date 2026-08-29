package watcher

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ConfigExtractorFn extracts a ResourceConfig from a Kubernetes object.
// Used with WithAutoRegister to automatically register resources when
// they are created or updated, and unregister them when deleted.
//
// Readiness checking is handled separately by the fetcher's
// IsResourceReadyToWatch method — see ResourceStateFetcher.
type ConfigExtractorFn func(obj client.Object) ResourceConfig

// EventFilter provides optional predicates to filter informer events
// before auto-register processes them. If a predicate returns false,
// the event is skipped entirely (the handler logic does not run).
// Nil functions are treated as "allow all" — only set the ones you
// need to filter.
type EventFilter struct {
	// Add is called on informer Add events. Return false to skip.
	Add func(obj client.Object) bool

	// Update is called on informer Update events. Return false to skip.
	// Both the old and new object are provided for comparison
	// (e.g. generation change, annotation diff).
	Update func(oldObj, newObj client.Object) bool

	// Delete is called on informer Delete events. Return false to skip.
	Delete func(obj client.Object) bool
}

// ReadinessRetryConfig holds parameters for the readiness retry loop
// that re-checks IsResourceReadyToWatch with exponential backoff when
// a resource is not ready during auto-registration. Zero values use
// defaults (InitialInterval: 10s, MaxInterval: 10m, MaxRetries: 0/unlimited,
// Jitter: 0.1).
type ReadinessRetryConfig struct {
	// InitialInterval is the delay before the first retry. Default: 10s.
	InitialInterval time.Duration

	// MaxInterval is the backoff cap. Default: 10m.
	MaxInterval time.Duration

	// MaxRetries is the maximum number of retry attempts. 0 = unlimited
	// (stopped by context cancellation or resource deletion). Default: 0.
	MaxRetries int

	// Jitter is the jitter factor applied to each retry sleep: the
	// effective delay is a random duration in
	// [interval, interval+Jitter*interval).
	Jitter *float64
}

const DefaultReadinessRetryJitter = 0.1

func (c ReadinessRetryConfig) withDefaults() ReadinessRetryConfig {
	if c.InitialInterval <= 0 {
		c.InitialInterval = 10 * time.Second
	}
	if c.MaxInterval <= 0 {
		c.MaxInterval = 10 * time.Minute
	}
	if c.Jitter == nil || *c.Jitter < 0 {
		jitter := DefaultReadinessRetryJitter
		c.Jitter = &jitter
	}
	return c
}

// readinessRetry tracks a single pending readiness-retry goroutine for a
// resource that was not ready when an informer event arrived.
type readinessRetry struct {
	// cancel stops the retry goroutine.
	cancel context.CancelFunc

	// obj is the latest object snapshot seen for this key. It is refreshed
	// when a newer informer event arrives while the retry is still pending,
	// so the eventual registration reflects the current spec (e.g. an
	// updated ResourceKey) rather than the object from the first event.
	// Guarded by autoRegisterConfig.mu.
	obj client.Object
}

// autoRegisterConfig holds the configuration for automatic resource
// registration via cache informer events.
type autoRegisterConfig struct {
	cache     cache.Cache
	obj       client.Object
	extractor ConfigExtractorFn
	filter    *EventFilter

	retryConfig ReadinessRetryConfig

	// mu guards the retries map and the fields of each readinessRetry.
	mu      sync.Mutex
	retries map[types.NamespacedName]*readinessRetry
}

// setupAutoRegister hooks into the cache informer for the configured
// object type and registers/unregisters resources automatically.
func setupAutoRegister(ctx context.Context, w *ExternalWatcher) error {
	informer, err := w.autoRegister.cache.GetInformer(ctx, w.autoRegister.obj)
	if err != nil {
		return fmt.Errorf("auto-register: failed to get informer: %w", err)
	}

	filter := w.autoRegister.filter

	_, err = informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cObj, ok := obj.(client.Object)
			if !ok {
				return
			}
			if filter != nil && filter.Add != nil && !filter.Add(cObj) {
				return
			}
			key := types.NamespacedName{Name: cObj.GetName(), Namespace: cObj.GetNamespace()}
			if !w.fetcher.IsResourceReadyToWatch(ctx, key) {
				w.logger.V(2).Info("auto-register: resource not ready, starting retry", "resource", key.String())
				w.startReadinessRetry(ctx, key, cObj)
				return
			}
			w.cancelReadinessRetry(key)
			config := w.autoRegister.extractor(cObj)
			w.doRegister(key, config)
			w.logger.V(1).Info("auto-registered resource", "resource", key.String())
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			cOld, okOld := oldObj.(client.Object)
			cNew, okNew := newObj.(client.Object)
			if !okOld || !okNew {
				return
			}
			if filter != nil && filter.Update != nil && !filter.Update(cOld, cNew) {
				return
			}
			key := types.NamespacedName{Name: cNew.GetName(), Namespace: cNew.GetNamespace()}
			if !w.fetcher.IsResourceReadyToWatch(ctx, key) {
				if w.IsRegistered(key) {
					w.doUnregister(key)
					w.logger.V(1).Info("auto-unregistered resource (no longer ready), scheduling readiness retry", "resource", key.String())
				} else {
					w.logger.V(2).Info("auto-register: resource not ready on update, starting retry", "resource", key.String())
				}
				// Schedule a readiness retry in both cases. Readiness is
				// external state that can recover without any Kubernetes
				// object change, so without a retry a resource unregistered
				// here might never be re-registered (the next informer event
				// could be hours away, or never come).
				w.startReadinessRetry(ctx, key, cNew)
				return
			}
			w.cancelReadinessRetry(key)
			config := w.autoRegister.extractor(cNew)
			w.doRegister(key, config)
			w.logger.V(2).Info("auto-register updated resource config", "resource", key.String())
		},
		DeleteFunc: func(obj interface{}) {
			var (
				key     types.NamespacedName
				cObj    client.Object
				haveObj bool
			)
			if d, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				if o, ok := d.Obj.(client.Object); ok {
					cObj, haveObj = o, true
				} else {
					// The tombstone carries no usable object (e.g. it holds
					// only a cache key). Fall back to the key string so the
					// resource is still unregistered instead of leaking a
					// watcher that polls a deleted resource forever. The
					// Delete filter can't run without the object; unregistering
					// is the safe default for cleanup.
					ns, name, err := toolscache.SplitMetaNamespaceKey(d.Key)
					if err != nil {
						w.logger.Error(err, "auto-register: cannot parse tombstone key, skipping delete", "key", d.Key)
						return
					}
					key = types.NamespacedName{Namespace: ns, Name: name}
				}
			} else if o, ok := obj.(client.Object); ok {
				cObj, haveObj = o, true
			} else {
				w.logger.V(2).Info("auto-register: delete event object is not a client.Object, skipping")
				return
			}

			if haveObj {
				if filter != nil && filter.Delete != nil && !filter.Delete(cObj) {
					return
				}
				key = types.NamespacedName{Name: cObj.GetName(), Namespace: cObj.GetNamespace()}
			}

			w.cancelReadinessRetry(key)
			w.doUnregister(key)
			w.logger.V(1).Info("auto-unregistered resource", "resource", key.String())
		},
	})
	if err != nil {
		return fmt.Errorf("auto-register: failed to add event handler: %w", err)
	}

	w.logger.Info("auto-register enabled for informer")
	return nil
}

// startReadinessRetry starts a per-resource goroutine that periodically
// re-checks IsResourceReadyToWatch with exponential backoff. Once ready,
// it extracts config and registers the resource. If a retry is already
// pending for the key, its object snapshot is refreshed instead of starting
// a second goroutine.
func (w *ExternalWatcher) startReadinessRetry(ctx context.Context, key types.NamespacedName, obj client.Object) {
	ar := w.autoRegister

	// Deep-copy the object so the goroutine doesn't read a stale or
	// mutated informer-managed pointer when it eventually succeeds.
	objCopy := obj.DeepCopyObject().(client.Object)

	ar.mu.Lock()
	if existing, exists := ar.retries[key]; exists {
		// A retry is already running; refresh its object so it registers
		// with the latest spec once ready, then leave it running.
		existing.obj = objCopy
		ar.mu.Unlock()
		return
	}
	retryCtx, cancel := context.WithCancel(ctx)
	entry := &readinessRetry{cancel: cancel, obj: objCopy}
	ar.retries[key] = entry
	ar.mu.Unlock()

	cfg := ar.retryConfig
	w.logger.V(1).Info("auto-register: starting readiness retry", "resource", key.String())

	go func() {
		defer func() {
			ar.mu.Lock()
			// Only remove our own entry — a Delete-then-Add sequence may have
			// cancelled this retry and installed a newer one under the same
			// key, which must remain tracked and cancelable.
			if ar.retries[key] == entry {
				delete(ar.retries, key)
			}
			ar.mu.Unlock()
		}()

		interval := cfg.InitialInterval
		attempts := 0
		for {
			delay := interval
			jitter := DefaultReadinessRetryJitter
			if cfg.Jitter != nil {
				jitter = *cfg.Jitter
			}
			if jitter > 0 {
				// wait.Jitter treats a non-positive factor as 1.0, so only
				// call it when jitter is actually enabled.
				delay = wait.Jitter(interval, jitter)
			}
			select {
			case <-retryCtx.Done():
				w.logger.V(2).Info("auto-register: readiness retry cancelled", "resource", key.String())
				return
			case <-time.After(delay):
			}

			attempts++
			if w.fetcher.IsResourceReadyToWatch(retryCtx, key) {
				// Register atomically with respect to cancellation. A Delete
				// event cancels + removes this entry under ar.mu before
				// unregistering; by holding ar.mu across the ownership check
				// and doRegister, a delete either wins (our entry is gone, so
				// we skip) or is forced to wait and then unregister what we
				// registered. Either way we never leave a watcher running for
				// a deleted resource.
				ar.mu.Lock()
				if ar.retries[key] != entry {
					ar.mu.Unlock()
					w.logger.V(2).Info("auto-register: readiness retry superseded before register",
						"resource", key.String())
					return
				}
				w.doRegister(key, ar.extractor(entry.obj))
				ar.mu.Unlock()
				w.logger.V(1).Info("auto-register: resource became ready after retry",
					"resource", key.String(), "attempts", attempts)
				return
			}

			w.logger.V(2).Info("auto-register: resource still not ready",
				"resource", key.String(), "attempt", attempts, "nextInterval", interval)

			if cfg.MaxRetries > 0 && attempts >= cfg.MaxRetries {
				w.logger.V(1).Info("auto-register: max retries reached, giving up",
					"resource", key.String(), "maxRetries", cfg.MaxRetries)
				return
			}

			// Exponential backoff for retry
			interval *= 2
			if interval > cfg.MaxInterval {
				interval = cfg.MaxInterval
			}
		}
	}()
}

// cancelReadinessRetry cancels the retry goroutine for the given key
func (w *ExternalWatcher) cancelReadinessRetry(key types.NamespacedName) {
	ar := w.autoRegister
	ar.mu.Lock()
	defer ar.mu.Unlock()
	if entry, ok := ar.retries[key]; ok {
		entry.cancel()
		delete(ar.retries, key)
	}
}

// cancelAllReadinessRetries cancels all pending retry goroutines
func (w *ExternalWatcher) cancelAllReadinessRetries() {
	ar := w.autoRegister
	ar.mu.Lock()
	defer ar.mu.Unlock()
	for _, entry := range ar.retries {
		entry.cancel()
	}
	ar.retries = make(map[types.NamespacedName]*readinessRetry)
}
