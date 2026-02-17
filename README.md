# Janus

A Kubernetes operator that executes atomic multi-resource changes using the
[Saga pattern](https://dl.acm.org/doi/10.1145/38714.38742) with
Lease-based advisory locking.

## Goals

Janus provides atomic multi-resource mutations for Kubernetes:

- **Atomic multi-resource mutations.** A `Transaction` groups an ordered list
  of create, update, patch, and delete operations. Either all succeed or all
  committed changes are reverted.
- **Crash-resilient progress.** The controller processes one item per reconcile
  cycle, persisting status to the API server after each step. A controller
  restart resumes from the last recorded checkpoint.
- **Advisory resource locking.** Lease objects prevent concurrent transactions
  from modifying the same resource. Locks are renewed before each commit and
  expire on timeout so a crashed controller cannot hold resources indefinitely.
- **Automatic rollback.** Prior resource state is captured before mutation and
  stored in an OwnerRef'd ConfigMap. On failure, committed changes are reverted
  in reverse order using the stored state.
- **ServiceAccount impersonation.** Resource operations execute under a
  user-specified ServiceAccount identity, enforcing RBAC boundaries per
  transaction.
- **Prometheus observability.** Built-in metrics for phase transitions,
  transaction duration, active transaction counts, per-item operation outcomes,
  and lock operation outcomes.

### Non-goals

- **Enforced locking.** Janus uses advisory Lease locks. It does not intercept
  or block direct `kubectl` writes to locked resources. Operators and users
  who bypass the Transaction CRD are not prevented from writing.
- **Cross-cluster transactions.** The current design targets a single cluster.
- **General workflow orchestration.** Janus executes resource mutations, not
  arbitrary jobs or scripts. For DAG-based workflows, use Argo Workflows or
  Tekton.

## Background

### Kubernetes' design philosophy

> [!NOTE]
> [Kubernetes deliberately does not support atomic transactions across multiple
> resources](https://github.com/kubernetes/design-proposals-archive/blob/main/architecture/architecture.md).
> This is an intentional design decision, not a limitation to be fixed. The
> [architecture principles](https://github.com/kubernetes/design-proposals-archive/blob/main/architecture/principles.md)
> explicitly state that atomic enforcement of invariants is "contention-prone
> and doesn't provide a recovery path in the case of a bug allowing the
> invariant to be violated."
>
> Instead, Kubernetes favors:
> - **Eventual consistency** through level-based controllers that continuously
>   reconcile actual state toward desired state
> - **Decoupled components** coordinating via a shared API rather than
>   distributed transactions
> - **Independent resource operations** where each API write commits
>   independently to etcd
>
> This design maximizes system resilience, extensibility, and horizontal
> scalability. It works well for the vast majority of Kubernetes use cases.

Janus extends Kubernetes where transactional semantics are needed. It does not
replace or contradict Kubernetes' design — it adds an optional layer for
scenarios where atomic multi-resource changes are a business requirement. If
your use case tolerates eventual consistency (and most do), standard Kubernetes
primitives are the right tool.

### The problem

Many Kubernetes operations require coordinated changes across multiple
resources. Deploying a new application version might involve updating a
ConfigMap, patching a Deployment image, and deleting a stale Secret. These
changes are semantically atomic — partial application leaves the cluster in an
inconsistent state — but Kubernetes treats each resource write independently.

Existing approaches handle this incompletely:

| Approach | Limitation |
|---|---|
| `kubectl apply -k` | No rollback on partial failure |
| Helm rollback | Operates on release-level snapshots, not individual resources |
| Argo Rollouts | Scoped to Deployment roll-forward, not arbitrary resources |
| Manual scripts | No crash resilience, no structured compensation |

### Why the Saga pattern

The [Saga pattern](https://dl.acm.org/doi/10.1145/38714.38742) (Garcia-Molina
& Salem, 1987) decomposes a long-lived transaction into a sequence of
sub-transactions, each paired with a compensating action. If the sequence
aborts at step *n*, the compensating actions for steps *n-1* through *1* are
executed in reverse order.

This fits Kubernetes resource mutations well:

| Forward action | Compensating action |
|---|---|
| Create resource | Delete the created resource |
| Update resource | Restore prior state |
| Patch resource | Restore prior state |
| Delete resource | Re-create from stored state |

Unlike two-phase commit, Sagas do not require participants to hold locks across
the prepare-commit boundary. Each sub-transaction commits independently, and
compensation is applied after the fact. This aligns with Kubernetes's
eventually-consistent, reconciliation-driven model.

### Why Lease-based locking

Kubernetes Lease objects (`coordination.k8s.io/v1`) provide a built-in
mechanism for advisory locking with expiration. Janus acquires a Lease per
resource before mutation and releases it on completion. If the controller
crashes, leases expire after `spec.lockTimeout` (default 5 minutes), unblocking
other transactions.

Leases are advisory — they prevent concurrent *Janus transactions* from
conflicting but do not block direct API writes. This is a deliberate trade-off:
enforced locking would require admission webhooks that intercept all writes to
any potentially transacted resource, adding latency and operational complexity
disproportionate to the benefit.

## How It Works

Janus defines a single CRD — `Transaction` — containing an ordered list of
resource mutations (create, update, patch, delete). The controller processes
them as a Saga: each step is paired with a compensating action so the entire
sequence can be rolled back on failure.

```
┌──────────────────────────────────────────────────────────┐
│  Transaction CR                                          │
│  spec.changes: [{target, type, content}, ...]            │
└────────────────────────┬─────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────────┐
│  TransactionReconciler (state machine)                   │
│                                                          │
│  Pending ──► Preparing ──► Prepared ──► Committing       │
│                                           │     │        │
│                                           │     ▼        │
│                                           │  Committed   │
│                                           ▼              │
│                                      RollingBack         │
│                                        │     │           │
│                                        ▼     ▼           │
│                                   RolledBack Failed      │
└────────────┬─────────────────────────────────────────────┘
             │ uses
             ▼
┌──────────────────────────────────────────────────────────┐
│  Lock Manager (internal/lock)                            │
│  Acquire/Release Lease objects (coordination.k8s.io/v1)  │
│  Advisory: available to all, enforced by convention      │
└──────────────────────────────────────────────────────────┘
```

**Prepare phase** — For each resource: acquire a Lease lock, read current
state into a rollback ConfigMap. This builds the compensating actions the
Saga needs if it must abort.

**Commit phase** — For each resource: verify the lock is still held, apply
the mutation. If any step fails, the Saga reverses through committed items
in reverse order, restoring each from the rollback ConfigMap.

One item is processed per reconcile cycle. Progress is persisted to the API
server after each item, so the controller can resume from where it left off
after a crash.

## Example

```yaml
apiVersion: backup.janus.io/v1alpha1
kind: Transaction
metadata:
  name: deploy-v2
spec:
  serviceAccountName: deploy-sa
  lockTimeout: 5m
  changes:
    - target:
        apiVersion: v1
        kind: ConfigMap
        name: app-config
      type: Patch
      content:
        data:
          version: "2.0"

    - target:
        apiVersion: apps/v1
        kind: Deployment
        name: web-server
      type: Patch
      content:
        spec:
          template:
            spec:
              containers:
                - name: web
                  image: myapp:v2.0

    - target:
        apiVersion: v1
        kind: Secret
        name: old-api-key
      type: Delete
```

If the Deployment patch fails, the ConfigMap patch is automatically reverted
to its prior state.

## Implementation

### CRD: Transaction

A single CRD in the `backup.janus.io/v1alpha1` API group. The spec is an
ordered list of resource changes; the status tracks per-item progress through
the state machine.

```
TransactionSpec                          TransactionStatus
┌─────────────────────────────┐          ┌──────────────────────────────┐
│ changes[]                   │          │ phase: Committing            │
│   ┌─ target: {v1/ConfigMap} │          │ version: 4                   │
│   │  type: Patch            │          │ items[]                      │
│   │  content: {data: ...}   │          │   ┌─ lockLease: janus-lock-… │
│   ├─ target: {apps/v1/...}  │          │   │  prepared: true          │
│   │  type: Patch            │          │   │  committed: true         │
│   │  content: {spec: ...}   │          │   ├─ prepared: true          │
│   └─ target: {v1/Secret}    │          │   │  committed: false  ← cur │
│      type: Delete           │          │   └─ prepared: true          │
└─────────────────────────────┘          │ rollbackRef: deploy-v2-rb    │
                                         └──────────────────────────────┘
```

Each mutation type maps to a Kubernetes API call:

| Type | API call | Notes |
|---|---|---|
| `Create` | `client.Create()` | Full resource manifest in `content` |
| `Update` | `client.Update()` | Full resource manifest; fetches current `resourceVersion` first |
| `Patch` | Server-side apply | Field manager `janus-{txnName}`; coexists with HPA and other controllers |
| `Delete` | `client.Delete()` | `content` ignored; idempotent if resource already gone |

### Reconciliation loop

The controller processes **one item per reconcile cycle**. After mutating a
single resource and updating `status.items[]`, it requeues. This ensures every
state transition is persisted to the API server before the next step begins —
a crash at any point can be recovered by re-reading status.

```
reconcile()
  │
  ├─ phase == Pending
  │    create rollback ConfigMap (OwnerRef → Transaction)
  │    init status.items[]
  │    phase → Preparing
  │    requeue
  │
  ├─ phase == Preparing
  │    find first item where prepared == false
  │    acquire Lease lock
  │    read current resource state → rollback ConfigMap
  │    mark item prepared
  │    requeue (or phase → Prepared if all done)
  │
  ├─ phase == Prepared
  │    phase → Committing
  │    requeue
  │
  ├─ phase == Committing
  │    find first item where committed == false
  │    renew lock (extends TTL for this item)
  │    apply mutation
  │    ├─ success: mark item committed, requeue
  │    └─ failure: phase → RollingBack, requeue
  │    (if all done: release locks, delete rollback CM, phase → Committed)
  │
  ├─ phase == RollingBack
  │    iterate items in reverse
  │    find first committed && !rolledBack item
  │    restore from rollback ConfigMap
  │    ├─ success: mark item rolledBack, requeue
  │    └─ failure: return error (controller-runtime retries with backoff)
  │    (if all done: release locks, phase → RolledBack)
  │    (if rollback CM missing: phase → Failed)
  │
  ├─ phase == Failed
  │    if un-rolled-back commits exist and rollback CM present:
  │      phase → RollingBack (automatic recovery)
  │    else: strip finalizer, terminal
  │
  └─ phase ∈ {Committed, RolledBack}
       strip finalizer, terminal — no further reconciliation
```

### Lock manager

Locks are Kubernetes Lease objects in the Transaction's namespace, named
deterministically: `janus-lock-{namespace}-{kind}-{name}` (lowercased).

```
Acquire(key, txnName, timeout)
  │
  ├─ Lease does not exist → create with holder = txnName
  ├─ Lease held by txnName → renew (update renewTime)
  ├─ Lease held by other, not expired → ErrAlreadyLocked
  └─ Lease held by other, expired → force-acquire (update holder + times)

Renew(lease, txnName, timeout)
  │
  ├─ Lease not found → ErrLockExpired
  ├─ Lease held by different txn → ErrAlreadyLocked
  ├─ Lease held by txnName but expired → ErrLockExpired
  └─ Lease held by txnName and valid → update renewTime + duration

Release(lease)
  └─ delete Lease (idempotent if already gone)

ReleaseAll(leases)
  └─ release each lease, return first error encountered
```

Labels on each Lease (`app.kubernetes.io/managed-by: janus`,
`janus.io/transaction: {txnName}`) enable identification. Bulk release
iterates the lease refs stored in `status.items[]`.

### Rollback storage

Prior resource state is stored in a ConfigMap named `{txnName}-rollback`,
owned by the Transaction via OwnerReference (garbage-collected when the
Transaction is deleted).

| Key format | Value |
|---|---|
| `{Kind}_{Namespace}_{Name}` | JSON-serialized resource object |

Keys use underscores as separators — Kubernetes resource names (DNS-1123)
and Kind values never contain underscores, so the format is collision-free.

During rollback, stored objects are cleaned of server-set metadata
(`resourceVersion`, `uid`, `creationTimestamp`, `generation`, `managedFields`,
`status`) before re-creation or update. OwnerReferences and finalizers are
preserved — they were part of the original resource state and are needed to
maintain GC chains and external controller contracts.

The ConfigMap is deleted on successful commit (no longer needed) and preserved
on rollback or failure (available for debugging).

### ServiceAccount impersonation

Resource operations (reads during prepare, mutations during commit, restores
during rollback) execute under the identity of the ServiceAccount named in
`spec.serviceAccountName`. The controller impersonates this SA via the
Kubernetes impersonation API, so the SA's RBAC bindings determine what
resources the transaction can touch. If the SA lacks permission for a
particular operation, the transaction fails cleanly during commit — and any
already-committed items are rolled back.

Impersonating clients are cached per `namespace/saName` pair with lazy
initialization (`sync.Once`). The cache is self-healing: entries are evicted
when SA validation fails (SA deleted or not found), and a fresh client is
created on the next transaction that uses that SA.

### Finalizer

Every Transaction gets a `backup.janus.io/lease-cleanup` finalizer before
any work begins. This ensures that if a Transaction is deleted while leases
are held, the controller gets a chance to release them. The finalizer is
stripped once the Transaction reaches a terminal phase (Committed, RolledBack,
or Failed) so that subsequent deletes are instant.

Deleting a Transaction during an active phase (Preparing, Committing)
releases leases but does **not** roll back partially committed changes.
Rollback is a business decision — use the RollingBack phase for explicit
compensation.

### Metrics

The controller registers Prometheus metrics on the controller-runtime
metrics registry:

| Metric | Type | Labels | Description |
|---|---|---|---|
| `janus_transaction_phase_transitions_total` | Counter | `from_phase`, `to_phase` | State machine edge traversals |
| `janus_transaction_duration_seconds` | Histogram | `outcome` | Wall-clock time from start to terminal phase |
| `janus_transactions_active` | Gauge | `phase` | In-flight transactions per non-terminal phase |
| `janus_item_operations_total` | Counter | `operation`, `result` | Per-item prepare/commit/rollback outcomes |
| `janus_lock_operations_total` | Counter | `operation`, `result` | Lock acquire/renew/release outcomes |
| `janus_transaction_item_count` | Histogram | — | Distribution of items per transaction |

## Getting Started

### Prerequisites
- go version v1.25.0+
- docker version 17.03+
- kubectl version v1.30.0+
- Access to a Kubernetes v1.30.0+ cluster

### Deploy

```sh
make docker-build docker-push IMG=<some-registry>/janus:tag
make install
make deploy IMG=<some-registry>/janus:tag
```

### Try it out

```sh
kubectl apply -k config/samples/
```

### Uninstall

```sh
kubectl delete -k config/samples/
make uninstall
make undeploy
```

## References

See [BIBLIOGRAPHY.md](BIBLIOGRAPHY.md) for annotated references on Sagas,
two-phase commit, crash recovery, and atomic commitment protocols.

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
