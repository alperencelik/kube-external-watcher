// Package watcher provides a reusable external state watcher for Kubernetes
// controllers. Instead of relying on reconcile.RequeueAfter to periodically
// poll external resources, operators import this package and wire an
// ExternalWatcher into their controller-runtime Manager. The watcher polls
// external state for each registered resource and enqueues reconcile
// requests directly to the controller's workqueue only when drift is
// detected.
//
// Usage:
//
//  1. Implement ResourceStateFetcher to fetch external state for your resources.
//  2. Create an ExternalWatcher via NewExternalWatcher(fetcher, opts...).
//  3. Wire it into your controller via WatchesRawSource(externalWatcher).
//  4. Call Register/Unregister from your reconciler to manage watched resources,
//     or use WithAutoRegister to manage them via informer events.
package watcher
