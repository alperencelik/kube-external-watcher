package watcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

type fakeRegistration struct{}

func (f *fakeRegistration) HasSynced() bool { return true }

type fakeInformer struct {
	cache.Informer // embed interface for only AddEventHandler is implemented
	handler        toolscache.ResourceEventHandler
}

func (f *fakeInformer) AddEventHandler(handler toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	f.handler = handler
	return &fakeRegistration{}, nil
}

type fakeCache struct {
	cache.Cache // embed interface for only GetInformer is implemented
	informer    *fakeInformer
	getErr      error
}

func (f *fakeCache) GetInformer(_ context.Context, _ client.Object, _ ...cache.InformerGetOption) (cache.Informer, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.informer, nil
}

// helpers

// newTestObj creates an objectReference for use as a simulated informer event.
func newTestObj(ns, name string) client.Object {
	return &objectReference{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
	}
}

// setupTestWatcher creates an ExternalWatcher configured with auto-register
// and calls setupAutoRegister. Returns the watcher, the fake informer
// (so tests can fire events), and the event channel. The testFetcher is
// returned so tests can control readiness via the ready field.
func setupTestWatcher(t *testing.T, extractor ConfigExtractorFn) (*ExternalWatcher, *fakeInformer, chan event.GenericEvent, *testFetcher) {
	t.Helper()
	fi := &fakeInformer{}
	fc := &fakeCache{informer: fi}

	fetcher := &testFetcher{ready: true}
	fetcher.setDesiredState("desired")
	fetcher.setResourceState("different")

	w := &ExternalWatcher{
		fetcher:                fetcher,
		comparator:             NewDeepEqualComparator(),
		defaultPollInterval:    50 * time.Millisecond,
		eventChannelBufferSize: 10,
		logger:                 logr.Discard(),
		watchers:               make(map[types.NamespacedName]*resourceWatcher),
		autoRegister: &autoRegisterConfig{
			cache:     fc,
			obj:       &objectReference{},
			extractor: extractor,
		},
	}
	w.eventCh = make(chan event.GenericEvent, w.eventChannelBufferSize)

	if err := setupAutoRegister(context.Background(), w); err != nil {
		t.Fatalf("setupAutoRegister failed: %v", err)
	}

	return w, fi, w.eventCh, fetcher
}

// startTestWatcher marks the watcher as started so Register spawns goroutines.
func startTestWatcher(t *testing.T, w *ExternalWatcher) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.ctx = ctx
	w.started = true
	w.mu.Unlock()
	return cancel
}

// Tests

