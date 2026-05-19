# HA Metrics Aggregation: Pluggable DataStore Backend Architecture

Author(s): @noalimoy, @nirrozenbaum

## Proposal Status

***Proposed***

## Summary

The Inference Payload Processor (IPP) uses a **DataStore** to hold runtime metrics (e.g., in-flight request counts per model). Scorers read these metrics to make model selection decisions. With a single replica the in-memory DataStore is sufficient. When IPP scales to **multiple replicas**, each replica only sees its own traffic — Scorers make decisions based on incomplete data:

```
                 ┌──────────┐      ┌──────────┐      ┌──────────┐
requests ──────► │ Replica 1│      │ Replica 2│      │ Replica 3│
                 │in-flight:2│      │in-flight:3│      │in-flight:0│
                 └──────────┘      └──────────┘      └──────────┘
                  Scorer sees 2     Scorer sees 3     Scorer sees 0
                  (actual: 5)       (actual: 5)       (actual: 5)
```

**Goal:** every Scorer on every replica should see the same aggregated total, without adding network calls to the request path.

This proposal defines a **pluggable DataStore backend architecture** that solves this problem by synchronizing metrics across replicas through an external store, while keeping all request-path operations strictly in-memory. For background see [Issue #79](https://github.com/llm-d/llm-d-inference-payload-processor/issues/79) and [Issue #85](https://github.com/llm-d/llm-d-inference-payload-processor/issues/85).

## Design Principles

- **All hot-path operations interact with local in-memory components.** `Put()` and `Get()` on `AttributeMap` must never block on network I/O.
- **Single replica mode should not be affected by HA considerations.**
- **Collective visibility** — every replica must see the aggregated state across all replicas.
- **Easy to scale** — adding replicas should be straightforward, not require complex reconfiguration.
- **Resilience** — if a replica crashes, its contribution to the aggregate is excluded automatically via key expiry. No manual intervention required.
- **Decoupled from request processing** — metrics collection and aggregation run as a background pipeline, separate from the per-request plugin chain.
- **All IPP metrics are transient and time-scoped** (e.g., relevant for the last 10m, 30m). They do not require persistent storage, only shared storage.
- **Backend-agnostic** — the core architecture is independent of the external store. Redis is the first implementation; the same pattern should apply to any key-value store. Extractors and Scorers use the same `DataStore` / `AttributeMap` API regardless of the backend.

## Existing Interfaces

The design builds on interfaces that already exist in the codebase. No changes are required.

```go
type DataStore interface {
    GetOrCreateModel(name string) Model
}

type Model interface {
    GetName() string
    GetAttributes() AttributeMap
}

type AttributeMap interface {
    Put(key string, value Cloneable)
    Get(key string) (Cloneable, bool)
    Delete(key string)
    Keys() []string
    Clone() AttributeMap
}
```

Note that `Put()` and `Get()` have **no** `context.Context` and **no** `error` return. This is by design — it guarantees that any implementation must be synchronous and in-memory. A backend that needs network I/O cannot satisfy this contract on the hot path, which is why the dual-map pattern below is necessary.

## Architecture: The Dual-Map Pattern

Each replica maintains two in-memory maps — one for its own metric values, and one for the aggregated view across all replicas:

```
┌─────────────────────────────────────────────────────────┐
│                     IPP Replica                         │
│                                                         │
│   Extractor ──Put()──► LOCAL DATA                       │
│                        (this replica's own metrics)     │
│                              │                          │
│                              ▼                          │
│                        ┌───────────┐                    │
│                        │ heartbeat │ background         │
│                        │ goroutine │ goroutine          │
│                        └─────┬─────┘                    │
│                              │ publish                  │
│  ┌───────────────────────────┼──────────────────────┐   │
│  │            External Store (e.g., Redis)           │   │
│  └───────────────────────────┼──────────────────────┘   │
│                              │ read + aggregate         │
│                        ┌─────┴─────┐                    │
│                        │  refresh  │ background         │
│                        │ goroutine │ goroutine          │
│                        └───────────┘                    │
│                              │                          │
│                              ▼                          │
│   Scorer ◄──Get()───── CACHE                            │
│                        (aggregated from ALL replicas)   │
│                                                         │
└─────────────────────────────────────────────────────────┘
```

- **Local data** — holds this replica's own metrics. Extractors write here via `Put()`. The heartbeat goroutine reads from here and publishes to the external store.
- **Cache** — holds aggregated metrics from **all** replicas. The refresh goroutine writes here after reading from the external store. Scorers read from here via `Get()`.

Both `Put()` and `Get()` interact only with local in-memory maps — any external backend DataStore implementation routes `Put()` to the local data map and `Get()` to the cache map.

**Note:** because `Put()` and `Get()` route to different maps, calling `Put("key", val)` followed immediately by `Get("key")` will return the previously cached aggregate — not the value just written. This is by design: local data reflects this replica only, while the cache reflects all replicas. The written value appears in the cache after the next heartbeat + refresh cycle (~1-2s by default).

## Background Goroutines

Two background goroutines bridge the local maps and the external store. They run independently of request processing and do not block the hot path.

### Heartbeat (publish)

Runs every `heartbeatInterval` (default: 1s). Iterates all models in the local data map, serializes each metric value, and writes it to the external store as a **per-replica key** with a TTL.

**Key format:** `<replica-id>:<model-name>:<metric-key>`

Each replica writes only its own keys. The replica ID is unique per pod (derived from `POD_NAME` env var in Kubernetes). The TTL ensures that keys from crashed replicas expire automatically (see [Self-Healing](#self-healing)).

### Refresh (aggregate)

Runs every `refreshInterval` (default: 1s). Reads all per-replica keys from the external store, groups them by `<model-name>:<metric-key>`, sums the metric fields across replicas, and writes the aggregated result to the cache map.

After a refresh cycle, every replica's cache contains the same aggregated totals — regardless of which replica handled which request.

**Example:**

```
External store:
  pod-abc:model-a:inflight-requests = {Requests: 2, Tokens: 8000}
  pod-def:model-a:inflight-requests = {Requests: 3, Tokens: 4000}

After refresh, every replica's cache:
  cache["model-a"]["inflight-requests"] = {Requests: 5, Tokens: 12000}
```

## Self-Healing

Every per-replica key is written with a TTL (default: 10s). If a replica crashes or becomes unreachable, it stops publishing heartbeats, its keys expire, and the next refresh cycle automatically excludes its values from the sum. No manual cleanup required.

**TTL sizing:** the `keyTTL` should be at least 5x the `heartbeatInterval` to tolerate brief network hiccups without premature key expiry (e.g., heartbeat = 1s, TTL = 10s allows up to ~10 missed heartbeats).

## Failure Modes


| Scenario                       | Behavior                                                                                                                                                                |
| ------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Replica crashes**            | In-flight requests terminate with the pod. Per-replica keys expire via TTL. Next refresh excludes them.                                                                 |
| **Replica restarts**           | New pod starts with empty local data. Within one refresh interval, the cache is populated from the external store.                                                      |
| **External store unavailable** | Local cache retains last known values. Scorers continue with stale data. When the store recovers, goroutines reconnect automatically. Degraded accuracy, not an outage. |
| **Rolling update**             | Pods restart one at a time. New pods read current state within one refresh interval. Old pod keys expire naturally via TTL.                                             |


## Timing Configuration


| Parameter           | Description                                                                | Default |
| ------------------- | -------------------------------------------------------------------------- | ------- |
| `heartbeatInterval` | How often each replica publishes local values to the external store        | 1s      |
| `refreshInterval`   | How often each replica reads and aggregates values from the external store | 1s      |
| `keyTTL`            | TTL for per-replica keys (stale keys auto-expire after this)               | 10s     |


These values control the trade-off between freshness and load on the external store. Lower intervals mean fresher data but more read/write operations.

## References

- [Issue #79 — HA metrics aggregation](https://github.com/llm-d/llm-d-inference-payload-processor/issues/79)
- [Issue #85 — Redis backend requirements](https://github.com/llm-d/llm-d-inference-payload-processor/issues/85)
- [PR #108 — Split DataStore interface and in-memory implementation](https://github.com/llm-d/llm-d-inference-payload-processor/pull/108) (merged)
- [PR #75 — Running requests extractor](https://github.com/llm-d/llm-d-inference-payload-processor/pull/75) (merged)
- [PR #91 — Generic ReadAttributeKey helper](https://github.com/llm-d/llm-d-inference-payload-processor/pull/91) (merged)

