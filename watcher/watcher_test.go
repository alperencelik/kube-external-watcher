package watcher_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/alperencelik/kube-external-watcher/mock"
	"github.com/alperencelik/kube-external-watcher/watcher"
)

// waitForEvents drains n events from ch or fails after timeout.
func waitForEvents(t *testing.T, ch <-chan event.GenericEvent, n int, timeout time.Duration) []event.GenericEvent {
	t.Helper()
	var events []event.GenericEvent
	deadline := time.After(timeout)
	for range n {
		select {
		case evt := <-ch:
			events = append(events, evt)
		case <-deadline:
			t.Fatalf("timed out after %v: got %d events, expected %d", timeout, len(events), n)
		}
	}
	return events
}

// drainEvents returns all events available within the given window.
func drainEvents(ch <-chan event.GenericEvent, window time.Duration) []event.GenericEvent {
	var events []event.GenericEvent
	deadline := time.After(window)
	for {
		select {
		case evt := <-ch:
			events = append(events, evt)
		case <-deadline:
			return events
		}
	}
}

func eventKey(evt event.GenericEvent) types.NamespacedName {
	return types.NamespacedName{
		Name:      evt.Object.GetName(),
		Namespace: evt.Object.GetNamespace(),
	}
}

func TestExternalWatcher_RegisterBeforeStart(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "default", Name: "pre-registered"}
	resourceKey := "resource-pre-registered"
	fetcher.SetDesiredState(key, "desired")
	fetcher.SetResourceState(resourceKey, "different")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(50*time.Millisecond),
		watcher.WithEventChannelBufferSize(10),
	)
	ch := ew.EventChannel()

	// Register before Start.
	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ew.Start(ctx)

	// The pre-registered watcher should detect drift on start.
	events := waitForEvents(t, ch, 1, 2*time.Second)
	if eventKey(events[0]) != key {
		t.Errorf("expected event for %v, got %v", key, eventKey(events[0]))
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
		watcher.WithEventChannelBufferSize(10),
	)
	ch := ew.EventChannel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ew.Start(ctx)

	// Brief wait for Start to execute.
	time.Sleep(50 * time.Millisecond)

	// Register after Start.
	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})

	waitForEvents(t, ch, 1, 2*time.Second)
}

func TestExternalWatcher_Unregister(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "default", Name: "to-remove"}
	resourceKey := "resource-to-remove"
	fetcher.SetDesiredState(key, "desired")
	fetcher.SetResourceState(resourceKey, "different")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(50*time.Millisecond),
		watcher.WithEventChannelBufferSize(10),
	)
	ch := ew.EventChannel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ew.Start(ctx)

	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})

	// Wait for initial drift event.
	waitForEvents(t, ch, 1, 2*time.Second)

	// Unregister and verify no more events.
	ew.Unregister(key)

	if ew.IsRegistered(key) {
		t.Error("expected resource to be unregistered")
	}

	fetcher.SetResourceState(resourceKey, "changed-after-unregister")
	extra := drainEvents(ch, 200*time.Millisecond)
	if len(extra) != 0 {
		t.Errorf("expected no new events after unregister, got %d", len(extra))
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

func TestExternalWatcher_NeedLeaderElection(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	ew := watcher.NewExternalWatcher(fetcher)

	if !ew.NeedLeaderElection() {
		t.Error("expected NeedLeaderElection to return true")
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
		watcher.WithEventChannelBufferSize(10),
	)
	ch := ew.EventChannel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ew.Start(ctx)

	// Register with a short per-resource interval (overriding the 1h default).
	ew.Register(key, watcher.ResourceConfig{
		PollInterval: 50 * time.Millisecond,
		ResourceKey:  resourceKey,
	})

	// Should get drift event quickly.
	waitForEvents(t, ch, 1, 2*time.Second)

	// Fix cloud state to match K8s, then break it again.
	fetcher.SetResourceState(resourceKey, "desired")
	time.Sleep(100 * time.Millisecond)

	fetcher.SetResourceState(resourceKey, "changed-again")

	waitForEvents(t, ch, 1, 2*time.Second)
}

func TestExternalWatcher_ReRegisterUpdatesConfig(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	key := types.NamespacedName{Namespace: "default", Name: "re-register"}
	resourceKey := "resource-re-register"
	fetcher.SetDesiredState(key, "desired")
	fetcher.SetResourceState(resourceKey, "different")

	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(50*time.Millisecond),
		watcher.WithEventChannelBufferSize(10),
	)
	ch := ew.EventChannel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ew.Start(ctx)

	// Register, wait for initial drift event.
	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})
	waitForEvents(t, ch, 1, 2*time.Second)

	// Re-register (update config) — should not panic or duplicate.
	ew.Register(key, watcher.ResourceConfig{
		PollInterval: 100 * time.Millisecond,
		ResourceKey:  resourceKey,
	})

	if !ew.IsRegistered(key) {
		t.Error("expected resource still registered after re-register")
	}
}

func TestExternalWatcher_ConcurrentRegisterUnregister(t *testing.T) {
	fetcher := mock.NewFakeResourceStateFetcher()
	ew := watcher.NewExternalWatcher(fetcher,
		watcher.WithDefaultPollInterval(50*time.Millisecond),
		watcher.WithEventChannelBufferSize(100),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ew.Start(ctx)
	time.Sleep(50 * time.Millisecond)

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
		watcher.WithEventChannelBufferSize(10),
	)
	ch := ew.EventChannel()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ew.Start(ctx)
	}()

	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})
	waitForEvents(t, ch, 1, 2*time.Second)

	// Cancel context — Start should return promptly.
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error from Start, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}

	// After shutdown, resource should be cleaned up.
	if ew.IsRegistered(key) {
		t.Error("expected resource unregistered after shutdown")
	}
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
		watcher.WithEventChannelBufferSize(10),
	)
	ch := ew.EventChannel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ew.Start(ctx)

	// 1. Register resource.
	ew.Register(key, watcher.ResourceConfig{ResourceKey: resourceKey})

	// 2. Wait for initial drift event (kube says "running", cloud says "stopped").
	events := waitForEvents(t, ch, 1, 2*time.Second)
	if eventKey(events[0]) != key {
		t.Errorf("expected event for %v, got %v", key, eventKey(events[0]))
	}

	// 3. Fix cloud state to match K8s — no more drift.
	fetcher.SetResourceState(resourceKey, kubeState)
	extra := drainEvents(ch, 200*time.Millisecond)
	if len(extra) != 0 {
		t.Errorf("expected no new events after cloud synced with K8s, got %d", len(extra))
	}

	// 4. Introduce new drift — cloud state changes.
	fetcher.SetResourceState(resourceKey, map[string]string{"status": "terminated", "version": "14.2"})
	events = waitForEvents(t, ch, 1, 2*time.Second)
	if eventKey(events[0]) != key {
		t.Errorf("expected event for %v, got %v", key, eventKey(events[0]))
	}

	// 5. Unregister and verify cleanup.
	ew.Unregister(key)
	fetcher.SetResourceState(resourceKey, map[string]string{"status": "deleted", "version": "14.2"})
	extra = drainEvents(ch, 200*time.Millisecond)
	if len(extra) != 0 {
		t.Errorf("expected no events after unregister, got %d", len(extra))
	}
}
