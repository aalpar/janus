# Request-Rollback Annotation

**Date:** 2026-03-01

## Problem

Committed transactions cannot be rolled back via kubectl. The `janus recover`
CLI handles manual recovery, but there is no controller-driven path to undo a
committed transaction. This creates an asymmetry: kubectl can trigger commit
(seal) but not rollback.

## Design

Add a `tx.janus.io/request-rollback` annotation. When the controller detects
it on a Committed transaction, it consumes the annotation (removes it) and
transitions the transaction to RollingBack. The existing rollback machinery
handles the rest — RV conflict detection, per-item progress, and the
Failed-with-conflicts terminal state.

### Annotation semantics

| Property | Value |
|----------|-------|
| Key | `tx.janus.io/request-rollback` |
| Value | ignored (presence is the trigger) |
| Trigger | one-shot — controller removes after consuming |
| Valid phases | Committed only |
| On other phases | ignored (no-op) |

### Reconcile flow change

The Committed case in `Reconcile` (currently strips finalizer and returns)
gains a check before returning:

```
Committed transaction
  ├─ has request-rollback annotation?
  │   ├─ remove annotation
  │   ├─ emit RequestRollback event
  │   └─ transition → RollingBack
  └─ no → strip lease-cleanup finalizer, return (existing behavior)
```

### Rollback ConfigMap preservation

Previously, `handleCommitting` eagerly deleted the rollback ConfigMap after
all items committed. This is incompatible with request-rollback — the
rollback data must survive past commit. The eager deletion was removed;
the ConfigMap's OwnerReference to the Transaction handles cleanup via
Kubernetes garbage collection when the Transaction is deleted.

### Edge cases

**Rollback ConfigMap missing.** `handleRollingBack` already checks for this
and marks the transaction Failed with a descriptive message. This can occur
if the Transaction was committed before the preservation fix, or if the
ConfigMap was manually deleted.

**Locks already released.** Committed transactions have released all locks.
Rollback does not re-acquire them. Safety comes from RV conflict detection
on each item — the same mechanism used by `retry-rollback` on Failed
transactions.

**Rollback conflicts.** Handled identically to any other rollback: conflict
items are skipped, the transaction lands in Failed with un-rolled-back items,
and `janus recover` handles the rest.

**Annotation on non-Committed transaction.** Ignored. The check only runs
in the Committed branch of the reconcile loop.

## Changes

| File | Change |
|------|--------|
| `api/v1alpha1/transaction_types.go` | Add `AnnotationRequestRollback` constant |
| `internal/controller/transaction_controller.go` | Extract `handleTerminal`; check annotation in Committed case; remove eager rollback CM deletion |
| `internal/controller/transaction_controller_test.go` | Unit test: Committed + annotation → RollingBack; annotation consumed |
| `test/e2e/e2e_test.go` | E2e test: full round-trip Create+Patch → Committed → annotate → RolledBack |
| `docs/TUTORIAL.md` | Add kubectl rollback example to Part 5 |
| `docs/USER_GUIDE.md` | Document the annotation |
| `CLAUDE.md` | Add annotation to the Annotations & Finalizers table |

No webhook changes — annotations are not subject to spec validation.
