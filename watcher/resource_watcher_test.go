package watcher

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp/cmpopts"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// testFetcher is an in-package fake ResourceStateFetcher for testing
// resourceWatcher directly. It is goroutine-safe.
type testFetcher struct {
	mu           sync.RWMutex
	desiredState any
	resourceState   any
	desiredErr   error
	resourceErr     error
	desiredCalls atomic.Int64
	resourceCalls   atomic.Int64
	ready        bool                      // controls IsResourceReadyToWatch; default false (set to true in helpers)
	transformFn  func(any) (any, error)    // optional transform; default identity
}

func (f *testFetcher) setDesiredState(s any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.desiredState = s
}

func (f *testFetcher) setResourceState(s any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resourceState = s
}

func (f *testFetcher) GetDesiredState(_ context.Context, _ types.NamespacedName) (any, error) {
	f.desiredCalls.Add(1)
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.desiredState, f.desiredErr
}

func (f *testFetcher) FetchExternalResource(_ context.Context, _ any) (any, error) {
	f.resourceCalls.Add(1)
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.resourceState, f.resourceErr
}

func (f *testFetcher) IsResourceReadyToWatch(_ context.Context, _ types.NamespacedName) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.ready
}

func (f *testFetcher) TransformExternalState(raw any) (any, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.transformFn != nil {
		return f.transformFn(raw)
	}
	return raw, nil
}

// testFetcherWithStatusUpdater embeds testFetcher and implements
// ResourceStatusUpdater for testing the optional callback.
type testFetcherWithStatusUpdater struct {
	testFetcher
	statusCalls atomic.Int64
	statusErr   error
	lastState   atomic.Value // stores the raw external state passed to UpdateResourceStatus
}

func (f *testFetcherWithStatusUpdater) UpdateResourceStatus(_ context.Context, _ types.NamespacedName, externalState any) error {
	f.statusCalls.Add(1)
	f.lastState.Store(externalState)
	return f.statusErr
}

