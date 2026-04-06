package watcher

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// metricsVecs holds the shared Prometheus vector collectors. Registered
// once against the metrics registry; individual metricsCollector
// instances curry the "controller" label for their watcher.
type metricsVecs struct {
	pollTotal             *prometheus.CounterVec
	fetchExternalDuration *prometheus.HistogramVec
	fetchExternalErrors   *prometheus.CounterVec
	driftDetectedTotal    *prometheus.CounterVec
	registeredResources   *prometheus.GaugeVec
}

var (
	globalVecs     *metricsVecs
	globalVecsOnce sync.Once
)

func initGlobalVecs() *metricsVecs {
	globalVecsOnce.Do(func() {
		globalVecs = newMetricsVecs(crmetrics.Registry)
	})
	return globalVecs
}

func newMetricsVecs(reg prometheus.Registerer) *metricsVecs {
	v := &metricsVecs{
		pollTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_external_watcher_poll_total",
			Help: "Total number of poll cycles executed by the external watcher.",
		}, []string{"controller", "namespace", "name", "result"}),

		fetchExternalDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kube_external_watcher_fetch_external_duration_seconds",
			Help:    "Duration of external resource fetch calls in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"controller", "namespace", "name"}),

		fetchExternalErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_external_watcher_fetch_external_errors_total",
			Help: "Total number of errors when fetching external resource state.",
		}, []string{"controller", "namespace", "name"}),

		driftDetectedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_external_watcher_drift_detected_total",
			Help: "Total number of drift detections that triggered reconciliation.",
		}, []string{"controller", "namespace", "name"}),

		registeredResources: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kube_external_watcher_registered_resources",
			Help: "Current number of resources registered with the external watcher.",
		}, []string{"controller"}),
	}

	reg.MustRegister(
		v.pollTotal,
		v.fetchExternalDuration,
		v.fetchExternalErrors,
		v.driftDetectedTotal,
		v.registeredResources,
	)

	return v
}

// metricsCollector is a per-watcher handle that binds a controller name
// to the shared metric vectors.
type metricsCollector struct {
	controller string
	vecs       *metricsVecs
}

func newMetricsCollector(controller string) *metricsCollector {
	return &metricsCollector{
		controller: controller,
		vecs:       initGlobalVecs(),
	}
}

func newMetricsCollectorWithVecs(controller string, vecs *metricsVecs) *metricsCollector {
	return &metricsCollector{
		controller: controller,
		vecs:       vecs,
	}
}

func (m *metricsCollector) incPollTotal(namespace, name, result string) {
	if m == nil {
		return
	}
	m.vecs.pollTotal.WithLabelValues(m.controller, namespace, name, result).Inc()
}

func (m *metricsCollector) observeFetchDuration(namespace, name string, duration time.Duration) {
	if m == nil {
		return
	}
	m.vecs.fetchExternalDuration.WithLabelValues(m.controller, namespace, name).Observe(duration.Seconds())
}

func (m *metricsCollector) incFetchExternalErrors(namespace, name string) {
	if m == nil {
		return
	}
	m.vecs.fetchExternalErrors.WithLabelValues(m.controller, namespace, name).Inc()
}

func (m *metricsCollector) incDriftDetected(namespace, name string) {
	if m == nil {
		return
	}
	m.vecs.driftDetectedTotal.WithLabelValues(m.controller, namespace, name).Inc()
}

func (m *metricsCollector) incRegisteredResources() {
	if m == nil {
		return
	}
	m.vecs.registeredResources.WithLabelValues(m.controller).Inc()
}

func (m *metricsCollector) decRegisteredResources() {
	if m == nil {
		return
	}
	m.vecs.registeredResources.WithLabelValues(m.controller).Dec()
}

func (m *metricsCollector) resetRegisteredResources() {
	if m == nil {
		return
	}
	m.vecs.registeredResources.WithLabelValues(m.controller).Set(0)
}
