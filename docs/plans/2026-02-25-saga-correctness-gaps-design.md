# Saga Correctness Gaps — Design

## Problem

Three correctness gaps exist in the commit and rollback paths of
`TransactionReconciler`:

1. **Crash between commit and status update → false conflict.**
   `handleCommitting` writes the target resource, then updates Transaction
   status in a separate API call. If the process crashes between these two
   writes, the retry path runs `checkConflict`, sees that the resourceVersion
   changed (by Janus's own previous write), and falsely fails the transaction.
   Affects Update, Delete, and Patch operations.

2. **SSA rollback has no conflict detection.**
   Update rollback checks the post-commit RV against the current RV and
   returns `ErrRollbackConflict` when they differ. SSA Patch and Create
   rollback apply a reverse patch with `ForceOwnership` and no RV check,
   silently overwriting external modifications.

3. **Create-of-new-resource has no conflict detection.**
   When `expectedRV == ""` (resource didn't exist at prepare time),
   `checkConflict` returns nil. If an external actor creates the resource
   between prepare and commit, SSA Apply merges onto it silently. On rollback,
   Janus deletes the entire resource — including the external actor's work.

## Design

### Gap 1: Skip `checkConflict` for SSA; detect self-writes for Update/Delete

**Observation:** `checkConflict` is redundant for SSA Create/Patch. SSA's field
ownership is the conflict mechanism, and SSA Apply is idempotent — re-applying
the same patch on retry produces the same result. The false-conflict problem
arises because `checkConflict` runs before the re-apply and sees Janus's own
previous write as an external modification.

**Changes:**

1. `handleCommitting`: Only call `checkConflict` for `ChangeTypeUpdate` and
   `ChangeTypeDelete`. SSA operations skip it entirely.

2. `checkConflict` signature: Add a `committedRV string` parameter. When
   `expectedRV != currentRV`, if `committedRV != "" && committedRV == currentRV`,
   return nil (self-write from a crashed previous attempt detected).

3. `handleCommitting`: Before calling `checkConflict` for Update/Delete, read
   the post-commit RV from the rollback ConfigMap (already stored there by
   `updateRollbackRV`). Pass it as `committedRV`.

**Remaining narrow window:** Crash between `applyChange` and `updateRollbackRV`
means the rollback ConfigMap still has the prepare-time RV. On retry,
`committedRV` won't match → false conflict → Failed. This window is two
consecutive API calls wide. Consequence is false failure, not data corruption.
Acceptable for v0.

### Gap 2: Add RV check before SSA rollback

**Changes to `applyRollback`:**

1. **Patch rollback** (currently line 919–948): Before applying the reverse
   patch, fetch the current resource and load the stored post-commit RV from
   the rollback ConfigMap. If `storedRV != "" && currentRV != storedRV`, return
   `ErrRollbackConflict`.

2. **Create rollback — resource existed** (currently line 886–903): Same RV
   check before applying the reverse SSA patch.

3. **Create rollback — resource didn't exist** (currently line 875–884): Before
   deleting, fetch the resource. Compare its RV against the post-commit RV
   stored in the rollback ConfigMap. If they differ, return
   `ErrRollbackConflict`. This prevents deleting a resource that was modified
   after Janus created it.

**Prerequisite:** `updateRollbackRV` (currently line 991) runs only for Update
and Patch. Extend it to also run for **Create** so the post-commit RV is
available for rollback conflict detection.

### Gap 3: Detect external creation when `expectedRV == ""`

**Changes to `handleCommitting`:**

For `ChangeTypeCreate` where `item.ResourceVersion == ""`: before calling
`applyChange`, fetch the target resource. If it exists, return
`ErrConflictDetected` with `Expected: ""`.

This check lives in `handleCommitting` rather than `checkConflict` because
after the Gap 1 fix, `checkConflict` is only called for Update/Delete.

## Testing

Each gap requires at least one test that:

- **Gap 1:** Simulates the crash-retry scenario — item's target resource already
  has the post-commit RV, but `item.Committed` is false. Verify the transaction
  continues rather than false-failing.
- **Gap 2:** Modifies a resource externally after Janus commits an SSA
  Create/Patch, then triggers rollback. Verify `ErrRollbackConflict` is raised.
- **Gap 3:** Creates a resource externally between prepare and commit for a
  Create operation. Verify conflict detection.

## Future Work (see TODO.md)

- Per-item rollback ConfigMaps for transactions exceeding ~1.5MB
- Consolidate rollback state into Transaction CR for small transactions
