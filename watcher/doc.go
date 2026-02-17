// Package watcher provides a reusable external state watcher for Kubernetes
// controllers. Instead of relying on reconcile.RequeueAfter to periodically
// poll external resources, operators import this package and wire an
// ExternalWatcher into their controller-runtime Manager. The watcher polls
// external state for each registered resource and triggers reconciliation
// only when drift is detected, using source.Channel as the bridge.
//
// Usage:
//
//  1. Implement ExternalStateFetcher to fetch external state for your resources.
//  2. Create an ExternalWatcher via NewExternalWatcher(fetcher, opts...).
//  3. Add it to your manager: mgr.Add(externalWatcher).
//  4. Wire its EventChannel into your controller via source.Channel.
//  5. Call Register/Unregister from your reconciler to manage watched resources.
package watcher
