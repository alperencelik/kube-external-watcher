package watcher

import (
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultPollInterval is the fallback poll interval when neither
	// the resource config nor the global option specifies one.
	DefaultPollInterval = 30 * time.Second

	// DefaultEventChannelBufferSize is the buffer size for the
	// GenericEvent channel used with source.Channel.
	DefaultEventChannelBufferSize = 1024
)

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

// WithEventChannelBufferSize sets the buffer size for the event channel.
func WithEventChannelBufferSize(size int) Option {
	return func(w *ExternalWatcher) {
		w.eventChannelBufferSize = size
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
// returns false, the resource is skipped (or unregistered if it was
// previously registered). If ready, the extractor function produces a
// ResourceConfig and the resource is registered. When the CR is deleted,
// the resource is unregistered.
//
// The cache is typically obtained via mgr.GetCache(). The obj parameter
// is a prototype of the CR type to watch (e.g. &myv1.Database{}).
func WithAutoRegister(c cache.Cache, obj client.Object, fn ConfigExtractorFn) Option {
	return func(w *ExternalWatcher) {
		w.autoRegister = &autoRegisterConfig{
			cache:     c,
			obj:       obj,
			extractor: fn,
		}
	}
}
