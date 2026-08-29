package watcher_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/alperencelik/kube-external-watcher/mock"
	"github.com/alperencelik/kube-external-watcher/watcher"
)

// newTestQueue creates a rate-limiting workqueue suitable for unit tests.
func newTestQueue() workqueue.TypedRateLimitingInterface[reconcile.Request] {
	return workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[reconcile.Request](),
	)
}

// waitForRequest waits for at least one reconcile.Request to land on the
// queue or fails after a 2s timeout. Returns the first request and marks
// it Done so subsequent waits see new items.
func waitForRequest(t *testing.T, q workqueue.TypedRateLimitingInterface[reconcile.Request]) reconcile.Request {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if q.Len() > 0 {
			req, _ := q.Get()
			q.Done(req)
			q.Forget(req)
			return req
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for reconcile request")
	return reconcile.Request{}
}

// drainRequests pops all requests from the queue that arrive within the
// given window. Returns them in arrival order.
func drainRequests(q workqueue.TypedRateLimitingInterface[reconcile.Request], window time.Duration) []reconcile.Request {
	var reqs []reconcile.Request
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if q.Len() > 0 {
			req, _ := q.Get()
			q.Done(req)
			q.Forget(req)
			reqs = append(reqs, req)
			continue
		}
		time.Sleep(5 * time.Millisecond)
	}
	return reqs
}

func TestExternalWatcher_RegisterBeforeStart(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "default", Name: "pre-registered"}
	resourceKey := "resource-pre-registered"
	fetcher.SetDesiredState(key, "desired")
	fetcher.SetResourceState(resourceKey, "different")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(50*time.Millisecond),
	)

	// Register before Start.
	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newTestQueue()
	if err := ew.Start(ctx, q); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	req := waitForRequest(t, q)
	if req.NamespacedName != key {
		t.Errorf("expected request for %v, got %v", key, req.NamespacedName)
	}
}

func TestExternalWatcher_RegisterAfterStart(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "default", Name: "post-start"}
	resourceKey := "resource-post-start"
	fetcher.SetDesiredState(key, "desired")
	fetcher.SetResourceState(resourceKey, "different")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(50*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newTestQueue()
	if err := ew.Start(ctx, q); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Register after Start.
	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})

	waitForRequest(t, q)
}

func TestExternalWatcher_Unregister(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "default", Name: "to-remove"}
	resourceKey := "resource-to-remove"
	fetcher.SetDesiredState(key, "desired")
	fetcher.SetResourceState(resourceKey, "different")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(50*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newTestQueue()
	if err := ew.Start(ctx, q); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})

	// Wait for initial drift event.
	waitForRequest(t, q)

	// Unregister and verify no more events.
	ew.Unregister(key)

	if ew.IsRegistered(key) {
		t.Error("expected resource to be unregistered")
	}

	fetcher.SetResourceState(resourceKey, "changed-after-unregister")
	extra := drainRequests(q, 200*time.Millisecond)
	if len(extra) != 0 {
		t.Errorf("expected no new requests after unregister, got %d", len(extra))
	}
}

func TestExternalWatcher_UnregisterUnknownKey(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	ew := watcher.NewExternalWatcher(fetcher)

	// Should not panic.
	ew.Unregister(types.NamespacedName{Namespace: "default", Name: "unknown"})
}

func TestExternalWatcher_IsRegistered(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "default", Name: "test"}
	fetcher.SetDesiredState(key, "state")

	ew := watcher.NewExternalWatcher(fetcher)

	if ew.IsRegistered(key) {
		t.Error("expected not registered initially")
	}

	ew.Register(key, watcher.ResourceConfig{ResourceKey: "resource-test"})

	if !ew.IsRegistered(key) {
		t.Error("expected registered after Register")
	}

	ew.Unregister(key)

	if ew.IsRegistered(key) {
		t.Error("expected not registered after Unregister")
	}
}

func TestExternalWatcher_StartTwiceReturnsError(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	ew := watcher.NewExternalWatcher(fetcher)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ew.Start(ctx, newTestQueue()); err != nil {
		t.Fatalf("first Start should succeed, got %v", err)
	}
	if err := ew.Start(ctx, newTestQueue()); err == nil {
		t.Fatal("expected second Start to return an error")
	}
}

func TestExternalWatcher_PerResourcePollInterval(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "default", Name: "custom-interval"}
	resourceKey := "resource-custom-interval"
	fetcher.SetDesiredState(key, "desired")
	fetcher.SetResourceState(resourceKey, "different")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(1*time.Hour),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newTestQueue()
	if err := ew.Start(ctx, q); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Register with a short per-resource interval (overriding the 1h default).
	ew.Register(key, watcher.ResourceConfig{
		PollInterval: 50 * time.Millisecond,
		ResourceKey:  resourceKey,
	})

	// Should get drift event quickly.
	waitForRequest(t, q)

	// Fix cloud state to match K8s, then break it again.
	fetcher.SetResourceState(resourceKey, "desired")
	time.Sleep(100 * time.Millisecond)

	fetcher.SetResourceState(resourceKey, "changed-again")

	waitForRequest(t, q)
}

func TestExternalWatcher_ReRegisterUpdatesConfig(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "default", Name: "re-register"}
	resourceKey := "resource-re-register"
	fetcher.SetDesiredState(key, "desired")
	fetcher.SetResourceState(resourceKey, "different")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(50*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newTestQueue()
	if err := ew.Start(ctx, q); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Register, wait for initial drift event.
	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})
	waitForRequest(t, q)

	// Re-register (update config) — should not panic or duplicate.
	ew.Register(key, watcher.ResourceConfig{
		PollInterval: 100 * time.Millisecond,
		ResourceKey:  resourceKey,
	})

	if !ew.IsRegistered(key) {
		t.Error("expected resource still registered after re-register")
	}
}

