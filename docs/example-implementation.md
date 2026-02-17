# Example Implementation

This guide walks through integrating `kube-external-watcher` into a Kubernetes operator that manages a cloud database resource. The example assumes a CRD called `Database` with a spec/status pattern.

## Prerequisites

- A controller-runtime based operator
- A CRD with a status field that holds the external resource identifier (e.g. `status.instanceID`)

## 1. Define your CRD types

```go
// api/v1/database_types.go
package v1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DatabaseSpec struct {
    // Engine is the database engine (e.g. "postgres", "mysql").
    Engine string `json:"engine"`

    // InstanceClass is the cloud instance type.
    InstanceClass string `json:"instanceClass"`

    // PollInterval overrides the global poll interval for this resource.
    // If zero, the global default is used.
    PollInterval metav1.Duration `json:"pollInterval,omitempty"`
}

type DatabaseStatus struct {
    // InstanceID is the cloud provider's identifier for the database instance.
    // Empty until the instance is provisioned.
    InstanceID string `json:"instanceID,omitempty"`

    // State is the current state of the cloud database (e.g. "available", "creating").
    State string `json:"state,omitempty"`
}

type Database struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   DatabaseSpec   `json:"spec,omitempty"`
    Status DatabaseStatus `json:"status,omitempty"`
}
```

## 2. Implement `ResourceStateFetcher`

The fetcher is the bridge between your operator and the watcher library. It implements four methods:

```go
// internal/fetcher/database_fetcher.go
package fetcher

import (
    "context"

    "k8s.io/apimachinery/pkg/types"
    "sigs.k8s.io/controller-runtime/pkg/client"

    myv1 "example.com/database-operator/api/v1"
)

// DatabaseFetcher implements watcher.ResourceStateFetcher for Database CRs.
type DatabaseFetcher struct {
    Client      client.Reader
    CloudClient CloudDatabaseAPI // your cloud SDK client
}

// GetDesiredState fetches the Kubernetes CR and extracts the fields
// relevant for drift comparison. The returned value is compared against
// the transformed cloud state by the StateComparator.
func (f *DatabaseFetcher) GetDesiredState(ctx context.Context, key types.NamespacedName) (any, error) {
    var db myv1.Database
    if err := f.Client.Get(ctx, key, &db); err != nil {
        return nil, err
    }
    // Return only the fields you want to compare against cloud state.
    return DatabaseState{
        Engine:        db.Spec.Engine,
        InstanceClass: db.Spec.InstanceClass,
    }, nil
}

// FetchExternalResource calls the cloud API to get the raw state of the
// database instance. The objKey is the ResourceKey from ResourceConfig —
// in this case, the instance ID string.
func (f *DatabaseFetcher) FetchExternalResource(ctx context.Context, objKey any) (any, error) {
    instanceID := objKey.(string)
    instance, err := f.CloudClient.DescribeInstance(ctx, instanceID)
    if err != nil {
        return nil, err
    }
    return instance, nil
}

// TransformExternalState normalizes the raw cloud API response into
// the same DatabaseState shape used by GetDesiredState. Called in the
// poll loop after FetchExternalResource and before HasDrifted.
func (f *DatabaseFetcher) TransformExternalState(raw any) (any, error) {
    instance := raw.(*CloudDatabaseInstance)
    return DatabaseState{
        Engine:        instance.Engine,
        InstanceClass: instance.InstanceClass,
    }, nil
}

// IsResourceReadyToWatch checks whether the Database CR has been
// provisioned (i.e. has an instance ID in its status). The watcher
// calls this before registering a resource via auto-register.
func (f *DatabaseFetcher) IsResourceReadyToWatch(ctx context.Context, key types.NamespacedName) bool {
    var db myv1.Database
    if err := f.Client.Get(ctx, key, &db); err != nil {
        return false
    }
    return db.Status.InstanceID != ""
}

// DatabaseState is the normalized shape used for drift comparison.
// Both GetDesiredState and TransformExternalState produce this type.
type DatabaseState struct {
    Engine        string
    InstanceClass string
}
```

## 3. Wire everything together

### Option A: Auto-register (recommended)

Auto-register hooks into the controller-runtime cache informer. When a `Database` CR is created/updated, the watcher automatically checks readiness and registers it. No manual `Register`/`Unregister` calls needed in the reconciler.

```go
// main.go
package main

import (
    "time"

    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/client"
    "sigs.k8s.io/controller-runtime/pkg/handler"
    "sigs.k8s.io/controller-runtime/pkg/source"

    myv1 "example.com/database-operator/api/v1"
    "example.com/database-operator/internal/fetcher"
    "github.com/alperencelik/kube-external-watcher/watcher"
)

func main() {
    mgr, _ := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{})

    dbFetcher := &fetcher.DatabaseFetcher{
        Client:      mgr.GetClient(),
        CloudClient: newCloudClient(),
    }

    // The extractor maps CR fields to ResourceConfig.
    // Readiness and transformation are handled by dbFetcher
    // (IsResourceReadyToWatch and TransformExternalState).
    ew := watcher.NewExternalWatcher(dbFetcher,
        watcher.WithDefaultPollInterval(30*time.Second),
        watcher.WithLogger(ctrl.Log.WithName("external-watcher")),
        watcher.WithAutoRegister(mgr.GetCache(), &myv1.Database{},
            func(obj client.Object) watcher.ResourceConfig {
                cr := obj.(*myv1.Database)
                return watcher.ResourceConfig{
                    PollInterval: cr.Spec.PollInterval.Duration,
                    ResourceKey:     cr.Status.InstanceID,
                }
            },
        ),
    )

    // Add watcher to manager (runs as a Runnable, respects leader election).
    mgr.Add(ew)

    // Wire the watcher's event channel to the controller.
    ctrl.NewControllerManagedBy(mgr).
        For(&myv1.Database{}).
        WatchesRawSource(
            source.Channel(ew.EventChannel(), &handler.EnqueueRequestForObject{}),
        ).
        Complete(&DatabaseReconciler{Client: mgr.GetClient()})

    mgr.Start(ctrl.SetupSignalHandler())
}
```

