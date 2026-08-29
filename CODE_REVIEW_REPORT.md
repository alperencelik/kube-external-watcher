# Code Review Report

- **Date:** 2026-07-24
- **Scope:** Entire codebase — all Go source in `watcher/` and `mock/` (~3.7k lines, 17 files)
- **Method:** Multi-agent review at `xhigh` effort — 6 finder agents (5 correctness angles + 1 cleanup/simplification angle) produced 76 candidate findings; every distinct location was independently verified by a dedicated verifier agent (54 verifier agents total). 75 candidates were confirmed and collapsed into 15 distinct defects below; 1 was refuted. Lower-severity metric/log/doc/test-hygiene notes were omitted under the report cap.

## Summary

The most severe issue is a guaranteed process crash in the default comparator (go-cmp panics on unexported fields inside a goroutine with no recover). The largest cluster is lifecycle/race bugs in the auto-register readiness-retry machinery: several interleavings can leave orphaned watchers polling for deleted CRs, or silently stop monitoring a live one. The rest are startup/shutdown state-machine gaps, a hot-loop config edge case, unbounded Prometheus label cardinality, and a self-deadlock hazard in the public test mock.

Findings are ranked most-severe first.

## Resolution status (2026-07-24)

All 15 findings were addressed. 14 were fixed in code; finding 14 was mitigated by documentation (a full fix would require making informer event handling asynchronous, breaking the documented synchronous-registration contract and the test suite). Regression tests were added for findings 1, 2, 5, 7, 8, 9, 10, and 15. The full suite passes under `go test -race` (3× iterations, no flakiness).

| # | Finding | Status |
|---|---------|--------|
| 1 | Comparator panics on unexported fields | Fixed — recover in `HasDrifted`/`Diff`, plus a panic guard around each poll |
| 2 | Re-register drops a changed ResourceKey | Fixed — `doRegister` now updates the resource key on the existing watcher |
| 3 | Readiness-retry TOCTOU registers a deleted resource | Fixed — register under `ar.mu` with an ownership re-check |
| 4 | Retry defer clobbers a newer retry's map entry | Fixed — entries tracked by identity; defer deletes only its own |
| 5 | Not-ready Update doesn't schedule a retry | Fixed — retry scheduled on both not-ready branches |
| 6 | Pending retry keeps a stale object snapshot | Fixed — retry entry's object is refreshed on new events |
| 7 | Registration after shutdown creates a zombie | Fixed — `stopped` flag makes post-shutdown `doRegister` a no-op |
| 8 | Failed Start leaves the watcher wedged | Fixed — `Start` rolls back state on setup failure |
| 9 | Delete tombstone without object is dropped | Fixed — fall back to the tombstone key string |
| 10 | Zero default poll interval hot-loops | Fixed — option rejects ≤0; `doRegister` clamps as a backstop |
| 11 | Per-resource metric labels never deleted | Fixed — `DeletePartialMatch` on unregister and shutdown |
| 12 | Initial poll skips jitter | Fixed — jittered initial delay before the first poll |
| 13 | `stop()` doesn't wait for the in-flight poll | Fixed — `stop` blocks on a done channel (outside the manager lock) |
| 14 | Synchronous readiness check blocks the informer | Mitigated via docs — interface now warns it must be fast/cache-backed |
| 15 | Mock holds RLock during the transform callback | Fixed — callback invoked without the lock held |

---

## 1. Default comparator panics on unexported fields, crashing the process

- **File:** `watcher/comparator.go:27` (also `watcher/resource_watcher.go:142`, `:175`)
- **Category:** correctness · **Verdict:** CONFIRMED

`DeepEqualComparator.HasDrifted` uses `cmp.Equal`, which panics on values containing unexported struct fields, and the panic happens inside the bare poll goroutine (`resource_watcher.go` `run`) that has no `recover` — crashing the whole process.

**Failure scenario:** A user's `GetDesiredState`/`TransformExternalState` returns a struct with an unexported field (very easy: any third-party SDK type without an `Equal` method). On the first poll, `cmp.Equal` panics with "cannot handle unexported field"; the panic propagates out of the resourceWatcher goroutine and terminates the entire controller-manager, putting the operator pod into a crash loop. The `(bool, error)` signature suggests errors are handled, but this failure mode bypasses the error return entirely.

