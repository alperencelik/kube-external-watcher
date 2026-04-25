# kube-external-watcher

A Go library for Kubernetes operators that manage external resources (cloud APIs, hypervisors, etc.) — replaces `reconcile.RequeueAfter` polling with drift-triggered reconciliation.

For the motivation and design rationale, see the write-up: [**The missing piece of Kubernetes operators**](https://100vms.com/blog/missing-piece-of-kubernetes-operators/).

## TL;DR

Operators managing external resources can't rely on Kubernetes events alone — out-of-band changes (console edits, other controllers, drift) go undetected. The usual workaround is `reconcile.RequeueAfter` on a fixed cadence, which reconciles whether or not anything changed.

`kube-external-watcher` runs alongside your controllers, polls the external API, and triggers reconciliation **only when drift is detected**:

1. Runs as a `manager.Runnable` (leader-elected, one replica polls).
2. Per-resource goroutines.
3. You implement `ResourceStateFetcher` + optional `StateComparator`.
4. Drift → `GenericEvent` → reconcile. No drift → nothing.

## Getting started

- [**Example implementation**](docs/example-implementation.md) — end-to-end integration pattern.
- [**Architecture**](docs/architecture.md) — interfaces, lifecycle, auto-register.

## Adopters

Projects using `kube-external-watcher` in the wild:

- [**kubemox**](https://github.com/alperencelik/kubemox) — a Kubernetes operator for Proxmox VE that helps you to create Proxmox resources with Custom Resources and detects out-of-band changes via this library.

Using it in your project? Open a PR to add yourself here.