### Option B: Manual register/unregister

For fine-grained control, inject the watcher into the reconciler and call `Register`/`Unregister` explicitly. `IsResourceReadyToWatch` is not called in this path — you control registration yourself.

```go
// main.go (setup)
ew := watcher.NewExternalWatcher(dbFetcher,
    watcher.WithDefaultPollInterval(30*time.Second),
    watcher.WithLogger(ctrl.Log.WithName("external-watcher")),
)
mgr.Add(ew)

ctrl.NewControllerManagedBy(mgr).
    For(&myv1.Database{}).
    WatchesRawSource(
        source.Channel(ew.EventChannel(), &handler.EnqueueRequestForObject{}),
    ).
    Complete(&DatabaseReconciler{
        Client:  mgr.GetClient(),
        Watcher: ew,
    })
```

```go
// internal/controller/database_reconciler.go
type DatabaseReconciler struct {
    client.Client
    Watcher watcher.WatcherManager
}

func (r *DatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var db myv1.Database
    if err := r.Get(ctx, req.NamespacedName, &db); err != nil {
        if apierrors.IsNotFound(err) {
            r.Watcher.Unregister(req.NamespacedName)
            return ctrl.Result{}, nil
        }
        return ctrl.Result{}, err
    }

    // Only register once provisioned.
    if db.Status.InstanceID != "" {
        r.Watcher.Register(req.NamespacedName, watcher.ResourceConfig{
            PollInterval: db.Spec.PollInterval.Duration,
            ResourceKey:     db.Status.InstanceID,
        })
    }

    // ... reconciliation logic (create/update cloud resource, update status) ...

    return ctrl.Result{}, nil
}
```

## 4. The reconciler (auto-register version)

With auto-register, the reconciler does not need to know about the watcher at all. It only handles the drift — the watcher triggers reconciliation when cloud state diverges from the desired state.

```go
// internal/controller/database_reconciler.go
type DatabaseReconciler struct {
    client.Client
}

func (r *DatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var db myv1.Database
    if err := r.Get(ctx, req.NamespacedName, &db); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // This runs on:
    //   1. CR create/update (standard controller-runtime watch)
    //   2. External drift detected (watcher's GenericEvent)
    //
    // In both cases, fetch the current cloud state and reconcile.
    if db.Status.InstanceID == "" {
        // Provision the cloud resource...
        return ctrl.Result{}, nil
    }

    // Compare and fix drift...
    return ctrl.Result{}, nil
}
```

## 5. How it works end-to-end

```

┌─────────────┐     informer event      ┌───────────────────┐
│  Cache      │ ───────────────────────>│  ExternalWatcher  │
│  (informer) │  Add/Update/Delete      │                   │
└─────────────┘                         │  IsReadyToWatch?  │
                                        │  ──> yes: Register│
                                        │  ──> no:  Skip    │
                                        └────────┬──────────┘
                                                 │
                                    one goroutine per resource
                                                 │
                                         ┌────────▼──────────┐
                                         │ resourceWatcher   │
                                         │                   │
                                         │ poll loop:        │
                                         │  GetDesiredState()│
                                         │  FetchExternal()  │
                                         │  Transform()      │
                                         │  HasDrifted()?    │
                                         │  ──> yes: event   │
                                         └────────┬──────────┘
                                                  │
                                          GenericEvent
                                                  │
                                         ┌────────▼─────────┐
                                         │  Controller      │
                                         │  Reconcile()     │
                                         └──────────────────┘
```

## 5. Transform and compare

The poll loop has three distinct stages after fetching:

1. **Fetch** — `GetDesiredState` and `FetchExternalResource` retrieve raw data from both sides.
2. **Transform** — `TransformExternalState` (on the fetcher) normalizes the raw cloud response into the same shape as the desired state.
3. **Compare** — `StateComparator.HasDrifted` compares two same-shaped values with user-provided `cmp.Options`.

The fetcher owns the transform because it already knows the cloud API response shape and the desired state shape. See the `TransformExternalState` method in section 2.

### Comparator with `cmp.Options`

The `DeepEqualComparator` accepts `cmp.Options` for fine-grained control. Since both inputs are already the same shape (transform ran first), you can use options like `cmpopts.IgnoreFields` and `cmpopts.SortSlices`:

```go
// Ignore InstanceClass after transformation — only care about engine drift.
comparator := watcher.NewDeepEqualComparator(
    cmpopts.IgnoreFields(DatabaseState{}, "InstanceClass"),
)

// Or sort slices so tag order doesn't matter.
comparator := watcher.NewDeepEqualComparator(
    cmpopts.SortSlices(func(a, b string) bool { return a < b }),
)
```

### Custom comparator

For full control, implement the `StateComparator` interface directly. Since the fetcher's `TransformExternalState` runs before comparison, both inputs are already in the same shape:

```go
type EngineOnlyComparator struct{}

func (c *EngineOnlyComparator) HasDrifted(desired, actual any) (bool, error) {
    d := desired.(DatabaseState)
    a := actual.(DatabaseState)
    return d.Engine != a.Engine, nil
}
```

Pass it via option:

```go
watcher.NewExternalWatcher(dbFetcher,
    watcher.WithComparator(&EngineOnlyComparator{}),
    // ...
)
```