func TestResourceWatcher_DriftDetectedOnFirstPoll(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{}
	fetcher.setDesiredState("desired")
	fetcher.setResourceState("actual")
	key := types.NamespacedName{Namespace: "default", Name: "test"}

	rw := newResourceWatcher(key, "resource-key-1", 1*time.Hour, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	rw.start(ctx)

	// First poll should detect drift (desired ≠ cloud).
	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "test" || evt.Object.GetNamespace() != "default" {
			t.Errorf("unexpected event: name=%s ns=%s", evt.Object.GetName(), evt.Object.GetNamespace())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for drift event on first poll")
	}

	cancel()
}

func TestResourceWatcher_NoDriftNoEvent(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{}
	fetcher.setDesiredState("same")
	fetcher.setResourceState("same")
	key := types.NamespacedName{Namespace: "default", Name: "test"}

	rw := newResourceWatcher(key, "resource-key-1", 50*time.Millisecond, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	rw.start(ctx)

	// Wait several poll cycles — no events expected since desired = cloud.
	time.Sleep(200 * time.Millisecond)

	select {
	case evt := <-eventCh:
		t.Errorf("unexpected event when states match: %v", evt)
	default:
		// Good — no events.
	}

	cancel()
}

func TestResourceWatcher_DriftTriggersEvent(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{}
	fetcher.setDesiredState("v1")
	fetcher.setResourceState("v1") // Initially in sync.
	key := types.NamespacedName{Namespace: "ns", Name: "db"}

	rw := newResourceWatcher(key, "resource-key-1", 50*time.Millisecond, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	rw.start(ctx)

	// Wait for initial poll to pass without event (states match).
	time.Sleep(30 * time.Millisecond)

	// Change cloud state to introduce drift.
	fetcher.setResourceState("v2")

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "db" || evt.Object.GetNamespace() != "ns" {
			t.Errorf("unexpected event key: name=%s ns=%s", evt.Object.GetName(), evt.Object.GetNamespace())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for drift event")
	}

	cancel()
}

func TestResourceWatcher_DesiredStateFetchErrorContinues(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{desiredErr: errors.New("kube api unavailable")}
	key := types.NamespacedName{Namespace: "default", Name: "test"}

	rw := newResourceWatcher(key, "resource-key-1", 50*time.Millisecond, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	rw.start(ctx)

	time.Sleep(200 * time.Millisecond)
	cancel()

	// No events should have been sent (all desired state fetches errored).
	select {
	case evt := <-eventCh:
		t.Errorf("unexpected event during desired state error condition: %v", evt)
	default:
	}

	// But fetcher was still called multiple times (polls continued).
	if fetcher.desiredCalls.Load() < 2 {
		t.Errorf("expected multiple desired state fetch calls despite errors, got %d", fetcher.desiredCalls.Load())
	}

	// Cloud fetch should NOT have been called (desired state fetch fails first).
	if fetcher.resourceCalls.Load() != 0 {
		t.Errorf("expected no resource fetch calls when desired state fetch errors, got %d", fetcher.resourceCalls.Load())
	}
}

func TestResourceWatcher_CloudFetchErrorContinues(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{resourceErr: errors.New("cloud api unavailable")}
	fetcher.setDesiredState("desired")
	key := types.NamespacedName{Namespace: "default", Name: "test"}

	rw := newResourceWatcher(key, "resource-key-1", 50*time.Millisecond, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	rw.start(ctx)

	time.Sleep(200 * time.Millisecond)
	cancel()

	// No events should have been sent (all cloud fetches errored).
	select {
	case evt := <-eventCh:
		t.Errorf("unexpected event during cloud error condition: %v", evt)
	default:
	}

	// Both fetchers were called (desired state succeeds, cloud fails).
	if fetcher.desiredCalls.Load() < 2 {
		t.Errorf("expected multiple desired state fetch calls, got %d", fetcher.desiredCalls.Load())
	}
	if fetcher.resourceCalls.Load() < 2 {
		t.Errorf("expected multiple resource fetch calls despite errors, got %d", fetcher.resourceCalls.Load())
	}
}

func TestResourceWatcher_ContextCancellation(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{}
	fetcher.setDesiredState("desired")
	fetcher.setResourceState("different")
	key := types.NamespacedName{Namespace: "default", Name: "test"}

	rw := newResourceWatcher(key, "resource-key-1", 1*time.Hour, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	rw.start(ctx)

	// Drain initial drift event.
	select {
	case <-eventCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial drift event")
	}

	// Cancel context — goroutine should exit promptly.
	cancel()
	time.Sleep(100 * time.Millisecond)

	// Change state — should NOT produce event since goroutine stopped.
	fetcher.setResourceState("changed-again")
	time.Sleep(100 * time.Millisecond)

	select {
	case evt := <-eventCh:
		t.Errorf("unexpected event after cancellation: %v", evt)
	default:
	}
}

func TestResourceWatcher_PollIntervalUpdate(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{}
	fetcher.setDesiredState("v1")
	fetcher.setResourceState("v1")
	key := types.NamespacedName{Namespace: "default", Name: "test"}

	rw := newResourceWatcher(key, "resource-key-1", 1*time.Hour, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	if got := rw.currentPollInterval(); got != 1*time.Hour {
		t.Errorf("expected initial poll interval 1h, got %v", got)
	}

	rw.updatePollInterval(30 * time.Second)

	if got := rw.currentPollInterval(); got != 30*time.Second {
		t.Errorf("expected updated poll interval 30s, got %v", got)
	}
}

func TestResourceWatcher_TransformAppliedBeforeComparison(t *testing.T) {
	type desiredState struct {
		Engine        string
		InstanceClass string
	}
	type cloudResponse struct {
		Engine        string
		InstanceClass string
		LastSyncTime  int64
	}

	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{}
	fetcher.setDesiredState(desiredState{Engine: "postgres", InstanceClass: "db.m5.large"})
	fetcher.setResourceState(cloudResponse{Engine: "postgres", InstanceClass: "db.m5.large", LastSyncTime: 12345})
	// Transform strips the cloud-only field so both sides match.
	fetcher.transformFn = func(raw any) (any, error) {
		resp := raw.(cloudResponse)
		return desiredState{
			Engine:        resp.Engine,
			InstanceClass: resp.InstanceClass,
		}, nil
	}
	key := types.NamespacedName{Namespace: "default", Name: "test"}

	rw := newResourceWatcher(key, "resource-key-1", 50*time.Millisecond, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	rw.start(ctx)

	// States match after transform — no event expected.
	time.Sleep(200 * time.Millisecond)

	select {
	case evt := <-eventCh:
		t.Errorf("unexpected event when states match after transform: %v", evt)
	default:
	}

	// Now introduce drift in a relevant field.
	fetcher.setResourceState(cloudResponse{Engine: "mysql", InstanceClass: "db.m5.large", LastSyncTime: 99999})

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "test" {
			t.Errorf("expected event for test, got %s", evt.Object.GetName())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for drift event after engine changed")
	}

	cancel()
}

func TestResourceWatcher_TransformErrorContinues(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{
		transformFn: func(raw any) (any, error) {
			return nil, errors.New("transform failed")
		},
	}
	fetcher.setDesiredState("desired")
	fetcher.setResourceState("raw-cloud")
	key := types.NamespacedName{Namespace: "default", Name: "test"}

	rw := newResourceWatcher(key, "resource-key-1", 50*time.Millisecond, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	rw.start(ctx)

	time.Sleep(200 * time.Millisecond)
	cancel()

	// No events should have been sent (all transforms errored).
	select {
	case evt := <-eventCh:
		t.Errorf("unexpected event during transform error: %v", evt)
	default:
	}

	// Both fetch calls were made (transform fails after fetch succeeds).
	if fetcher.desiredCalls.Load() < 2 {
		t.Errorf("expected multiple desired state fetch calls, got %d", fetcher.desiredCalls.Load())
	}
	if fetcher.resourceCalls.Load() < 2 {
		t.Errorf("expected multiple resource fetch calls, got %d", fetcher.resourceCalls.Load())
	}
}

func TestResourceWatcher_TransformWithCmpOptions(t *testing.T) {
	type normalizedState struct {
		Engine        string
		InstanceClass string
		Tags          []string
	}
	type cloudResponse struct {
		Engine        string
		InstanceClass string
		Tags          []string
		UpdatedAt     int64
	}

	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{}
	fetcher.setDesiredState(normalizedState{
		Engine:        "postgres",
		InstanceClass: "db.m5.large",
		Tags:          []string{"prod", "us-east"},
	})
	// Cloud returns tags in different order — should not drift with SortSlices.
	fetcher.setResourceState(cloudResponse{
		Engine:        "postgres",
		InstanceClass: "db.m5.large",
		Tags:          []string{"us-east", "prod"},
		UpdatedAt:     99999,
	})
	fetcher.transformFn = func(raw any) (any, error) {
		resp := raw.(cloudResponse)
		return normalizedState{
			Engine:        resp.Engine,
			InstanceClass: resp.InstanceClass,
			Tags:          resp.Tags,
		}, nil
	}
	key := types.NamespacedName{Namespace: "default", Name: "test"}

	comparator := NewDeepEqualComparator(
		cmpopts.SortSlices(func(a, b string) bool { return a < b }),
	)

	rw := newResourceWatcher(key, "resource-key-1", 50*time.Millisecond, fetcher, comparator, eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	rw.start(ctx)

	// Tag order differs but SortSlices makes them equal — no drift.
	time.Sleep(200 * time.Millisecond)

	select {
	case evt := <-eventCh:
		t.Errorf("unexpected event when only tag order differs: %v", evt)
	default:
	}

	cancel()
}

func TestResourceWatcher_StatusUpdaterCalledOnEveryPoll(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcherWithStatusUpdater{}
	fetcher.setDesiredState("same")
	fetcher.setResourceState("same") // no drift
	key := types.NamespacedName{Namespace: "default", Name: "test-status"}

	rw := newResourceWatcher(key, "rk", 50*time.Millisecond, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	rw.start(ctx)

	time.Sleep(200 * time.Millisecond)
	cancel()

	// Status updater should have been called on every poll, even without drift.
	if calls := fetcher.statusCalls.Load(); calls < 2 {
		t.Errorf("expected multiple status update calls, got %d", calls)
	}

	// No drift events should have been sent.
	select {
	case evt := <-eventCh:
		t.Errorf("unexpected drift event: %v", evt)
	default:
	}
}

func TestResourceWatcher_StatusUpdaterReceivesRawState(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcherWithStatusUpdater{}
	fetcher.setDesiredState("desired")
	fetcher.setResourceState("raw-external-state")
	// Transform changes the state, but UpdateResourceStatus should get the raw value.
	fetcher.transformFn = func(raw any) (any, error) {
		return "transformed", nil
	}
	key := types.NamespacedName{Namespace: "default", Name: "test-raw"}

	rw := newResourceWatcher(key, "rk", 1*time.Hour, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rw.poll(ctx)

	if got := fetcher.lastState.Load(); got != "raw-external-state" {
		t.Errorf("expected raw state passed to UpdateResourceStatus, got %v", got)
	}
}

func TestResourceWatcher_StatusUpdaterErrorDoesNotBlockPoll(t *testing.T) {
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcherWithStatusUpdater{statusErr: errors.New("status update failed")}
	fetcher.setDesiredState("desired")
	fetcher.setResourceState("different") // drift exists
	key := types.NamespacedName{Namespace: "default", Name: "test-status-err"}

	rw := newResourceWatcher(key, "rk", 1*time.Hour, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rw.poll(ctx)

	// Status update errored, but drift detection should still proceed.
	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "test-status-err" {
			t.Errorf("expected event for test-status-err, got %s", evt.Object.GetName())
		}
	default:
		t.Fatal("expected drift event despite status update error")
	}

	if fetcher.statusCalls.Load() != 1 {
		t.Errorf("expected 1 status update call, got %d", fetcher.statusCalls.Load())
	}
}

func TestResourceWatcher_WithoutStatusUpdaterStillWorks(t *testing.T) {
	// testFetcher does NOT implement ResourceStatusUpdater — poll should work normally.
	eventCh := make(chan event.GenericEvent, 10)
	fetcher := &testFetcher{}
	fetcher.setDesiredState("desired")
	fetcher.setResourceState("different")
	key := types.NamespacedName{Namespace: "default", Name: "test-no-updater"}

	rw := newResourceWatcher(key, "rk", 1*time.Hour, fetcher, NewDeepEqualComparator(), eventCh, logr.Discard(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rw.poll(ctx)

	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "test-no-updater" {
			t.Errorf("expected event for test-no-updater, got %s", evt.Object.GetName())
		}
	default:
		t.Fatal("expected drift event without status updater")
	}
}