func TestAutoRegister_AddEventRegistersResource(t *testing.T) {
	w, fi, eventCh, _ := setupTestWatcher(t, func(obj client.Object) ResourceConfig {
		return ResourceConfig{ResourceKey: "resource-" + obj.GetName()}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.mu.Lock()
	w.ctx = ctx
	w.started = true
	w.mu.Unlock()

	// Simulate an Add event from the informer.
	obj := newTestObj("default", "my-resource")
	fi.handler.OnAdd(obj, false)

	key := types.NamespacedName{Namespace: "default", Name: "my-resource"}
	if !w.IsRegistered(key) {
		t.Fatal("expected resource to be registered after Add event")
	}

	// The resource watcher should detect drift and emit an event.
	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "my-resource" {
			t.Errorf("expected event for my-resource, got %s", evt.Object.GetName())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for drift event after auto-register")
	}
}

func TestAutoRegister_UpdateEventUpdatesConfig(t *testing.T) {
	callCount := 0
	w, fi, _, _ := setupTestWatcher(t, func(obj client.Object) ResourceConfig {
		callCount++
		return ResourceConfig{
			PollInterval: time.Duration(callCount) * time.Second,
			ResourceKey:  "resource-" + obj.GetName(),
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.mu.Lock()
	w.ctx = ctx
	w.started = true
	w.mu.Unlock()

	obj := newTestObj("default", "updatable")

	// Add then Update.
	fi.handler.OnAdd(obj, false)
	fi.handler.OnUpdate(obj, obj)

	key := types.NamespacedName{Namespace: "default", Name: "updatable"}
	if !w.IsRegistered(key) {
		t.Fatal("expected resource to remain registered after Update event")
	}

	// Extractor was called twice (once for add, once for update).
	if callCount != 2 {
		t.Errorf("expected extractor called 2 times, got %d", callCount)
	}
}

func TestAutoRegister_DeleteEventUnregistersResource(t *testing.T) {
	w, fi, _, _ := setupTestWatcher(t, func(obj client.Object) ResourceConfig {
		return ResourceConfig{ResourceKey: "resource-" + obj.GetName()}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.mu.Lock()
	w.ctx = ctx
	w.started = true
	w.mu.Unlock()

	obj := newTestObj("default", "to-delete")

	// Add then Delete.
	fi.handler.OnAdd(obj, false)

	key := types.NamespacedName{Namespace: "default", Name: "to-delete"}
	if !w.IsRegistered(key) {
		t.Fatal("expected resource to be registered after Add")
	}

	fi.handler.OnDelete(obj)

	if w.IsRegistered(key) {
		t.Error("expected resource to be unregistered after Delete event")
	}
}

func TestAutoRegister_DeleteTombstoneHandled(t *testing.T) {
	w, fi, _, _ := setupTestWatcher(t, func(obj client.Object) ResourceConfig {
		return ResourceConfig{ResourceKey: "resource-" + obj.GetName()}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.mu.Lock()
	w.ctx = ctx
	w.started = true
	w.mu.Unlock()

	obj := newTestObj("default", "tombstone-obj")
	fi.handler.OnAdd(obj, false)

	key := types.NamespacedName{Namespace: "default", Name: "tombstone-obj"}
	if !w.IsRegistered(key) {
		t.Fatal("expected resource to be registered after Add")
	}

	// Simulate a DeletedFinalStateUnknown tombstone.
	tombstone := toolscache.DeletedFinalStateUnknown{
		Key: "default/tombstone-obj",
		Obj: obj,
	}
	fi.handler.OnDelete(tombstone)

	if w.IsRegistered(key) {
		t.Error("expected resource to be unregistered after tombstone Delete")
	}
}

func TestAutoRegister_GetInformerError(t *testing.T) {
	fc := &fakeCache{getErr: errors.New("informer unavailable")}

	w := &ExternalWatcher{
		fetcher:                &testFetcher{},
		comparator:             NewDeepEqualComparator(),
		defaultPollInterval:    50 * time.Millisecond,
		eventChannelBufferSize: 10,
		logger:                 logr.Discard(),
		watchers:               make(map[types.NamespacedName]*resourceWatcher),
		autoRegister: &autoRegisterConfig{
			cache:     fc,
			obj:       &objectReference{},
			extractor: func(obj client.Object) ResourceConfig { return ResourceConfig{} },
		},
	}
	w.eventCh = make(chan event.GenericEvent, 10)

	err := setupAutoRegister(context.Background(), w)
	if err == nil {
		t.Fatal("expected error when GetInformer fails")
	}
}

func TestAutoRegister_NotReadySkipsRegistration(t *testing.T) {
	w, fi, _, fetcher := setupTestWatcher(t, func(obj client.Object) ResourceConfig {
		return ResourceConfig{ResourceKey: "resource-" + obj.GetName()}
	})

	// Mark not ready via fetcher.
	fetcher.mu.Lock()
	fetcher.ready = false
	fetcher.mu.Unlock()

	cancel := startTestWatcher(t, w)
	defer cancel()

	obj := newTestObj("default", "not-ready")
	fi.handler.OnAdd(obj, false)

	key := types.NamespacedName{Namespace: "default", Name: "not-ready"}
	if w.IsRegistered(key) {
		t.Fatal("expected not-ready resource to be skipped on Add")
	}
}

func TestAutoRegister_BecomesReadyOnUpdate(t *testing.T) {
	w, fi, eventCh, fetcher := setupTestWatcher(t, func(obj client.Object) ResourceConfig {
		return ResourceConfig{ResourceKey: "resource-" + obj.GetName()}
	})

	// Start not ready.
	fetcher.mu.Lock()
	fetcher.ready = false
	fetcher.mu.Unlock()

	cancel := startTestWatcher(t, w)
	defer cancel()

	obj := newTestObj("default", "deferred")
	key := types.NamespacedName{Namespace: "default", Name: "deferred"}

	// Add while not ready — should be skipped.
	fi.handler.OnAdd(obj, false)
	if w.IsRegistered(key) {
		t.Fatal("expected resource to be skipped on Add when not ready")
	}

	// Now mark ready via fetcher and send Update.
	fetcher.mu.Lock()
	fetcher.ready = true
	fetcher.mu.Unlock()
	fi.handler.OnUpdate(obj, obj)

	if !w.IsRegistered(key) {
		t.Fatal("expected resource to be registered after Update when ready")
	}

	// Should emit a drift event.
	select {
	case evt := <-eventCh:
		if evt.Object.GetName() != "deferred" {
			t.Errorf("expected event for deferred, got %s", evt.Object.GetName())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for drift event")
	}
}

func TestAutoRegister_UnregistersWhenNoLongerReady(t *testing.T) {
	w, fi, _, fetcher := setupTestWatcher(t, func(obj client.Object) ResourceConfig {
		return ResourceConfig{ResourceKey: "resource-" + obj.GetName()}
	})

	cancel := startTestWatcher(t, w)
	defer cancel()

	obj := newTestObj("default", "transient")
	key := types.NamespacedName{Namespace: "default", Name: "transient"}

	// Add while ready — should register.
	fi.handler.OnAdd(obj, false)
	if !w.IsRegistered(key) {
		t.Fatal("expected resource to be registered on Add when ready")
	}

	// Mark not ready via fetcher and send Update — should unregister.
	fetcher.mu.Lock()
	fetcher.ready = false
	fetcher.mu.Unlock()
	fi.handler.OnUpdate(obj, obj)

	if w.IsRegistered(key) {
		t.Fatal("expected resource to be unregistered after Update when no longer ready")
	}
}
