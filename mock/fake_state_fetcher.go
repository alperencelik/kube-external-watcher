package mock

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/alperencelik/kube-external-watcher/watcher"
)

// DesiredStateFetchCall records a single invocation of GetDesiredState.
type DesiredStateFetchCall struct {
	Key types.NamespacedName
}

// ResourceFetchCall records a single invocation of FetchExternalResource.
type ResourceFetchCall struct {
	ObjKey any
}

// FakeResourceStateFetcher is a programmable ResourceStateFetcher for
// testing. It allows tests to set per-resource desired and external
// states/errors, and records every call for later assertions.
type FakeResourceStateFetcher struct {
	mu sync.RWMutex

	desiredStates map[types.NamespacedName]any
	desiredErrors map[types.NamespacedName]error
	desiredCalls  []DesiredStateFetchCall

	resourceStates map[string]any // keyed by fmt.Sprint(objKey)
	resourceErrors map[string]error
	resourceCalls  []ResourceFetchCall

	readyToWatch map[types.NamespacedName]bool // per-resource readiness; default true
	transformFn  func(any) (any, error)       // optional transform; default identity
}

// Compile-time interface check.
var _ watcher.ResourceStateFetcher = (*FakeResourceStateFetcher)(nil)

// NewFakeResourceStateFetcher creates a new FakeResourceStateFetcher with empty state.
func NewFakeResourceStateFetcher() *FakeResourceStateFetcher {
	return &FakeResourceStateFetcher{
		desiredStates: make(map[types.NamespacedName]any),
		desiredErrors: make(map[types.NamespacedName]error),
		resourceStates:   make(map[string]any),
		resourceErrors:   make(map[string]error),
		readyToWatch:  make(map[types.NamespacedName]bool),
	}
}

// SetDesiredState programs the fake to return the given state for a K8s resource.
func (f *FakeResourceStateFetcher) SetDesiredState(key types.NamespacedName, state any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.desiredStates[key] = state
}

// SetDesiredError programs the fake to return an error for a K8s resource.
// Set to nil to clear a previously configured error.
func (f *FakeResourceStateFetcher) SetDesiredError(key types.NamespacedName, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.desiredErrors, key)
	} else {
		f.desiredErrors[key] = err
	}
}

// SetResourceState programs the fake to return the given state for an external resource.
// The resourceKey is converted to a string key via fmt.Sprint for map storage.
func (f *FakeResourceStateFetcher) SetResourceState(resourceKey any, state any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resourceStates[fmt.Sprint(resourceKey)] = state
}

// SetResourceError programs the fake to return an error for an external resource.
// Set to nil to clear a previously configured error.
func (f *FakeResourceStateFetcher) SetResourceError(resourceKey any, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fmt.Sprint(resourceKey)
	if err == nil {
		delete(f.resourceErrors, k)
	} else {
		f.resourceErrors[k] = err
	}
}

// GetDesiredState implements watcher.ResourceStateFetcher.
func (f *FakeResourceStateFetcher) GetDesiredState(_ context.Context, key types.NamespacedName) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.desiredCalls = append(f.desiredCalls, DesiredStateFetchCall{Key: key})
	if err, ok := f.desiredErrors[key]; ok {
		return nil, err
	}
	return f.desiredStates[key], nil
}

// FetchExternalResource implements watcher.ResourceStateFetcher.
func (f *FakeResourceStateFetcher) FetchExternalResource(_ context.Context, objKey any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resourceCalls = append(f.resourceCalls, ResourceFetchCall{ObjKey: objKey})
	k := fmt.Sprint(objKey)
	if err, ok := f.resourceErrors[k]; ok {
		return nil, err
	}
	return f.resourceStates[k], nil
}

// TransformExternalState implements watcher.ResourceStateFetcher.
// By default returns the raw value as-is. Use SetTransformFn to override.
func (f *FakeResourceStateFetcher) TransformExternalState(raw any) (any, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.transformFn != nil {
		return f.transformFn(raw)
	}
	return raw, nil
}

// SetTransformFn programs the fake to use the given function for
// TransformExternalState. Set to nil to restore the default identity
// transform.
func (f *FakeResourceStateFetcher) SetTransformFn(fn func(any) (any, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transformFn = fn
}

// SetReadyToWatch programs the fake to return the given readiness for a
// resource. By default (key not in map), IsResourceReadyToWatch returns true.
func (f *FakeResourceStateFetcher) SetReadyToWatch(key types.NamespacedName, ready bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readyToWatch[key] = ready
}

// IsResourceReadyToWatch implements watcher.ResourceStateFetcher.
// Returns true by default; use SetReadyToWatch to override per resource.
func (f *FakeResourceStateFetcher) IsResourceReadyToWatch(_ context.Context, key types.NamespacedName) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if ready, ok := f.readyToWatch[key]; ok {
		return ready
	}
	return true
}

// DesiredStateCallCount returns the total number of GetDesiredState invocations.
func (f *FakeResourceStateFetcher) DesiredStateCallCount() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.desiredCalls)
}

// ResourceCallCount returns the total number of FetchExternalResource invocations.
func (f *FakeResourceStateFetcher) ResourceCallCount() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.resourceCalls)
}

// DesiredStateCallsForKey returns the number of GetDesiredState calls for a specific resource.
func (f *FakeResourceStateFetcher) DesiredStateCallsForKey(key types.NamespacedName) int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	count := 0
	for _, c := range f.desiredCalls {
		if c.Key == key {
			count++
		}
	}
	return count
}

// Reset clears all recorded calls, states, and errors.
func (f *FakeResourceStateFetcher) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.desiredCalls = nil
	f.resourceCalls = nil
	f.desiredStates = make(map[types.NamespacedName]any)
	f.desiredErrors = make(map[types.NamespacedName]error)
	f.resourceStates = make(map[string]any)
	f.resourceErrors = make(map[string]error)
	f.readyToWatch = make(map[types.NamespacedName]bool)
}
