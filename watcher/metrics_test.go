package watcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func newTestMetrics(t *testing.T) (*metricsCollector, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	vecs := newMetricsVecs(reg)
	m := newMetricsCollectorWithVecs("test-controller", vecs)
	return m, reg
}

func getCounterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if matchLabels(m.GetLabel(), labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func getGaugeValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if matchLabels(m.GetLabel(), labels) {
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func getHistogramCount(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if matchLabels(m.GetLabel(), labels) {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

func matchLabels(pairs []*io_prometheus_client.LabelPair, expected map[string]string) bool {
	if len(pairs) != len(expected) {
		return false
	}
	for _, p := range pairs {
		v, ok := expected[p.GetName()]
		if !ok || v != p.GetValue() {
			return false
		}
	}
	return true
}

func TestNilMetricsCollector_NoPanic(t *testing.T) {
	var m *metricsCollector
	// All nil-receiver methods should be no-ops.
	m.incPollTotal("ns", "name", "success")
	m.observeFetchDuration("ns", "name", time.Second)
	m.incFetchExternalErrors("ns", "name")
	m.incDriftDetected("ns", "name")
	m.incRegisteredResources()
	m.decRegisteredResources()
	m.resetRegisteredResources()
}

func TestMetrics_PollSuccessIncrementsCounter(t *testing.T) {
	m, reg := newTestMetrics(t)
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{}
	fetcher.setDesiredState("state")
	fetcher.setResourceState("state") // no drift
	key := types.NamespacedName{Namespace: "default", Name: "test-poll"}

	rw := newResourceWatcher(key, "rk", 1*time.Hour, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rw.poll(ctx)

	labels := map[string]string{"controller": "test-controller", "namespace": "default", "name": "test-poll", "result": "success"}
	if v := getCounterValue(t, reg, "kube_external_watcher_poll_total", labels); v != 1 {
		t.Errorf("expected poll_total{success}=1, got %v", v)
	}
}

func TestMetrics_PollErrorIncrementsCounter(t *testing.T) {
	m, reg := newTestMetrics(t)
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{desiredErr: errors.New("fail")}
	key := types.NamespacedName{Namespace: "default", Name: "test-err"}

	rw := newResourceWatcher(key, "rk", 1*time.Hour, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rw.poll(ctx)

	labels := map[string]string{"controller": "test-controller", "namespace": "default", "name": "test-err", "result": "error"}
	if v := getCounterValue(t, reg, "kube_external_watcher_poll_total", labels); v != 1 {
		t.Errorf("expected poll_total{error}=1, got %v", v)
	}
}

func TestMetrics_FetchExternalErrorIncrementsCounter(t *testing.T) {
	m, reg := newTestMetrics(t)
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{resourceErr: errors.New("api fail")}
	fetcher.setDesiredState("desired")
	key := types.NamespacedName{Namespace: "default", Name: "test-fetch-err"}

	rw := newResourceWatcher(key, "rk", 1*time.Hour, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rw.poll(ctx)

	labels := map[string]string{"controller": "test-controller", "namespace": "default", "name": "test-fetch-err"}
	if v := getCounterValue(t, reg, "kube_external_watcher_fetch_external_errors_total", labels); v != 1 {
		t.Errorf("expected fetch_external_errors=1, got %v", v)
	}

	pollLabels := map[string]string{"controller": "test-controller", "namespace": "default", "name": "test-fetch-err", "result": "error"}
	if v := getCounterValue(t, reg, "kube_external_watcher_poll_total", pollLabels); v != 1 {
		t.Errorf("expected poll_total{error}=1, got %v", v)
	}
}

func TestMetrics_FetchDurationRecorded(t *testing.T) {
	m, reg := newTestMetrics(t)
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{}
	fetcher.setDesiredState("state")
	fetcher.setResourceState("state")
	key := types.NamespacedName{Namespace: "default", Name: "test-duration"}

	rw := newResourceWatcher(key, "rk", 1*time.Hour, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rw.poll(ctx)

	labels := map[string]string{"controller": "test-controller", "namespace": "default", "name": "test-duration"}
	if c := getHistogramCount(t, reg, "kube_external_watcher_fetch_external_duration_seconds", labels); c != 1 {
		t.Errorf("expected histogram sample count=1, got %v", c)
	}
}

func TestMetrics_DriftDetectedIncrementsCounter(t *testing.T) {
	m, reg := newTestMetrics(t)
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{}
	fetcher.setDesiredState("desired")
	fetcher.setResourceState("different")
	key := types.NamespacedName{Namespace: "default", Name: "test-drift"}

	rw := newResourceWatcher(key, "rk", 1*time.Hour, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rw.poll(ctx)

	labels := map[string]string{"controller": "test-controller", "namespace": "default", "name": "test-drift"}
	if v := getCounterValue(t, reg, "kube_external_watcher_drift_detected_total", labels); v != 1 {
		t.Errorf("expected drift_detected=1, got %v", v)
	}
}

func TestMetrics_RegisteredResourcesGauge(t *testing.T) {
	m, reg := newTestMetrics(t)

	labels := map[string]string{"controller": "test-controller"}

	m.incRegisteredResources()
	m.incRegisteredResources()
	if v := getGaugeValue(t, reg, "kube_external_watcher_registered_resources", labels); v != 2 {
		t.Errorf("expected registered_resources=2, got %v", v)
	}

	m.decRegisteredResources()
	if v := getGaugeValue(t, reg, "kube_external_watcher_registered_resources", labels); v != 1 {
		t.Errorf("expected registered_resources=1, got %v", v)
	}

	m.resetRegisteredResources()
	if v := getGaugeValue(t, reg, "kube_external_watcher_registered_resources", labels); v != 0 {
		t.Errorf("expected registered_resources=0, got %v", v)
	}
}
