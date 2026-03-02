# Janus Invariants

What Janus promises, what it assumes, and where the boundaries are.

Derived from the controller implementation as of 2026-02-28.

---

## Safety Invariants

Properties that must never be violated.

### S1. No partial commit without rollback path

If any item in a transaction is committed, either (a) all items will eventually commit, or (b) a rollback ConfigMap exists containing sufficient state to reverse all committed items.

**Enforcement:** The rollback ConfigMap is created in `handlePending` *before* transitioning to Preparing. Every item's prior state is saved to the ConfigMap before the commit happens. The ConfigMap is OwnerRef'd to the Transaction.

**Assumption:** The rollback ConfigMap never exceeds the 1MB Kubernetes ConfigMap limit. If a `cm.Update` fails mid-prepare because of this, earlier items' rollback state is already saved but none have been committed yet — safe, but the transaction fails without a clear "rollback data too large" message.

### S2. No silent overwrites of external modifications (commit path)

If an external actor modifies a resource after Janus prepared it but before Janus commits, the transaction must fail — not silently overwrite.

**Enforcement:**
- **Update/Delete:** RV-based `checkConflict` compares the resource's current resourceVersion against the one captured at prepare time.
- **Create (new resource):** Existence check before commit — if the resource was created externally between prepare and commit, the transaction fails with `ErrConflictDetected`.
- **Create (existing resource) / Patch:** Not enforced via RV. These use Server-Side Apply, where field ownership is the conflict mechanism. SSA will not overwrite fields owned by other managers, but *will* overwrite fields it owns even if the resource was modified externally.