## 2. Re-registering silently drops a changed ResourceKey

- **File:** `watcher/watcher.go:160`
- **Category:** correctness · **Verdict:** CONFIRMED

Re-registering an existing resource updates only the poll interval and silently drops a changed `ResourceConfig.ResourceKey`, despite `Register`'s doc claiming "its configuration is updated".

**Failure scenario:** An external resource is recreated with a new ID and the CR is updated; the auto-register Update handler (`auto_register.go:147`) or a manual `Register` calls `doRegister` with the new ResourceKey, but `doRegister` hits the `existing` branch and only calls `updatePollInterval`. The watcher keeps calling `FetchExternalResource` with the old ID forever — fetch errors or drift against a deleted resource are reported while real drift on the new resource is never detected, until the operator process restarts.

## 3. TOCTOU race: readiness retry registers a just-deleted resource

- **File:** `watcher/auto_register.go:226` (check at `:224`)
- **Category:** correctness · **Verdict:** CONFIRMED

Between the `IsResourceReadyToWatch` check and `doRegister`, a Delete event can cancel the retry and unregister, after which `doRegister` registers the deleted resource anyway.

**Failure scenario:** CR is deleted at the moment its readiness retry finally succeeds: `DeleteFunc` runs `cancelReadinessRetry` + `doUnregister`, but the retry goroutine has already passed the select on `retryCtx.Done()` and the readiness check, so it calls `doRegister` afterward. A watcher goroutine for a deleted CR runs indefinitely, polling the external API and enqueuing `reconcile.Request`s for a nonexistent object; nothing ever unregisters it because no further informer events will arrive for that key.

## 4. Retry goroutine's deferred delete clobbers a newer retry's map entry

- **File:** `watcher/auto_register.go:199`
- **Category:** correctness · **Verdict:** CONFIRMED

The retry goroutine's deferred `delete(ar.retries, key)` deletes whatever entry currently occupies the key, which can be a newer retry started after this one was cancelled, leaving that newer retry untracked and uncancelable.

**Failure scenario:** Delete event cancels retry #1 (removes it from the map); an Add event immediately re-creates retry #2 for the same key before retry #1's goroutine finishes; retry #1's defer then deletes retry #2's cancel func from the map. A subsequent Delete event's `cancelReadinessRetry` finds no entry, so retry #2 keeps running and, when the fetcher reports ready, registers a watcher for the already-deleted resource (same leak as the TOCTOU case, via a different path).

## 5. Not-ready Update unregisters without scheduling a retry

- **File:** `watcher/auto_register.go:137`
- **Category:** correctness · **Verdict:** CONFIRMED

`UpdateFunc` unregisters a resource that became not-ready but does not start a readiness retry, so the resource is only ever re-registered if another informer event happens to arrive.

**Failure scenario:** A registered resource's external dependency blips exactly when an informer Update fires: `IsResourceReadyToWatch` returns false, the resource is unregistered, and no retry goroutine is started (the retry branch is only taken when `!IsRegistered`). Readiness is external state, so it can recover without any Kubernetes object change — with no further Update events (default cache resync is ~10h), the resource silently stops being drift-monitored for hours or forever while everything looks healthy.

## 6. Pending readiness retry keeps a stale object snapshot

- **File:** `watcher/auto_register.go:181` (snapshot at `:191`, use at `:225`)
- **Category:** correctness · **Verdict:** CONFIRMED

`startReadinessRetry` returns early when a retry is already pending, discarding the newer object, so the eventual registration uses the object snapshot from the first not-ready event and loses CR spec changes made during the retry window.

**Failure scenario:** A CR is created pointing at the wrong external ID (not ready), a retry starts; the user fixes the spec while the resource is still not ready — `UpdateFunc`'s not-ready/not-registered branch calls `startReadinessRetry`, which returns at the `exists` check without replacing `objCopy`. When readiness arrives, the extractor runs on the stale first-event object and registers with the old wrong ResourceKey; because `doRegister` also ignores ResourceKey on re-register (finding 2), even the next ready Update event cannot correct it.

