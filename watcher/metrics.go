package watcher

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// metricsCollector holds Prometheus metrics for the external watcher.
type metricsCollector struct {
	pollTotal             *prometheus.CounterVec
	fetchExternalDuration *prometheus.HistogramVec
	fetchExternalErrors   *prometheus.CounterVec
	driftDetectedTotal    *prometheus.CounterVec
	registeredResources   prometheus.Gauge
}

func newMetricsCollector() *metricsCollector {
	return newMetricsCollectorWithRegisterer(crmetrics.Registry)
}

func newMetricsCollectorWithRegisterer(reg prometheus.Registerer) *metricsCollector {
	m := &metricsCollector{
		pollTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_external_watcher_poll_total",
			Help: "Total number of poll cycles executed by the external watcher.",
		}, []string{"namespace", "name", "result"}),

		fetchExternalDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kube_external_watcher_fetch_external_duration_seconds",
			Help:    "Duration of external resource fetch calls in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"namespace", "name"}),

		fetchExternalErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_external_watcher_fetch_external_errors_total",
			Help: "Total number of errors when fetching external resource state.",
		}, []string{"namespace", "name"}),

		driftDetectedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kube_external_watcher_drift_detected_total",
			Help: "Total number of drift detections that triggered reconciliation.",
		}, []string{"namespace", "name"}),

		registeredResources: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kube_external_watcher_registered_resources",
			Help: "Current number of resources registered with the external watcher.",
		}),
	}

	reg.MustRegister(
		m.pollTotal,
		m.fetchExternalDuration,
		m.fetchExternalErrors,
		m.driftDetectedTotal,
		m.registeredResources,
	)

	return m
}

func (m *metricsCollector) incPollTotal(namespace, name, result string) {
	if m == nil {
		return
	}
	m.pollTotal.WithLabelValues(namespace, name, result).Inc()
}

func (m *metricsCollector) observeFetchDuration(namespace, name string, duration time.Duration) {
	if m == nil {
		return
	}
	m.fetchExternalDuration.WithLabelValues(namespace, name).Observe(duration.Seconds())
}

func (m *metricsCollector) incFetchExternalErrors(namespace, name string) {
	if m == nil {
		return
	}
	m.fetchExternalErrors.WithLabelValues(namespace, name).Inc()
}

func (m *metricsCollector) incDriftDetected(namespace, name string) {
	if m == nil {
		return
	}
	m.driftDetectedTotal.WithLabelValues(namespace, name).Inc()
}

func (m *metricsCollector) incRegisteredResources() {
	if m == nil {
		return
	}
	m.registeredResources.Inc()
}

func (m *metricsCollector) decRegisteredResources() {
	if m == nil {
		return
	}
	m.registeredResources.Dec()
}

func (m *metricsCollector) resetRegisteredResources() {
	if m == nil {
		return
	}
	m.registeredResources.Set(0)
}
