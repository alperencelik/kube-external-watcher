# kube-external-watcher

kube-external-watcher is a Go library for watching external resources in Kubernetes operators. It decouples the reconciliation loop from external state observation by introducing an external watcher that periodically polls the external resource and triggers reconciliation only when a drift is detected.

## The problem

Kubernetes operators that manages the external/cloud resources can't guarantee the external state will only change in response to Kubernetes events due to other possible actors. For example, an RDS instance can be modified by a user directly in AWS Console, or by another controller/operator. If the operator only reconciles on Kubernetes events (e.g., CR create/update/delete), it won't detect these out-of-band changes and the Kubernetes state will drift from the actual external state. 

To mitigate this, the operators usually call `reconcile.RequeueAfter{someInterval}` to requeue reconciliation on a fixed cadence (e.g. every 60 seconds), regardless of whether the external state actually changed. This leads to unnecessary reconciliations, increased load on the API server, and inefficient resource usage. Ideally, we want to trigger reconciliation only when the external state actually changes (drift detection), rather than on a fixed schedule.

## The proposed solution

I think every operator that manages external resources can benefit from an external watcher that bridges the external state and the Kubernetes state. The watcher would periodically poll the external resource, compare it with the desired state, and trigger reconciliation only when a drift is detected. This decouples the observation of external state from the reconciliation logic, leading to a more efficient and event-driven architecture.

I was using a similar pattern (spawning external watchers and triggering reconciliation in a event of drift) in a few of my operators(e.g. https://github.com/alperencelik/kubemox), and I think it's a common enough problem that it would be valuable to build a reusable library for it. This project is the result of that effort which tries to become a reference implementation of an external watcher that can be easily integrated into any Kubernetes controller/operator that needs to maintain external resources.

External watcher will definitely needs to call those cloud APIs periodically to check for drift, but the key is that it only triggers reconciliation when a drift is detected, rather than on a fixed schedule. This way, we can reduce unnecessary reconciliations and only react to actual changes in the external state and also keep the controller queue much cleaner :)

## How it works

kube-external-watcher provides an `ExternalWatcher` that can be added to the controller manager. It implements `manager.Runnable` and `manager.LeaderElectionRunnable`, so it runs alongside the controllers and only the leader replica polls the external APIs. The watcher maintains a map of registered resources, each with its own poll interval. For each registered resource, it spawns a goroutine that periodically calls `GetDesiredState`, `FetchExternalResource`, `TransformExternalState`, and `HasDrifted` to check for drift. If drift is detected, it sends a `GenericEvent` to trigger reconciliation for that resource. To put it simply;

1. Runs alongside controllers inside the controller-manager.
2. Checks if the resource is ready to be watched (e.g., has the necessary status fields) before registering it for polling.
3. Periodically polls the external state of a resource.
4. Compares external state against the desired state. (You define what "drift" means by implementing the `StateComparator` interface)
5. Triggers reconciliation by sending an event that `ctrl.manager` listens to, but only when a drift is detected.
6. The controller reconciles the resource, which may update the external state, and the cycle continues.

## Getting Started

For a detailed example implementation and integration pattern, please refer to the `docs/example-implementation.md` file.

## Architecture

For architectural overview, please refer to the `docs/architecture.md` file.
