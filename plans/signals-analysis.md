# Janus Signals Analysis

## System Dynamics Diagram

```
              Transaction CR (input)
                     │
                     ▼
  ┌─────────────────────────────────────────────────────┐
  │              TransactionReconciler                   │
  │                                                      │
  │  ┌──────┐   ┌──────────┐   ┌──────────┐   ┌──────┐ │
  │  │Pend- │──▶│Preparing │──▶│Committing│──▶│Comm- │ │
  │  │ ing  │   │(1/cycle) │   │(1/cycle) │   │itted │ │
  │  └──────┘   └──┬───────┘   └──┬───────┘   └──────┘ │
  │                 │              │                      │
  │                 │ error        │ error/lock-expired   │
  │                 ▼              ▼                      │
  │           ┌──────────┐   ┌──────────┐                │
  │           │ Failed   │◀──│Rolling-  │◄── no circuit  │
  │           │          │──▶│  Back    │    breaker      │
  │           └──────────┘   └──────────┘                │
  │           recovery loop   retry forever              │
  └──────────────────┬──────────────────────────────────┘
                     │
        ┌────────────┼────────────┐
        ▼            ▼            ▼
  ┌──────────┐ ┌──────────┐ ┌──────────┐
  │  Lease   │ │ Rollback │ │  User    │
  │  Mgr     │ │ ConfigMap│ │ Resource │
  │          │ │ (≤1MB)   │ │  (SSA)   │
  │ TTL only │ │OwnerRef  │ │imperison │
  │ guard    │ │ GC'd     │ │ ated via │
  └──────────┘ └──────────┘ │   SA     │
       │                     └──────────┘
       │
  temporal coupling: locks acquired in Preparing
  are NOT renewed until Committing begins
```

---

## Findings

### 1. Lock Decay During Preparation Phase

**Lens**: Temporal Coupling
**Where**: `internal/controller/transaction_controller.go:155-194` (`handlePreparing`)
**Theory**: Temporal contract violation. The lock acquired for item 0 carries an implicit contract: "I will use this lock within `timeout` seconds." But the preparation phase processes items sequentially — one per reconcile cycle — with no renewal of previously acquired locks. This creates a deadline chain where the budget for all subsequent items must fit within item 0's lock TTL.

**Dynamics**: A transaction with N items, where each preparation cycle takes T_cycle (status update + requeue + API calls), has a total preparation time of N × T_cycle. The first lock expires at T_timeout after acquisition. If N × T_cycle > T_timeout, the first lock expires before committing begins. When `handleCommitting` tries to renew that lock, it fails with `ErrLockExpired` or `ErrAlreadyLocked` (if another transaction took it over), triggering rollback — undoing all preparation work.

With defaults: T_timeout = 300s (5 min). Under normal API latency (~50ms round-trip, 3 API calls per prepare item + status update), T_cycle ≈ 200ms. So N_max ≈ 300s / 0.2s = 1500 items before lock decay becomes an issue at baseline. But under API server pressure (T_cycle = 2-5s), N_max drops to 60-150 items. Under a slow API server (T_cycle = 30s), N_max = 10 items.

```
Lock timeline for 10-item transaction:

  Item 0 lock acquired ──── 5min TTL ──── EXPIRED
  │                                         │
  ├─ Item 1 prepared  (T+0.2s)              │
  ├─ Item 2 prepared  (T+0.4s)              │
  ├─ ...                                    │
  ├─ Item 9 prepared  (T+1.8s)              │
  ├─ Transition to Prepared (T+2.0s)        │
  ├─ Transition to Committing (T+2.2s)      │
  └─ Commit item 0: RENEW ← succeeds       │
      (still within 5min)                   │
                                            │
  Under API pressure (T_cycle = 30s):       │
  ├─ Item 9 prepared  (T+270s)              │
  ├─ Transition to Committing (T+300s)      │
  └─ Commit item 0: RENEW ← ErrLockExpired! ▼
```

**Severity**: Low at steady state (most transactions have few items). High under overload (API server latency spikes push T_cycle up, reducing the effective N_max). This is the kind of bug that manifests only under stress — a cliff edge.

**Proposed direction**: Renew all held locks during each preparation cycle, or at minimum, check remaining TTL and renew proactively when it drops below a threshold. The renewal cost is one API call per held lock per cycle — bounded by N, which is already bounded by the items-per-transaction distribution (the `janus_transaction_item_count` histogram captures this). Reference: Nygard's *Release It!*, "Timeouts" pattern — always renew leases if you're going to hold them across multiple work units.

---

### 2. Metastable Rollback Loop

