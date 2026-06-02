package watcher

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultPollInterval is the fallback poll interval when neither
// the resource config nor the global option specifies one.
const DefaultPollInterval = 30 * time.Second

// Option is a functional option for configuring an ExternalWatcher.
type Option func(*ExternalWatcher)

// WithDefaultPollInterval sets the global default poll interval.
// Per-resource intervals in ResourceConfig take precedence.
func WithDefaultPollInterval(d time.Duration) Option {
	return func(w *ExternalWatcher) {
		w.defaultPollInterval = d
	}
}

// WithComparator sets a custom StateComparator. If not set,
// DeepEqualComparator is used.
func WithComparator(c StateComparator) Option {
	return func(w *ExternalWatcher) {
		w.comparator = c
	}
}

// WithLogger sets a structured logger. If not set, a no-op logger is used.
func WithLogger(l logr.Logger) Option {
	return func(w *ExternalWatcher) {
		w.logger = l
	}
}

// WithMetrics enables Prometheus metrics, registered against
// controller-runtime's default metrics registry. The controller name
// is used as a label to distinguish metrics from different controllers
// when multiple ExternalWatcher instances share the same process.
// When not set, all metric operations are no-ops with zero overhead.
func WithMetrics(controllerName string) Option {
	return func(w *ExternalWatcher) {
		w.metrics = newMetricsCollector(controllerName)
	}
}

// WithAutoRegister enables automatic resource registration via cache
// informer events. When a CR of the given type is created or updated,
// the fetcher's IsResourceReadyToWatch method is called first — if it
// returns false, a per-resource retry goroutine with exponential backoff
// re-checks readiness until the resource is ready, deleted, or the
// watcher shuts down. If ready, the extractor function produces a
// ResourceConfig and the resource is registered. When the CR is deleted,
// the resource is unregistered.
//
// The cache is typically obtained via mgr.GetCache(). The obj parameter
// is a prototype of the CR type to watch (e.g. &myv1.Database{}). Pass
// AutoRegisterOption values (AutoRegisterWithFilter,
// AutoRegisterWithReadinessRetry) to tune behavior.
func WithAutoRegister(c cache.Cache, obj client.Object, fn ConfigExtractorFn, opts ...AutoRegisterOption) Option {
	return func(w *ExternalWatcher) {
		cfg := &autoRegisterConfig{
			cache:     c,
			obj:       obj,
			extractor: fn,
			retries:   make(map[types.NamespacedName]context.CancelFunc),
		}
		for _, opt := range opts {
			opt(cfg)
		}
		cfg.retryConfig = cfg.retryConfig.withDefaults()
		w.autoRegister = cfg
	}
}

// AutoRegisterOption configures auto-register behavior. Pass these to
// WithAutoRegister to tune filtering and readiness retries.
type AutoRegisterOption func(*autoRegisterConfig)

// AutoRegisterWithFilter sets an EventFilter to control which informer
// events are processed by auto-register. Events rejected by the filter
// are silently skipped — the handler logic does not run for them.
func AutoRegisterWithFilter(f EventFilter) AutoRegisterOption {
	return func(cfg *autoRegisterConfig) {
		cfg.filter = &f
	}
}

// AutoRegisterWithReadinessRetry overrides the default readiness retry
// configuration used when IsResourceReadyToWatch returns false
func AutoRegisterWithReadinessRetry(rc ReadinessRetryConfig) AutoRegisterOption {
	return func(cfg *autoRegisterConfig) {
		cfg.retryConfig = rc
	}
}
