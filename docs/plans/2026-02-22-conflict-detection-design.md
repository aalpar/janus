# Conflict Detection Design

## Problem

Between prepare (read current state) and commit (apply mutation), another actor
can modify a target resource. For Update this partially surfaces via
`resourceVersion` conflicts, but Patch and Delete are blind to intervening
changes. The rollback snapshot becomes stale, and the commit overwrites changes
silently.

## Decisions

- **On conflict:** Fail the transaction without automatic rollback. The user
  decides whether to rollback or re-submit.
- **Scope:** Update, Patch, and Delete. Create has no prior state to conflict
  with.
- **Storage:** `resourceVersion` stored in `ItemStatus` (CRD field), not
  extracted from the rollback ConfigMap.

## Design

### Phase 1: Pre-commit version check (Approach A)

Capture `resourceVersion` during prepare. Before each commit, GET the target
resource and compare its current `resourceVersion` against the stored one. On
mismatch, fail the transaction with a `ConflictDetected` condition.

Covers all three change types uniformly. One check point, one code path.

### Phase 2: Native preconditions (Approach C)

Layer Kubernetes-native preconditions to close the TOCTOU gap between the
Phase 1 check and the actual mutation:

- **Update:** Set the stored `resourceVersion` on the object instead of
  fetching a fresh one. The API server rejects if it changed. Eliminates the
  redundant GET currently in `applyChange`.
- **Delete:** Pass `Preconditions{ResourceVersion: storedRV}` to the delete
  call.
- **Patch (SSA):** No native mechanism. Phase 1 is the only defense.

When the API server returns 409 Conflict or a precondition failure, translate
it to the same `ErrConflictDetected` type for consistent user-facing messages.

## Changes

| File | Change |
|------|--------|
| `api/v1alpha1/transaction_types.go` | Add `ResourceVersion` field to `ItemStatus` |
| `api/v1alpha1/zz_generated.deepcopy.go` | Regenerate |
| `internal/controller/transaction_controller.go` | Capture RV in prepare, add `checkConflict`, call in commit, modify `applyChange` for Update/Delete preconditions, translate 409 errors |
| `internal/controller/errors.go` | Add `ErrConflictDetected` |
| `internal/controller/transaction_controller_test.go` | Tests for conflict detection |

## What doesn't change

- **Rollback logic** — conflict detection is a commit-time concern.
- **Lock manager** — locks prevent concurrent Janus transactions; conflict
  detection catches external actors. Orthogonal.
- **Prepare for Create** — no `resourceVersion` to capture. `applyChange`
  already handles `AlreadyExists` idempotently.