**Lens**: Feedback Loop (Positive / Self-sustaining)
**Where**: `internal/controller/transaction_controller.go:97-108` (Failed recovery) + `handleRollingBack:209-248`
**Theory**: Metastable failure (Bronson et al., HotOS 2021). The system enters a state where the recovery mechanism itself sustains the failure condition, even after the original trigger is removed.

**Dynamics**: The Failed → RollingBack recovery path is level-triggered: every reconcile of a Failed transaction with un-rolled-back commits and an existing rollback ConfigMap transitions to RollingBack. If RollingBack then hits a persistent failure (target resource in a bad state, RBAC revoked, API server rejecting the restore), the error is returned to controller-runtime, which retries with backoff. But the rollback never completes, the transaction never reaches a terminal state, and the system cycles:

```
  Failed ──(recovery check)──▶ RollingBack ──(item fails)──▶ error returned
    ▲                                                            │
    │                                                            │
    └─── NO, stays in RollingBack. Retried forever by ◄─────────┘
         controller-runtime backoff
```

`handleRollingBack` returns the error directly on item failure. It does NOT call `setFailed`. So the transaction stays in `RollingBack` phase, and controller-runtime retries. It never goes back to `Failed` from `RollingBack` (unless the ConfigMap disappears). The system is stuck in RollingBack with exponential backoff retries — functionally a metastable state.

The backoff does prevent a retry storm (gain < 1), but the transaction never reaches a terminal state. The `janus_transactions_active{phase="RollingBack"}` gauge increments and never decrements. Over time, these accumulate.

**Severity**: Medium. The individual transaction doesn't cause harm (backoff prevents resource waste), but it's a slow leak: each stuck rollback is one more entry in the work queue, one more active-transactions gauge tick, one more set of held leases (if not expired). The accumulated effect over weeks could be significant.

**Proposed direction**: Add a max-retry counter or max-duration deadline to rollback. After exhausting retries, transition to Failed with a condition indicating rollback was abandoned. This creates a definitive terminal state. The `janus_item_operations_total{operation="rollback",result="error"}` counter already tracks failures — add a threshold check. Reference: The metastable failure literature recommends "escape hatches" — mechanisms that detect self-sustaining failure and force the system into a known-safe terminal state.

---

### 3. Rollback ConfigMap Size Cliff

**Lens**: Saturation and Overload
**Where**: `internal/controller/transaction_controller.go:278-296` (`saveRollbackState`)
**Theory**: Cliff-type saturation. ConfigMaps have a hard 1MB limit enforced by the Kubernetes API server. The system stores serialized JSON of full resource objects, including metadata, spec, and status (only `status` is stripped by `cleanForRestore`, but during *save* the full object is marshaled).

**Dynamics**: If a transaction touches N resources, the rollback ConfigMap stores N serialized resource snapshots. Large resources (CRDs with complex specs, Deployments with many containers, ConfigMaps with large data sections) can easily exceed 100KB each. At 10 resources × 100KB = 1MB — exactly at the cliff edge.

When the limit is hit, the API server returns a 413 (RequestEntityTooLarge) or a validation error. This propagates as a `ResourceOpError`, which triggers `failAndReleaseLocks`. The failure happens during Preparing, before any mutations — so no data loss. But the transaction is permanently failed for structural reasons the user can't fix without splitting it into smaller transactions.

```
Rollback ConfigMap capacity:

  Items × Avg Resource Size = Total
  5 × 10KB  = 50KB   ✓ comfortable
  5 × 100KB = 500KB  ✓ approaching
  10 × 100KB = 1MB   ✗ CLIFF

  No graceful degradation path:
  request ──▶ API server ──▶ 413 ──▶ failAndReleaseLocks
```

**Severity**: Low at steady state (most ConfigMaps/Secrets are small). Medium for users with large CRDs or large data payloads. The failure mode is safe (fails before mutations), but the error message doesn't explain the root cause — it just says "saving rollback state: <API error>".

