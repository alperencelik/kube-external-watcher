# kube-external-watcher

A Go library for Kubernetes operators that manage external resources (cloud APIs, hypervisors, etc.) — replaces `reconcile.RequeueAfter` polling with drift-triggered reconciliation.

For the motivation and design rationale, see the write-up: [**The missing piece of Kubernetes operators**](https://100vms.com/blog/missing-piece-of-kubernetes-operators/).

## TL;DR

Operators managing external resources can't rely on Kubernetes events alone — out-of-band changes (console edits, other controllers, drift) go undetected. The usual workaround is `reconcile.RequeueAfter` on a fixed cadence, which reconciles whether or not anything changed.

`kube-external-watcher` runs alongside your controllers, polls the external API, and triggers reconciliation **only when drift is detected**:

1. Implements `source.Source` — wire it into your controller with `WatchesRawSource(ew)`.
2. Per-resource goroutines.
3. You implement `ResourceStateFetcher` + optional `StateComparator`(defaults to deepEqual comparison).
4. Drift → `reconcile.Request` on the controller's workqueue → reconcile. No drift → nothing.

## Getting started

- [**Example implementation**](docs/example-implementation.md) — end-to-end integration pattern.
- [**Architecture**](docs/architecture.md) — interfaces, lifecycle, auto-register.

## Adopters

Projects using `kube-external-watcher` in the wild:

- [**kubemox**](https://github.com/alperencelik/kubemox) — a Kubernetes operator for Proxmox VE that helps you to manage Proxmox resources (VMs, containers, storage, networks) declaratively.
- [**talos-operator**](https://github.com/alperencelik/talos-operator) — a Kubernetes operator for managing Talos Linux clusters.
- [**oxide-operator**](https://github.com/alperencelik/oxide-operator) - a Kubernetes operator for managing Oxide racks.

Using it in your project? Open a PR to add yourself here.
