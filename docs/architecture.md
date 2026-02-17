# Architecture 

Currently there is only watcher/ package, which contains all core logic. The main components are:

### Core Interfaces (watcher/interfaces.go)

- `ResourceStateFetcher` — User implements `GetDesiredState`, `FetchExternalResource`, `TransformExternalState`, and `IsResourceReadyToWatch` to bridge Kubernetes and external resource state. `TransformExternalState` normalizes the raw external API response into the same shape as the desired state.
- `StateComparator` — `HasDrifted(desired, actual any) (bool, error)`. Receives two same-shaped values (transform is applied upstream by the fetcher for type conversion) and determines if drift occurred. Returns a bool and an error (error = unable to compare, do not trigger reconciliation, but log the error). 
- `WatcherManager` — `Register(key, ResourceConfig)`, `Unregister(key)`, `IsRegistered(key) bool`.
- `ResourceConfig` — Per-resource config. `PollInterval` (zero = global default).

### ExternalWatcher (watcher/watcher.go)

The central orchestrator. Implements:
- `manager.Runnable` — `Start(ctx) error`, blocks until shutdown.
- `manager.LeaderElectionRunnable` — `NeedLeaderElection() = true` (only leader polls).
- `WatcherManager` — register/unregister resources at any time.

PS: Currently the leader election is set to true to avoid duplicate external API calls from non-leader replicas. In the future, I might consider distributing the watcher across replicas for distributing the load. But to avoid complexity around state synchronization, the project will stick to leader election for now.

Lifecycle:

1. User creates via `NewExternalWatcher(fetcher, opts...)`.
2. Added to manager via `mgr.Add(watcher)`.
3. Event channel wired to controller via `source.Channel(watcher.EventChannel(), handler)`.
4. On `Start()`: if `WithAutoRegister` is configured, sets up informer event handler. Starts pre-registered watchers, blocks on ctx.
5. `Register()` can be called before or after Start. Post-start, goroutine spawns immediately.
6. On shutdown: all resource watchers stopped, map cleared.

### Auto-Registration (watcher/auto_register.go)

If users want to automatically register/unregister resources with the help of `informer`, they can use the  `WithAutoRegister(cache, obj, extractorFn)` option when creating the `ExternalWatcher`. This hooks into the controller-runtime cache informer for the specified object type. That hooks into the controller-runtime cache informer for the given object type:

- **Add/Update events**: Calls `fetcher.IsResourceReadyToWatch(ctx, key)` first. If false, the resource is skipped (or unregistered if previously registered). If true, calls `extractorFn(obj)` to get a `ResourceConfig` and registers the resource.
- **Delete events**: Calls `Unregister(key)`. Handles `DeletedFinalStateUnknown` tombstones.

The informer event handler is set up in `Start()` via `cache.GetInformer()`. This runs after the manager starts the cache, so the informer is synced.

### resourceWatcher (watcher/resource_watcher.go)

One goroutine per registered resource:

1. Initial poll on start.
2. Loop: wait `pollInterval` → `GetDesiredState` → `FetchExternalResource` → `TransformExternalState` → `HasDrifted(desired, transformed)` → send `GenericEvent` if drifted.
3. Fetch/transform/compare errors logged, do not trigger reconciliation, loop continues.

### Event Bridge

Uses `source.Channel` with `event.GenericEvent`. The watcher creates a minimal `objectReference` (name + namespace only, satisfies `client.Object`) to identify which resource drifted. `handler.EnqueueRequestForObject` extracts the `NamespacedName` and enqueues a `reconcile.Request`.

### Integration pattern for users

#### Option A: Auto-register (recommended)

Uses `WithAutoRegister` to automatically register/unregister resources via cache informer events. No manual `Register`/`Unregister` calls needed in the reconciler.

```go
// 1. Create manager
mgr, _ := ctrl.NewManager(cfg, ctrl.Options{})

// 2. Create watcher with auto-register
// myFetcher must implement GetDesiredState, FetchExternalResource,
// TransformExternalState, and IsResourceReadyToWatch.
ew := watcher.NewExternalWatcher(myFetcher,
    watcher.WithDefaultPollInterval(30*time.Second),
    watcher.WithComparator(watcher.NewDeepEqualComparator(
        cmpopts.IgnoreFields(DatabaseState{}, "Tags"),
    )),
    watcher.WithAutoRegister(mgr.GetCache(), &myv1.Database{}, func(obj client.Object) watcher.ResourceConfig {
        cr := obj.(*myv1.Database)
        return watcher.ResourceConfig{
            PollInterval: cr.Spec.PollInterval,
            ResourceKey:     cr.Status.InstanceID,
        }
    }),
)

// 3. Add to manager
mgr.Add(ew)

// 4. Wire to controller — no manual Register/Unregister needed
ctrl.NewControllerManagedBy(mgr).
    For(&myv1.Database{}).
    WatchesRawSource(source.Channel(ew.EventChannel(), &handler.EnqueueRequestForObject{})).
    Complete(myReconciler)
```

#### Option B: Manual register/unregister

For cases where you need fine-grained control over when resources are watched, inject the watcher into the reconciler and call `Register`/`Unregister` explicitly.

```go
// 1. Create watcher
ew := watcher.NewExternalWatcher(myFetcher, watcher.WithDefaultPollInterval(30*time.Second))

// 2. Add to manager
mgr.Add(ew)

// 3. Wire to controller
ctrl.NewControllerManagedBy(mgr).
    For(&MyCR{}).
    WatchesRawSource(source.Channel(ew.EventChannel(), &handler.EnqueueRequestForObject{})).
    Complete(&MyReconciler{watcher: ew})

// 4. In reconciler: register/unregister
func (r *MyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var cr MyCR
    if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
        if apierrors.IsNotFound(err) {
            r.watcher.Unregister(req.NamespacedName)
            return ctrl.Result{}, nil
        }
        return ctrl.Result{}, err
    }
    r.watcher.Register(req.NamespacedName, watcher.ResourceConfig{
        PollInterval: cr.Spec.PollInterval,
        ResourceKey:     cr.Status.ExternalID,
    })
    // ... reconciliation logic ...
}
```