**Proposed direction**: Two options:
1. **Signal clearly**: Calculate the serialized size before writing and return a descriptive error if approaching the limit.
2. **Structural**: Store rollback data in a per-item ConfigMap (one CM per resource, OwnerRef'd to the transaction) instead of a single aggregate CM. This pushes the limit to 1MB per resource instead of 1MB total. Cost: more API calls during prepare, more objects to manage.

---

### 4. Active Transactions Gauge Drift After Crash

**Lens**: Signal Integrity (Information Loss)
**Where**: `internal/controller/transaction_controller.go:341-360` (`transition` + `recordPhaseChange`)
**Theory**: Data processing inequality (Shannon). The gauge metric is derived from in-process state transitions. If the process crashes between persisting the phase change (status update) and recording the metric, the information is lost — the gauge drifts. This is a lossy transform: I(actual_phase; metric) < I(actual_phase; status) after a crash.

**Dynamics**: `transition()` first updates the status (persisted in etcd), then calls `recordPhaseChange()` (in-memory Prometheus counter). If the process crashes between these two operations, the status reflects the new phase but the gauge doesn't. On restart, the reconciler picks up from the persisted phase — but never reconciles the gauge.

Over time, the drift accumulates: each crash during a transition adds ±1 to the gauge error. The gauge could become negative (impossible in reality), or show phantom active transactions that are actually completed.

```
Timeline:
  t0: status updated to Committing   (etcd ✓)
  t1: CRASH
  t2: restart, reconcile from Committing
      → never decrements Preparing gauge
      → Preparing gauge permanently +1 too high
```

**Severity**: Low in practice (crashes during the microsecond between status update and metric recording are unlikely). But the error accumulates without bound and is never corrected. If you operate Janus at scale and rely on `janus_transactions_active` for alerting or capacity decisions, the drift produces false positives.

**Proposed direction**: Replace the event-sourced gauge with a periodic reconciliation loop that counts actual Transaction CRs in each phase. A Prometheus `Collector` interface implementation that queries the k8s API every scrape would be perfectly accurate but adds API load proportional to scrape frequency. Alternatively, accept the drift and add a periodic reconciliation goroutine (e.g., every 60s) that resets the gauge to match reality. Reference: The Nyquist-Shannon sampling theorem applies to the reconciliation frequency — it must sample at least 2× the rate of transitions to capture true state.

---

### 5. Impersonation Client Cache Lacks Bounded Eviction

**Lens**: Cross-talk / Saturation
**Where**: `internal/controller/transaction_controller.go:70` (`impersonatedClients sync.Map`)
**Theory**: Unbounded growth (queue without drain). The `sync.Map` grows by one entry per unique `namespace/saName` pair. Entries are evicted only when SA validation fails (SA not found), but successful entries persist forever. In a cluster with many namespaces and rotating service accounts, the map grows monotonically.

**Dynamics**: Each `cachedClient` holds a `client.Client`, which includes HTTP transport, connection pools, and TLS state. These are not lightweight. In a cluster with 100 namespaces × 10 SAs = 1000 entries, each holding an HTTP client with 100 idle connections, that's 100K idle connections consuming kernel file descriptors and memory.

In practice, the growth is bounded by the number of *unique* `namespace/saName` pairs across all Transactions that have been processed. For most deployments, this is small. But in multi-tenant clusters with automated Transaction creation per namespace, it can grow without bound.

**Severity**: Low for typical deployments. Medium for multi-tenant at-scale deployments. There's no observability into the cache size — no metric tracks it.

**Proposed direction**: Add an LRU eviction policy or a time-based TTL to the cache. Alternatively, add a metric (`janus_impersonated_clients_cached`) to make the growth visible. The cost of recreating a client is one REST config copy + one HTTP transport setup — bounded and fast.

---

### 6. No Backpressure on Concurrent Transaction Volume

**Lens**: Saturation / Feedback
**Where**: `internal/controller/transaction_controller.go:415-420` (`SetupWithManager`)
**Theory**: Open-loop control. The system has no feedback path from load to admission. The controller processes every Transaction CR without regard to how many are already in flight. In control theory terms, the system lacks a feedback path H(s) — it's purely feedforward.

**Dynamics**: Under steady state, the work queue drains at the rate of one reconcile per transaction per cycle. Under burst load (e.g., a CD pipeline applies 50 Transactions simultaneously), all 50 enter the work queue. Each competes for API server bandwidth, lock acquisition, and controller goroutines. The API server's response time degrades nonlinearly (M/M/1 queuing: W = 1/(μ-λ)), and all transactions slow down together.

```
Response time under load (M/M/1 model):

  Utilization ρ     W (response time factor)
  0.5               2×
  0.7               3.3×
  0.9               10×
  0.95              20×

  50 concurrent transactions on a controller tuned for 10
  → API server utilization jumps from ρ=0.5 to ρ=0.95
  → per-reconcile latency jumps 10×
  → lock decay risk (Finding #1) activated
```

**Severity**: Low for typical use. High under burst load. The system has no cliff (it degrades gradually as API latency increases), but the lock decay issue (Finding #1) creates a derived cliff: when T_cycle increases enough, locks start expiring, triggering cascading rollbacks.

**Proposed direction**: Two options:
1. Set `MaxConcurrentReconciles` in `SetupWithManager` to cap concurrent reconciles. This provides a simple admission control knob.
2. Add a controller-level rate limiter that observes `janus_transactions_active` and delays new transaction processing when the gauge exceeds a threshold. This closes the feedback loop.

---

### 7. Delete During Active Transaction Leaves Partial State

**Lens**: Mode Transition
**Where**: `internal/controller/transaction_controller.go:130-148` (`handleDeletion`)
**Theory**: Incomplete mode transition. Deletion during the Committing phase releases locks and removes the finalizer, but does NOT roll back partially committed resources. The system transitions from "actively mutating cluster state" to "deleted" without an intermediate cleanup phase.

**Dynamics**: User deletes a Transaction while it's in Committing phase (items 0-4 committed, items 5-9 pending). `handleDeletion` releases all leases and removes the finalizer. The Transaction CR is garbage collected. But items 0-4's mutations remain in the cluster, and items 5-9 are unapplied. The cluster is in a state that was never intended to exist — a partial application of what was supposed to be an atomic set of changes.

The rollback ConfigMap is OwnerRef'd to the Transaction, so it gets GC'd too — eliminating the only record of what the original resource states were.

The comment in the code acknowledges this is intentional. But from a signals perspective, this is a half-state: the system looked alive (Transaction existed, finalizer present) but the outcome is undefined.

**Severity**: Medium. The failure mode requires deliberate user action (delete during active processing), but the consequence is severe — silently partial state with no recovery path. Once the Transaction and its rollback ConfigMap are deleted, there's no way to identify or undo the partial changes.

**Proposed direction**: Either (a) refuse deletion during non-terminal phases by keeping the finalizer and instead initiating rollback, or (b) emit a prominent warning Event before allowing the deletion to proceed. The current best-effort approach is pragmatic but produces silent partial state.

---

## Risk Summary

| # | Finding | P(trigger) | Blast radius | Priority |
|---|---------|-----------|-------------|----------|
| 1 | Lock decay during preparation | Low-Med (API pressure) | High (cascading rollback) | **High** |
| 2 | Metastable rollback loop | Med (persistent target failure) | Med (stuck transaction, resource leak) | **High** |
| 6 | No backpressure / admission control | Low (burst load) | High (amplifies #1) | **Med** |
| 7 | Delete during active leaves partial state | Low (deliberate action) | High (unrecoverable partial state) | **Med** |
| 3 | Rollback CM size cliff | Low (large resources) | Low (safe fail, before mutations) | **Low** |
| 4 | Active transactions gauge drift | Very low (crash timing) | Low (metric inaccuracy) | **Low** |
| 5 | Unbounded impersonation client cache | Very low (many unique SAs) | Low (memory growth) | **Low** |

---

## Closing

### 1. Stability Assessment

**Conditionally stable.** The system is stable under nominal conditions: small transactions (< 50 items), moderate API latency (< 500ms per call), and no persistent failures in target resources. The controller-runtime work queue + exponential backoff provide sufficient negative feedback to prevent divergence.

The instability conditions are:
- **API server pressure** (T_cycle rises) → triggers lock decay (Finding #1)
- **Persistent target failure** (resource can't be rolled back) → metastable rollback (Finding #2)
- **Burst load** (many concurrent transactions) → API saturation → amplifies #1

The gain margin is healthy for most workloads, but thin under stress. The phase margin (tolerance for delays) is set by the 5-minute lock timeout, which is generous under nominal conditions but erodes rapidly under load.

### 2. Weakest Transition

**Committing → RollingBack → (stuck).** The transition from committing to rollback is triggered correctly (lock renewal failure, commit failure). But the rollback phase has no terminal escape: if the rollback itself keeps failing, the transaction remains in RollingBack indefinitely. This is the only transition path that can produce an unbounded non-terminal state.

### 3. Top 3 Dynamic Risks

1. **Lock decay during preparation** (P: Low-Med × Impact: High) — Renew all held locks during each prepare cycle, not just the current item's lock. Cost: O(N) API calls per cycle, where N is the number of already-prepared items.

2. **Metastable rollback** (P: Med × Impact: Med) — Add a max-retry counter or max-duration deadline to rollback items. After exhaustion, transition to Failed with an explicit "rollback-abandoned" condition. The user can then investigate and retry manually.

3. **No admission control under burst load** (P: Low × Impact: High) — Set `MaxConcurrentReconciles` to a reasonable default (e.g., 5-10) in `SetupWithManager`. This caps the controller's API server impact and provides headroom for lock TTLs.