func TestExternalWatcher_ReRegisterUpdatesResourceKey(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "default", Name: "rekey"}
	oldRK := "old-resource-id"
	newRK := "new-resource-id"
	fetcher.SetDesiredState(key, "desired")
	// Old external identity matches desired (no drift); new identity drifts.
	fetcher.SetResourceState(oldRK, "desired")
	fetcher.SetResourceState(newRK, "different")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(30*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newTestQueue()
	if err := ew.Start(ctx, q); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Register with the old key — polls should observe no drift.
	ew.Register(key, watcher.ResourceConfig{ResourceKey: oldRK})
	if extra := drainRequests(q, 150*time.Millisecond); len(extra) != 0 {
		t.Fatalf("expected no drift while polling the matching old key, got %d requests", len(extra))
	}

	// Re-register with a new external identifier that drifts. If the config
	// update dropped the ResourceKey, the watcher would keep polling oldRK
	// and never enqueue a request.
	ew.Register(key, watcher.ResourceConfig{ResourceKey: newRK})

	req := waitForRequest(t, q)
	if req.NamespacedName != key {
		t.Errorf("expected request for %v, got %v", key, req.NamespacedName)
	}
}

func TestExternalWatcher_ConcurrentRegisterUnregister(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(50*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newTestQueue()
	if err := ew.Start(ctx, q); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := types.NamespacedName{Namespace: "default", Name: "concurrent"}
			resourceKey := "resource-concurrent"
			fetcher.SetDesiredState(key, n)
			fetcher.SetResourceState(resourceKey, n)
			ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})
			ew.IsRegistered(key)
			ew.Unregister(key)
		}(i)
	}
	wg.Wait()
}

func TestExternalWatcher_GracefulShutdown(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "default", Name: "shutdown"}
	resourceKey := "resource-shutdown"
	fetcher.SetDesiredState(key, "desired")
	fetcher.SetResourceState(resourceKey, "different")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(50*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	q := newTestQueue()
	if err := ew.Start(ctx, q); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})
	waitForRequest(t, q)

	// Cancel context — watcher should clean up running resource watchers.
	cancel()

	// Give shutdown a moment to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !ew.IsRegistered(key) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("expected resource unregistered after shutdown")
}

func TestExternalWatcher_EndToEnd(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "production", Name: "my-database"}
	resourceKey := "db-instance-12345"

	kubeState := map[string]string{"status": "running", "version": "14.2"}
	fetcher.SetDesiredState(key, kubeState)
	fetcher.SetResourceState(resourceKey, map[string]string{"status": "stopped", "version": "14.2"})

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(50*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newTestQueue()
	if err := ew.Start(ctx, q); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// 1. Register resource.
	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})

	// 2. Wait for initial drift event (kube says "running", cloud says "stopped").
	req := waitForRequest(t, q)
	if req.NamespacedName != key {
		t.Errorf("expected request for %v, got %v", key, req.NamespacedName)
	}

	// 3. Fix cloud state to match K8s — no more drift.
	fetcher.SetResourceState(resourceKey, kubeState)
	extra := drainRequests(q, 200*time.Millisecond)
	if len(extra) != 0 {
		t.Errorf("expected no new requests after cloud synced with K8s, got %d", len(extra))
	}

	// 4. Introduce new drift — cloud state changes.
	fetcher.SetResourceState(resourceKey, map[string]string{"status": "terminated", "version": "14.2"})
	req = waitForRequest(t, q)
	if req.NamespacedName != key {
		t.Errorf("expected request for %v, got %v", key, req.NamespacedName)
	}

	// 5. Unregister and verify cleanup.
	ew.Unregister(key)
	fetcher.SetResourceState(resourceKey, map[string]string{"status": "deleted", "version": "14.2"})
	extra = drainRequests(q, 200*time.Millisecond)
	if len(extra) != 0 {
		t.Errorf("expected no requests after unregister, got %d", len(extra))
	}
}

func TestExternalWatcher_LastDriftLookup(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "ns", Name: "drift-lookup"}
	resourceKey := "resource-drift-lookup"
	fetcher.SetDesiredState(key, "running")
	fetcher.SetResourceState(resourceKey, "stopped")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(30*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newTestQueue()
	if err := ew.Start(ctx, q); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Unknown key returns false.
	if _, ok := ew.LastDrift(types.NamespacedName{Name: "missing"}); ok {
		t.Error("expected LastDrift on unknown key to return false")
	}

	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})
	waitForRequest(t, q)

	// Reconciler-style lookup: a drift was just observed.
	info, ok := ew.LastDrift(key)
	if !ok {
		t.Fatal("expected LastDrift to return drift info after a drifted poll")
	}
	if info.Key != key {
		t.Errorf("DriftInfo.Key = %v, want %v", info.Key, key)
	}
	if info.Diff == "" {
		t.Error("expected non-empty Diff from default DeepEqualComparator")
	}
}

func TestExternalWatcher_LastDriftAutoClearedOnCleanPoll(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "ns", Name: "auto-clear"}
	resourceKey := "resource-auto-clear"
	fetcher.SetDesiredState(key, "v1")
	fetcher.SetResourceState(resourceKey, "v2")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(30*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := newTestQueue()
	if err := ew.Start(ctx, q); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})
	waitForRequest(t, q)

	if _, ok := ew.LastDrift(key); !ok {
		t.Fatal("expected drift recorded after first drifted poll")
	}

	// External resource brought back into compliance — next poll observes
	// no drift and auto-clears the entry.
	fetcher.SetResourceState(resourceKey, "v1")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := ew.LastDrift(key); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for LastDrift to auto-clear after clean poll")
}
