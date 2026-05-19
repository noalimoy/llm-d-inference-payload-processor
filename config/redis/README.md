# Redis Backend for IPP Metrics Aggregation

This guide covers how to deploy, configure, and verify Redis as the external store for IPP cross-replica metrics aggregation.

For the architecture and design rationale, see the [HA Metrics Aggregation design proposal](../../docs/proposals/ha-metrics-aggregation/README.md).
For background on requirements, see [Issue #79](https://github.com/llm-d/llm-d-inference-payload-processor/issues/79) and [Issue #85](https://github.com/llm-d/llm-d-inference-payload-processor/issues/85).

## How It Works

```
Replica 1               Replica 2               Replica 3
Put→local               Put→local               Put→local
(model-a: reqs=2,       (model-a: reqs=3,       (model-a: reqs=0,
 tok=8000)               tok=4000)               tok=0)
    │                       │                       │
    └── heartbeat ──────────┼── heartbeat ──────────┘
                            ▼
                     Redis (user-owned)
          r1:model-a:inflight-requests = {Reqs:2,Tok:8000}   TTL 10s
          r2:model-a:inflight-requests = {Reqs:3,Tok:4000}   TTL 10s
          r3:model-a:inflight-requests = {Reqs:0,Tok:0}      TTL 10s
                            ▲
    ┌── refresh (~1s) ──────┼── refresh (~1s) ──────┐
    ▼                       ▼                       ▼
Get→cache: model-a      Get→cache: model-a      Get→cache: model-a
  reqs=5, tok=12000       reqs=5, tok=12000       reqs=5, tok=12000
Scorer reads             Scorer reads             Scorer reads
  {Reqs:5, Tok:12000}     {Reqs:5, Tok:12000}     {Reqs:5, Tok:12000}
```

Each replica keeps two in-memory maps (local data and cache) and uses two background goroutines (heartbeat and refresh) to sync through Redis. Scorers always read from the local cache — no network call on the request path. For the full architecture, see the [design proposal](../../docs/proposals/ha-metrics-aggregation/README.md).

## Prerequisites

A running Redis instance accessible from the IPP pods. IPP does not deploy or manage Redis — you provide the endpoint (via a Redis Operator, Helm chart, or managed service like ElastiCache).

Redis requirements:

- Redis 6.x or later
- No persistence needed (`--save ""`) — all IPP metrics are transient and reconstructed from live traffic
- No AOF, no RDB
- Single-node Redis is sufficient (if Redis restarts, IPP continues with cached data until keys rebuild)

## Deploy Redis

Choose one of the following options.

### Option 1: Simple Deployment (dev / testing)

```bash
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ipp-redis
  namespace: llm-d
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ipp-redis
  template:
    metadata:
      labels:
        app: ipp-redis
    spec:
      containers:
      - name: redis
        image: redis:7-alpine
        args: ["--save", "", "--maxmemory", "64mb", "--maxmemory-policy", "allkeys-lru"]
        ports:
        - containerPort: 6379
        resources:
          requests:
            cpu: 100m
            memory: 64Mi
          limits:
            cpu: 250m
            memory: 128Mi
---
apiVersion: v1
kind: Service
metadata:
  name: ipp-redis
  namespace: llm-d
spec:
  selector:
    app: ipp-redis
  ports:
  - port: 6379
EOF
```

The Redis endpoint is `ipp-redis.llm-d.svc.cluster.local:6379`.

### Option 2: Redis Operator (production)

If your cluster runs a Redis Operator (e.g., [Spotahome](https://github.com/spotahome/redis-operator),[OpsTree](https://github.com/OT-CONTAINER-KIT/redis-operator)), create a minimal standalone instance:

```yaml
apiVersion: databases.spotahome.com/v1
kind: RedisFailover
metadata:
  name: ipp-redis
  namespace: llm-d
spec:
  sentinel:
    replicas: 3
  redis:
    replicas: 2
    customConfig:
      - "save \"\""
      - "appendonly no"
```

Refer to your operator's documentation for the resulting Service endpoint:

```bash
kubectl get svc -n llm-d | grep redis
```

Use the Service name as the `datastore.redis.endpoint` value (see [Configure IPP to Use Redis](#configure-ipp-to-use-redis) below).

### Option 3: Managed Redis (cloud)

Use your cloud provider's managed Redis service:


| Provider | Service               | Notes                                 |
| -------- | --------------------- | ------------------------------------- |
| AWS      | ElastiCache for Redis | Disable backups, use `cache.t3.micro` |
| GCP      | Memorystore for Redis | Basic tier, no replicas needed        |
| Azure    | Azure Cache for Redis | Basic C0, disable persistence         |


Use the endpoint provided by the managed service (e.g., `my-redis.abc123.cache.amazonaws.com:6379`).

## Configure IPP to Use Redis

> **Note:** The configuration flags and Helm values below are part of the Redis backend implementation (not yet merged). They document the target interface.

### Helm

Set the Redis backend in your `values.yaml` (add the `datastore` section alongside existing fields):

```yaml
payloadProcessor:
  replicas: 3                  # scale to multiple replicas
  datastore:                   # NEW — add this section
    backend: redis
    redis:
      endpoint: "ipp-redis.llm-d.svc.cluster.local:6379"
      # password: ""                  # optional, omit if Redis has no auth
      # heartbeatInterval: "1s"       # how often to publish local values
      # refreshInterval: "1s"         # how often to read aggregated values
      # keyTTL: "10s"                 # per-replica key expiry
```

Or via `--set`:

```bash
helm install payload-processor ./config/charts/payload-processor \
    --set payloadProcessor.replicas=3 \
    --set payloadProcessor.datastore.backend=redis \
    --set payloadProcessor.datastore.redis.endpoint=ipp-redis.llm-d.svc.cluster.local:6379
```

### CLI Flags

When running IPP directly (outside Helm):

```bash
./payload-processor \
    --datastore-backend=redis \
    --redis-endpoint=ipp-redis.llm-d.svc.cluster.local:6379 \
    --redis-heartbeat-interval=1s \
    --redis-refresh-interval=1s \
    --redis-key-ttl=10s
```

## Configuration


| Parameter                           | Description                                                       | Default                         |
| ----------------------------------- | ----------------------------------------------------------------- | ------------------------------- |
| `datastore.backend`                 | Storage backend: `inmemory` or `redis`                            | `inmemory`                      |
| `datastore.redis.endpoint`          | Redis address (`host:port`)                                       | — (required when backend=redis) |
| `datastore.redis.password`          | Redis AUTH password                                               | `""` (no auth)                  |
| `datastore.redis.heartbeatInterval` | How often each replica publishes its local values to Redis        | `1s`                            |
| `datastore.redis.refreshInterval`   | How often each replica reads and aggregates all values from Redis | `1s`                            |
| `datastore.redis.keyTTL`            | TTL for per-replica keys (stale keys auto-expire after this)      | `10s`                           |


## Redis Key Schema and Data Flow

Extractors write to the local data map via `Put()`. The heartbeat goroutine reads from that map and publishes per-replica keys to Redis:

```
Key format:  <replica-id>:<model-name>:<metric-key> = <serialized value>

Example (2 replicas, 2 models):
  ipp-pod-abc:model-a:inflight-requests = {"Requests":2,"Tokens":8000}      TTL 10s
  ipp-pod-abc:model-b:inflight-requests = {"Requests":4,"Tokens":16000}     TTL 10s
  ipp-pod-def:model-a:inflight-requests = {"Requests":3,"Tokens":4000}      TTL 10s
  ipp-pod-def:model-b:inflight-requests = {"Requests":6,"Tokens":22000}     TTL 10s
```

The refresh goroutine scans all keys via `SCAN`, groups by `<model>:<metric>`, and sums each field across replicas. After refresh, every replica's cache holds the same aggregated result:

```
cache["model-a"]["inflight-requests"] = {Requests: 5, Tokens: 12000}    // 2+3, 8000+4000
cache["model-b"]["inflight-requests"] = {Requests: 10, Tokens: 38000}   // 4+6, 16000+22000
```

Scorers read from the cache via `Get()` — no network call.

```
Put()  → writes to local data  → heartbeat publishes to Redis
Get()  → reads from local cache ← refresh aggregates from Redis
```

## Verify

After deploying Redis and configuring IPP:

```bash
# 1. Check Redis is reachable
kubectl exec -it deploy/ipp-redis -n llm-d -- \
    redis-cli PING
# Expected: PONG

# 2. Check keys are being written (after sending a few requests)
kubectl exec -it deploy/ipp-redis -n llm-d -- \
    redis-cli KEYS "*"
# Expected: per-replica keys like ipp-pod-abc:model-a:inflight-requests

# 3. Check TTL is set
kubectl exec -it deploy/ipp-redis -n llm-d -- \
    redis-cli TTL "ipp-pod-abc:model-a:inflight-requests"
# Expected: a value between 1 and 10
```

## Failure Modes


| Scenario              | Behavior                                                                                                                                                                       |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **Replica crashes**   | In-flight requests terminate with the pod. Per-replica keys expire via TTL (~10s). Next refresh excludes them.                                                                 |
| **Replica restarts**  | New pod starts with an empty local store. Within ~1s the refresh goroutine reads aggregated state from Redis.                                                                  |
| **Redis unavailable** | Local cache retains last known aggregated values. Scorers continue with stale data. When Redis recovers, goroutines reconnect automatically. Degraded accuracy, not an outage. |
| **Rolling update**    | Pods restart one at a time. New pods read current state within ~1s. Old pod keys expire naturally via TTL.                                                                     |


## Notes

- All IPP metrics stored in Redis are transient. Redis data loss (restart, eviction) causes temporary accuracy degradation, not an outage — metrics rebuild from live traffic within seconds.
- The `keyTTL` should be at least 5× the `heartbeatInterval` to tolerate brief network hiccups without premature key expiry (e.g., heartbeat=1s → TTL≥5s, default 10s).
- The replica ID used in key names is derived from the Kubernetes pod name (`POD_NAME` env var). Each pod produces a unique set of keys.

