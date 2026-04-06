package watcher

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// ResourceStateFetcher bridges Kubernetes and external resource state
// for drift detection. Users implement this interface to provide the
// watcher with both sides of the comparison.
type ResourceStateFetcher interface {
	// GetDesiredState retrieves the desired state from the Kubernetes CR
	// corresponding to the given key. Implementations typically use a
	// controller-runtime client.Reader to fetch the CR and extract the
	// fields that should be compared against the external resource state.
	GetDesiredState(ctx context.Context, key types.NamespacedName) (any, error)

	// FetchExternalResource retrieves the raw state from an external
	// API (e.g., a cloud provider). The objKey is the resource identifier
	// whose type depends on the external API (e.g., instance ID string,
	// ARN, numeric ID, composite struct). The value comes from
	// ResourceConfig.ResourceKey provided at registration time.
	//
	// The returned value is passed to TransformExternalState to
	// normalize it into the same shape as the desired state before
	// comparison.
	FetchExternalResource(ctx context.Context, objKey any) (any, error)

	// TransformExternalState normalizes the raw external API response
	// (from FetchExternalResource) into the same shape as the desired
	// state (from GetDesiredState), so both sides can be compared
	// like-for-like by the StateComparator. Called in the poll loop
	// after FetchExternalResource and before HasDrifted.
	TransformExternalState(raw any) (any, error)

	// IsResourceReadyToWatch returns whether the resource identified by
	// key is ready to be watched. Auto-registration calls this before
	// registering a resource — if it returns false, the resource is
	// skipped (or unregistered if it was previously registered).
	// Implementations typically fetch the CR and check whether the
	// external resource has been provisioned (e.g. status.instanceID
	// is non-empty).
	IsResourceReadyToWatch(ctx context.Context, key types.NamespacedName) bool
}

// ResourceStatusUpdater is an optional interface that ResourceStateFetcher
// implementations can implement to receive a callback on every successful
// poll cycle. This runs after FetchExternalResource succeeds, regardless
// of whether drift was detected — useful for syncing external state
// (e.g., uptime, IP addresses, power state) into the Kubernetes resource's
// .status on every poll.
type ResourceStatusUpdater interface {
	UpdateResourceStatus(ctx context.Context, key types.NamespacedName, externalState any) error
}

// StateComparator determines whether the desired state and the
// (transformed) external state differ. Both inputs are expected to be
// in the same shape — the fetcher's TransformExternalState normalizes
// the external API response before HasDrifted is called. If drift is
// detected, the watcher triggers reconciliation for the corresponding
// resource.
//
// The default DeepEqualComparator uses github.com/google/go-cmp/cmp
// with user-provided cmp.Options for fine-grained comparison control
// (e.g. cmpopts.IgnoreFields, cmpopts.SortSlices).
type StateComparator interface {
	// HasDrifted returns true if the desired and actual state differ
	// in a way that should trigger reconciliation. Both inputs should
	// already be in the same shape (transformation is done upstream).
	HasDrifted(desired, actual any) (bool, error)
}

// WatcherManager defines the interface for registering and unregistering
// resources with the external watcher. The reconciler calls these methods
// during its Reconcile loop.
type WatcherManager interface {
	// Register starts watching the external state for the given resource.
	// If the resource is already registered, its configuration is updated.
	// Goroutine-safe.
	Register(key types.NamespacedName, config ResourceConfig)

	// Unregister stops watching the external state for the given resource.
	// No-op if the key is not registered.
	Unregister(key types.NamespacedName)

	// IsRegistered returns whether a resource is currently being watched.
	IsRegistered(key types.NamespacedName) bool
}

// ResourceConfig holds per-resource configuration for the watcher.
type ResourceConfig struct {
	// PollInterval overrides the global default poll interval for this
	// specific resource. If zero, the global default is used.
	PollInterval time.Duration

	// ResourceKey is the external resource identifier passed to
	// ResourceStateFetcher.FetchExternalResource. Its type depends on
	// the external API — for example, a string instance ID for AWS EC2,
	// an ARN, a numeric ID, or a composite struct.
	ResourceKey any
}