## 7. Registration after shutdown creates a zombie watcher entry

- **File:** `watcher/watcher.go:126` (also `:173`, `resource_watcher.go:123`)
- **Category:** correctness · **Verdict:** CONFIRMED

`shutdownOnContextDone` clears the watchers map and resets the gauge but leaves `started=true` with the cancelled ctx, so a late `doRegister` creates a zombie "registered" entry, runs one poll on a cancelled context, and permanently skews the gauge.

**Failure scenario:** During manager shutdown, a straggling informer Add/Update event (or a reconciler's `Register`) runs `doRegister` after `shutdownOnContextDone`: the watcher is inserted into the map, `incRegisteredResources` fires after `resetRegisteredResources` set the gauge to 0, and `rw.start` launches `run()` with the cancelled `w.ctx` — `run` performs its unconditional initial poll (hitting the external API and possibly Add-ing to the shut-down workqueue) before exiting. `IsRegistered` then reports true and `kube_external_watcher_registered_resources` reports >0 for a resource nobody is polling.

## 8. Failed Start leaves the watcher permanently wedged

- **File:** `watcher/watcher.go:85` (also `:92`)
- **Category:** correctness · **Verdict:** CONFIRMED

`Start` sets `started=true` before `setupAutoRegister` can fail; on informer error the flag is never rolled back and the shutdown goroutine is never launched, so the watcher is permanently wedged and any retry of `Start` returns the misleading "Start called more than once" error instead of the real cause.

**Failure scenario:** `cache.GetInformer` fails transiently (e.g. cache not started yet, or an uncached type). `Start` returns the wrapped error, but `started` is left true, `w.ctx`/`w.queue` hold stale values, and `shutdownOnContextDone` was never spawned. Any code that retries wiring the source (or a test harness that re-invokes `Start`) gets "external watcher: Start called more than once", masking the actual informer failure; pre-registered watchers can never be started or cleaned up for the life of the process.

## 9. Delete tombstone without a client.Object is silently dropped

- **File:** `watcher/auto_register.go:154`
- **Category:** cleanup · **Verdict:** CONFIRMED

`DeleteFunc` silently drops the event when a `DeletedFinalStateUnknown` tombstone's `Obj` is not a `client.Object` (nil or `cache.ExplicitKey` case), never unregistering the resource; the tombstone's `Key` string is available but unused as a fallback.

**Failure scenario:** After an informer relist/disconnect produces a tombstone without a usable object, the deleted CR's watcher is never unregistered: it polls the external API forever, logs errors each cycle, keeps enqueueing reconciles for a nonexistent resource, and the `registered_resources` gauge stays inflated until restart.

## 10. Zero default poll interval spins a hot poll loop

- **File:** `watcher/options.go:29`
- **Category:** correctness · **Verdict:** CONFIRMED

`WithDefaultPollInterval` accepts zero or negative durations without validation, and `doRegister`'s `config.PollInterval > 0` guard falls back to this unvalidated default, producing a hot poll loop (`time.After(<=0)` fires immediately) — unlike `WithPollJitter`, which clamps invalid input.

**Failure scenario:** A user wires the interval from an unset config field: `NewExternalWatcher(f, WithDefaultPollInterval(cfg.Interval))` with `cfg.Interval==0`. Every resource registered without a per-resource `PollInterval` gets interval 0; `run()` spins in a tight loop calling `GetDesiredState`/`FetchExternalResource` continuously, pegging a CPU core and hammering the external API with unbounded request volume until the process is killed or rate-limited.

## 11. Per-resource metric labels are never deleted (unbounded cardinality)

- **File:** `watcher/watcher.go:198` (also `watcher/metrics.go:39`)
- **Category:** correctness · **Verdict:** CONFIRMED

`doUnregister` (and shutdown) never delete the per-resource Prometheus label values (namespace/name on `pollTotal`, `fetchExternalDuration`, `fetchExternalErrors`, `driftDetectedTotal`), so metric cardinality grows unboundedly with resource churn.

**Failure scenario:** A long-running controller managing short-lived CRs (created/deleted continuously) accumulates a label set per historical resource in the shared controller-runtime registry; memory usage and `/metrics` scrape size grow without bound, eventually degrading or OOM-killing the controller pod and overloading Prometheus, since only `resetRegisteredResources` (the gauge) is cleared on shutdown and nothing calls `DeleteLabelValues` on unregister.

## 12. Initial poll skips jitter, causing a synchronized startup burst

- **File:** `watcher/resource_watcher.go:123`
- **Category:** correctness · **Verdict:** CONFIRMED

`run()` performs the initial poll immediately with no jitter — jitter is only applied to sleeps between subsequent polls — contradicting the `DefaultPollJitter` contract in `options.go` (lines 17–22) that watchers registered together during startup cache sync should not hit the external API in synchronized bursts.

**Failure scenario:** A controller with auto-register restarts against a namespace with hundreds of CRs. The informer's initial list delivers Add events back-to-back, each `doRegister` immediately spawns a goroutine whose first `FetchExternalResource` fires at once: the external API receives a synchronized burst of N calls, gets rate-limited (429s), the first poll cycle fails for most resources (error logs plus `fetch_external_errors` and error-result poll metrics spike), and any drift present at startup is not detected until the next interval.

## 13. stop() doesn't wait for the in-flight poll to finish

- **File:** `watcher/resource_watcher.go:81`
- **Category:** correctness · **Verdict:** CONFIRMED

`resourceWatcher.stop` cancels the context but does not wait for the goroutine, so an in-flight poll continues after `Unregister` returns.

**Failure scenario:** A reconciler handling deletion calls `Unregister` and then removes the CR's finalizer. A poll already past its ctx check continues: it calls `UpdateResourceStatus` on the now-deleted CR (status-update error / conflict noise) and enqueues a `reconcile.Request` for the deleted resource via `rw.queue.Add`, triggering a spurious reconcile of a nonexistent object after the controller believed the watch was fully stopped.

## 14. Synchronous readiness check blocks informer event delivery

- **File:** `watcher/auto_register.go:115`
- **Category:** cleanup · **Verdict:** CONFIRMED

`IsResourceReadyToWatch` is called synchronously inside the informer Add/Update event handlers; a slow implementation (one that queries the external API) blocks this handler's event delivery and buffers events unboundedly in client-go's ring buffer.

**Failure scenario:** With a readiness check that does a 5s external API call and hundreds of CRs listed at startup, event processing serializes: Delete events queue for minutes behind Add readiness checks, so deleted resources keep polling meanwhile, and the handler's pending-notification buffer grows without bound (memory).

## 15. Mock holds RLock while calling transformFn — Set*/Reset self-deadlocks

- **File:** `mock/fake_state_fetcher.go:121` (also `:124`)
- **Category:** correctness · **Verdict:** CONFIRMED

`TransformExternalState` holds `f.mu.RLock` while invoking the user-supplied `transformFn`; if the callback calls any `Set*`/`Reset` method on the same fake (which take `f.mu.Lock`), the goroutine self-deadlocks because a `sync.RWMutex` writer blocks behind the caller's own read lock.

**Failure scenario:** A test uses `SetTransformFn` with a callback that, e.g., records progress by calling `f.SetResourceState` (or `f.Reset`) to reprogram the fake mid-poll. The poll goroutine deadlocks inside the mock — RLock held, Lock waiting on it — the resource watcher stops polling entirely, and the test hangs until the `go test` 10-minute timeout kills it with an unhelpful goroutine dump instead of a failure message.

---

## Refuted during verification

- `watcher/resource_watcher_test.go:117` — claim that the `wait.Jitter` branch in `run()` is never exercised by any test. Refuted by the verifier; jitter coverage exists beyond option plumbing.

## Suggested fix order

1. Comparator panic (finding 1) — add panic recovery around comparison (or the whole poll) and surface it via the error return.
2. Auto-register race family (findings 3, 4, 5, 6) — shared fix shape: re-check cancellation under the lock before registering, track retries by generation/identity so a defer only deletes its own entry, refresh the object snapshot on new events, and schedule a retry on the not-ready Update path.
3. Start/shutdown state machine (findings 7, 8) — roll back `started` on setup failure; gate `doRegister` on a live context.
4. ResourceKey update on re-register (finding 2), then the remainder (9–15).