**Scope limitation:** For SSA operations, the invariant is "no overwrites of fields Janus doesn't own" — weaker than full optimistic concurrency. This is intentional (SSA's design), but users should understand that Patch operations may overwrite concurrent changes to the same fields.

### S3. No silent overwrites of external modifications (rollback path)

If an external actor modifies a resource after Janus committed it but before rollback, the rollback must detect the conflict — not silently revert the external change.

**Enforcement:**
- **Create rollback (delete):** Compares post-commit RV against current RV before deleting.
- **Create rollback (restore via SSA):** `checkRollbackRV` compares post-commit RV.
- **Patch rollback (reverse SSA):** `checkRollbackRV` compares post-commit RV.
- **Update rollback:** Uses RV on the update call itself (Kubernetes rejects stale writes).
- **Delete rollback (re-create):** No conflict check — the resource is gone, so there's nothing to conflict with. `AlreadyExists` on retry is treated as a no-op.

**On conflict:** The item is marked with `ErrRollbackConflict`, skipped, and the transaction continues rolling back remaining items. After all items are processed, the transaction enters Failed with a message directing the user to `janus recover`.

### S4. Mutual exclusion of resources across transactions

Two Janus transactions cannot both hold a lock on the same resource simultaneously, unless one's lease has expired.

**Enforcement:** `LeaseManager.Acquire` checks for an existing Lease object. If held by a different transaction and not expired, `ErrAlreadyLocked` is returned. The acquiring transaction fails.

**Scope:** Advisory locking only. Non-Janus actors (kubectl, other controllers) are not blocked. Janus detects their modifications via RV checks (S2, S3) but cannot prevent them.

### S5. Phase monotonicity

A transaction's phase advances forward through the state machine only:

```
Pending → Preparing → Prepared → Committing → Committed
                                      ↓
                              RollingBack → RolledBack
                                      ↓
                                    Failed
```

No backward transitions, with two exceptions:

1. **Failed → RollingBack** when the transaction has unrolled commits (`hasUnrolledCommits` is true) AND the rollback ConfigMap still exists. This recovery re-entry happens automatically on every reconcile of a Failed transaction that meets both conditions.

2. **Committed → RollingBack** when the `request-rollback` annotation is added to a Committed transaction. The controller consumes the annotation and transitions to RollingBack. This is a one-shot user-initiated action.

### S6. One item per reconcile cycle

Each reconcile processes at most one uncommitted (or unrolled-back) item, then persists status and requeues.

**Purpose:** Crash resilience. If the controller crashes after committing item N but before committing item N+1, the persisted status reflects item N's commit. On recovery, item N is skipped (already committed) and item N+1 is attempted.

**Enforcement:** `return r.updateStatusAndRequeue(ctx, txn)` at the end of each item loop iteration in `handlePreparing`, `handleCommitting`, and `handleRollingBack`.

### S7. Self-write detection on crash retry

If the controller crashes after committing an item but before updating status, the retry must not false-fail with a conflict error.

**Enforcement:** `updateRollbackRV` stores the post-commit resourceVersion in the rollback ConfigMap after each successful commit. On retry, `readCommittedRV` reads this value and `checkConflict` recognizes a match as `errSelfWrite` rather than an external modification. The item is marked committed without re-applying.

**Scope:** Only applies to Update/Delete (RV-checked operations). SSA Create/Patch are inherently idempotent — re-applying produces the same result.

---

## Liveness Invariants

Properties that must eventually hold.

### L1. Every sealed transaction reaches a terminal phase

A sealed transaction must eventually reach Committed, RolledBack, or Failed.

**Enforcement:**
- Transaction timeout (`spec.timeout` or default) fires in `checkTimeout` — transitions to RollingBack (if commits exist) or Failed (if none).
- Lock renewal failure triggers rollback.
- Rollback timeout transitions to Failed.

**Assumption:** `r.Status().Update(ctx, txn)` inside `transition()` eventually succeeds. If it keeps failing (e.g., persistent conflict with another writer), the transaction is stuck. Controller-runtime retries with backoff, but there's no circuit breaker.

### L2. Locks are eventually released

All leases acquired by a transaction are eventually released — either explicitly by the controller or implicitly by timeout expiry.

**Enforcement:**
- `releaseAllLocks` runs on all terminal transitions and deletion handling.
- Lease expiry as fallback — other transactions can take over expired leases.

**Gap:** `releaseAllLocks` logs errors but does not surface them. If release fails, the `tx.janus.io/lease-cleanup` finalizer isn't removed, which blocks Transaction deletion. The lease will eventually expire (allowing other transactions to proceed), but the Transaction object itself is stuck until either the release succeeds on retry or the finalizer is manually removed.

### L3. Rollback ConfigMap is eventually cleaned up

The rollback ConfigMap is preserved when the transaction reaches Committed (available for `request-rollback`) and cleaned up via OwnerReference when the Transaction is deleted.

**Enforcement:** The ConfigMap is OwnerRef'd to the Transaction, so Kubernetes garbage-collects it when the Transaction is deleted. No explicit deletion on commit.

---

## Boundary Invariants

Scope of guarantees — what Janus does and does not protect against.

### B1. Cooperative actors only

Janus provides isolation guarantees only between Janus transactions, not against arbitrary Kubernetes actors.

Advisory locking cannot prevent `kubectl edit` or modifications by other controllers. RV-based conflict detection *detects* external modifications but cannot *prevent* them. This is a fundamental consequence of the advisory locking model — Janus trades enforcement for zero-install compatibility (no admission webhooks required to protect target resources).

### B2. Single-controller assumption

Correctness assumes a single controller instance is reconciling transactions at any time.

Controller-runtime's leader election provides this. If leader election fails or two controllers run simultaneously, the one-item-per-cycle invariant (S6) breaks — both could attempt to commit the same item concurrently.

### B3. Kubernetes API linearizability

RV-based conflict detection assumes that Kubernetes API operations (Get, Update, Create, Delete) are linearizable with respect to resourceVersion.

This holds for etcd-backed Kubernetes. If a caching layer (e.g., aggressive informer cache) serves stale reads, `checkConflict` could miss modifications. The controller uses direct client calls (not cached reads) for target resource operations, so this holds in practice.

---

## Properties Janus Does Not Provide

Invariants users might expect but that the design explicitly does not guarantee.

### N1. Snapshot isolation

External observers can see a partially-committed transaction's effects. Between committing item 0 and item 1, item 0's changes are visible while item 1's are not.

This is inherent to the Saga pattern. Janus provides eventual consistency with rollback capability, not snapshot isolation. If observation-time atomicity is required, it must be handled at the application level.

### N2. Deterministic winner under contention

If two transactions target overlapping resources and are sealed simultaneously, the winner is determined by lock acquisition order. Lock acquisition order depends on reconcile scheduling, which is non-deterministic.

Both transactions could fail (if each acquires some locks but not all). The system is safe (no silent overwrites), but the outcome is non-deterministic.

### N3. Bounded rollback duration

Rollback duration is bounded by the transaction timeout, but individual rollback operations can take arbitrarily long (network latency, API server load). If a single rollback operation takes longer than the remaining timeout window, the transaction transitions to Failed with some items still not rolled back.

The `janus recover` CLI exists for this scenario — manual intervention to resolve items that couldn't be automatically rolled back.
