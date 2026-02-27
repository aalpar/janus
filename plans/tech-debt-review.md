# Janus Technical Debt Review

**Date:** 2026-02-27
**Scope:** Full codebase — code quality review + staff engineer technical debt assessment
**Method:** Two parallel analyses (structural code review + technical debt audit), findings deduplicated and cross-referenced

---

## Critical — Must Fix

### C1. Silently discarded `json.Marshal` error on rollback metadata

**Where:** `internal/controller/transaction_controller.go:439`

**What:** `metaJSON, _ := json.Marshal(meta)` — the error is assigned to `_` and never checked. If marshal fails, `metaJSON` is nil, and `string(nil)` writes an empty string to the `_meta` key in the rollback ConfigMap. The controller proceeds to the Preparing phase with corrupted rollback data.

**Why it matters:** Offline recovery via `janus recover` becomes impossible for that transaction. `recover.BuildPlan` at `internal/recover/plan.go:53` would fail with "rollback ConfigMap missing _meta key" — but only when the user tries to recover, not when the data was lost.

**Fix:** Check the error, matching the pattern at lines 496-499:
```go
metaJSON, err := json.Marshal(meta)
if err != nil {
    return ctrl.Result{}, r.setFailed(ctx, txn, fmt.Sprintf("serializing rollback metadata: %v", err))
}
```

**Effort:** S

---

### C2. OwnerReference on rollback ConfigMap missing `Controller` and `BlockOwnerDeletion`

**Where:** `internal/controller/transaction_controller.go:412-417`, `cmd/janus/main.go:230-235`

**What:** The OwnerReference on the rollback ConfigMap lacks `Controller: ptr.To(true)` and `BlockOwnerDeletion: ptr.To(true)`. Without `BlockOwnerDeletion`, the garbage collector can delete the ConfigMap before the Transaction's finalizer (`tx.janus.io/lease-cleanup`) has run.

**Why it matters:** If GC races the finalizer during Transaction deletion, rollback data is destroyed. The finalizer at line 676-682 handles a missing rollback ConfigMap by transitioning to Failed — so the failure is surfaced, but recovery is impossible. Same issue exists for ResourceChange OwnerReferences in the CLI.

**Fix:** Add both fields to the OwnerReference:
```go
OwnerReferences: []metav1.OwnerReference{{
    APIVersion:         backupv1alpha1.GroupVersion.String(),
    Kind:               "Transaction",
    Name:               txn.Name,
    UID:                txn.UID,
    Controller:         ptr.To(true),
    BlockOwnerDeletion: ptr.To(true),
}},
```
Apply the same fix at `cmd/janus/main.go:230-235`.

**Effort:** S

---

## High Priority

### H1. String matching for rollback conflict detection

**Where:** `internal/controller/transaction_controller.go:701`

**What:** `strings.Contains(item.Error, "rollback conflict")` is used to decide whether to skip items that had a rollback conflict in a previous reconcile. This matches on the human-readable error string from `ErrRollbackConflict.Error()`.

**Why it matters:** If the error message is reworded, conflicted items start being re-attempted instead of skipped, potentially causing silent overwrites. This violates the project's convention of using typed errors and `errors.As` at boundaries.

**Fix:** Add a `ConflictDetected` bool field to `ItemStatus`. Set it when a rollback conflict is recorded. Check the field instead of parsing the error string.

**Effort:** S

---

### H2. Inline rollback ConfigMap manipulation in `handlePreparing`

**Where:** `internal/controller/transaction_controller.go:486-531`

**What:** The `ChangeTypeCreate` path in `handlePreparing` manually fetches the rollback ConfigMap, marshals an Envelope, writes it back, and updates — all inline. This is the same procedure that `saveRollbackState` (line 1131) encapsulates for every other change type. Lines 495-511 are a hand-unrolled version of "write an envelope to the rollback ConfigMap." The only data difference: the Create-not-found envelope has no `PriorState`.

**Why it matters:** If the rollback ConfigMap write procedure changes (key format, error handling, metadata), `saveRollbackState` gets updated and this inline copy doesn't. The duplicated `cm.Data == nil` initialization (line 504 vs line 1154) is a concrete example.

**Fix:** Have `saveRollbackState` accept an optional `*unstructured.Unstructured` (nil for "no prior state") instead of requiring one, then call it uniformly for all change types.

**Effort:** S

---

### H3. Duplicate `ResourceRef` / `MetaTarget` types

**Where:** `api/v1alpha1/transaction_types.go` (`ResourceRef`), `internal/rollback/types.go` (`MetaTarget`)

**What:** Structurally identical types (APIVersion, Kind, Name, Namespace) serving the same purpose. `MetaTarget` exists to keep the rollback package free of API dependencies. The controller manually converts between them at line 428. 64 uses of `ResourceRef`, 20 uses of `MetaTarget` across the codebase.

**Why it matters:**
- If a field is added to `ResourceRef`, the field-by-field conversion at line 428 silently drops it
- The same conceptual error (RV conflict) has three distinct types: `ErrConflictDetected` and `ErrRollbackConflict` (controller, using `ResourceRef`) and `ErrConflict` (recover, using `MetaTarget`)
- The decoupling rationale is sound in principle, but both the controller and CLI already import the API package

**Fix:** Either accept the API dependency in `rollback` (same module, stable CRD types) and unify on `ResourceRef`, or extract a minimal `ResourceID` type into `internal/types` that both packages import.

**Effort:** M

---

## Medium Priority

### M1. `fmt.Errorf` in production controller code

**Where:** `internal/controller/transaction_controller.go` — lines 371, 456, 551, 687, 898, 1015

**What:** Six call sites return `fmt.Errorf` instead of typed errors. Violates the CLAUDE.md convention: "No `fmt.Errorf` in production — always project error types with Unwrap()."

**Why it matters:** Callers cannot use `errors.As` to dispatch on these errors. Pragmatic impact is low today (nobody matches on them), but the convention exists to preserve the option.

**Fix:** Wrap in `ResourceOpError` for the listing errors. For the `errUnknownChangeType` wrapping (lines 898, 1015), the `%w` is correct for `errors.Is` — consider documenting the exception or wrapping in a typed error.

**Effort:** M

---

### M2. `fmt.Errorf` throughout `internal/recover` package

**Where:** `internal/recover/apply.go` (lines 41, 45, 54, 69, 78, 83, 100, 124), `internal/recover/plan.go` (lines 53, 57, 69)

**What:** 11 occurrences of `fmt.Errorf` in CLI-adjacent code. The package already defines `ErrConflict` as a typed error, showing the pattern is understood but not applied consistently.

**Fix:** Lower priority than M1. Either introduce typed errors for apply/plan paths or explicitly exempt CLI-adjacent code from the convention.

**Effort:** M

---

### M3. `listChanges` fetches all ResourceChanges in namespace, filters client-side

**Where:** `internal/controller/transaction_controller.go:1306-1331`

**What:** `r.List(ctx, &list, client.InNamespace(txn.Namespace))` fetches all ResourceChanges, then filters by OwnerReference UID in a loop. Called 4 times per reconcile. With N transactions each with M items, every reconcile fetches N*M objects.

**Why it matters:** The CLI already labels ResourceChanges with `tx.janus.io/transaction` (cmd/janus/main.go:228). This label is available for server-side filtering but unused.

**Fix:** Add `client.MatchingLabels{"tx.janus.io/transaction": txn.Name}` to the List call. Keep the UID check as a safety belt. Document the label as a required convention.

**Effort:** S

---

### M4. Controller file is 1427 lines with interleaved concerns

**Where:** `internal/controller/transaction_controller.go`

**What:** Single file contains reconcile dispatcher, all phase handlers, all resource operations, all status helpers, rollback data helpers, utility functions, and the impersonation client cache (~40 methods/functions).

**Why it matters:** Adding a new ChangeType requires touching `applyChange`, `applyRollback`, `handlePreparing`, and `computeReversePatch` — all scattered across the same flat namespace. Not broken at 1427 lines, but the blast radius of adding a ChangeType is 4+ functions.

**Fix:** Extract resource operations into `internal/controller/resource_ops.go`. Extract rollback helpers into `internal/controller/rollback_helpers.go`. File split only — no new interfaces or abstraction changes.

**Effort:** M

---

### M5. Impersonation client cache never fully evicts

**Where:** `internal/controller/transaction_controller.go:62-80, 227-256`

**What:** `impersonatedClients` is a `sync.Map` keyed by `namespace/saName`. Entries are evicted on SA-not-found errors, but successful entries live forever. Each entry holds a `client.Client` with its own HTTP transport and connection pool.

**Why it matters:** In a multi-tenant cluster with SA rotation, the map grows unboundedly — memory leak proportional to distinct SAs ever used. Not urgent at v0.0.1 scale.

**Fix:** Add a TTL or LRU bound, or evict when no active transactions reference the SA. At minimum, add a `// TODO: add eviction` comment.

**Effort:** M

---

### M6. Three parallel conflict error types

**Where:** `internal/controller/errors.go` (`ErrConflictDetected`, `ErrRollbackConflict`), `internal/recover/apply.go` (`ErrConflict`)

**What:** Three types representing the same semantic: "resource's current state doesn't match expected." They differ in field types (`ResourceRef` vs `MetaTarget`) and naming.

**Why it matters:** Adding RV-based conflict handling in a new context requires choosing among three types or creating a fourth. Dependent on H3 (type unification).

**Fix:** Unify the resource identifier type first (H3), then collapse `ErrRollbackConflict` and `ErrConflict` into one type.

**Effort:** M (dependent on H3)

---

### M7. `handlePending` annotation write doesn't return

**Where:** `internal/controller/transaction_controller.go:381-389`

**What:** After writing the `automatic-rollback` annotation via `r.Update()`, execution falls through into status modification instead of returning and requeueing. This produces two API writes in a single reconcile for the first-time case — inconsistent with the "one write per reconcile" pattern used for finalizer additions (lines 106-111, 160-164).

**Why it matters:** If the status update fails after the annotation write succeeds, the reconcile is partially applied. Inconsistency with established pattern.

**Fix:** Return after the annotation write and let the next reconcile handle initialization.

**Effort:** S

---

### M8. `guessAPIVersion` silent wrong default for unknown kinds

**Where:** `cmd/janus/main.go:469-482`

**What:** Maps 8 well-known Kinds to API versions, defaults to `"v1"` for everything else. CRDs and resources like `NetworkPolicy` silently get the wrong apiVersion.

**Why it matters:** Failure is silent at `add` time and only manifests during commit as a confusing "resource not found" error.

**Fix:** Add an `--api-version` flag. When omitted, use the guess but log a warning for unrecognized Kinds.

**Effort:** S

---

### M9. `setLeaseTime` truncates sub-second durations, accepts negative

**Where:** `internal/lock/manager.go:88`

**What:** `int32(timeout.Seconds())` truncates. Duration < 1s becomes 0 (instantly-expired lease). Negative durations produce negative `LeaseDurationSeconds`.

**Why it matters:** A misconfigured `lockTimeout` of 500ms silently defeats the locking mechanism — every lock acquisition by another transaction succeeds immediately.

**Fix:** Enforce minimum 1s floor in the controller's `lockTimeout()` helper at line 1168, or add CRD validation markers.

**Effort:** S

---

## Low Priority

### L1. Import alias `backupv1alpha1` is a legacy artifact

**Where:** 7 files importing `github.com/aalpar/janus/api/v1alpha1`

**What:** Alias suggests the project was originally named "backup." The project is now "janus" with API group `tx.janus.io`.

**Fix:** Rename to `txv1alpha1` or `v1alpha1`. Grep-and-replace across 7 files.

**Effort:** S

---

### L2. `LeaseName` can exceed 253-character Kubernetes name limit

**Where:** `internal/lock/manager.go:68-74`

**What:** `"janus-lock-" + namespace + "-" + kind + "-" + name` can reach 392 characters with max-length components.

**Fix:** Truncate or hash when exceeding 253 characters.

**Effort:** S

---

### L3. `rollback.Key` underscore separator collision risk

**Where:** `internal/rollback/types.go:64-66`

**What:** `Key(kind, namespace, name)` uses `_` separator. If any component contains underscores, keys can collide. Extremely unlikely with standard Kubernetes naming.

**Fix:** No action needed today. Switch separator if CRD support broadens.

**Effort:** S

---

### L4. ResourceChange webhook `ValidateUpdate` is a no-op

**Where:** `api/v1alpha1/transaction_webhook.go:98-102`

**What:** Comment says "always-immutable on update" but validator accepts all updates. A `kubectl edit` can change the spec freely between creation and seal.

**Fix:** Add spec comparison matching the Transaction pattern, or update the comment to reflect actual behavior.

**Effort:** S

---

### L5. `conditionForPhase` allocates map per call

**Where:** `internal/controller/transaction_controller.go:1227-1245`

**What:** Allocates a `map[TransactionPhase]condDef` on every call. Not a performance concern (called once per transition), but a `switch` would be more idiomatic.

**Fix:** Replace with `switch` statement.

**Effort:** S

---

### L6. No `default` case in webhook `validateResourceChangeSpec` switch

**Where:** `api/v1alpha1/transaction_webhook.go:123-134`

**What:** Switch on `rc.Spec.Type` handles all four types but no `default`. CRD enum marker provides server-side validation, so this is defense-in-depth only.

**Fix:** Add `default:` case returning validation error.

**Effort:** S

---

### L7. `computeReversePatch` does not remove fields added by forward patch

**Where:** `internal/controller/transaction_controller.go:1373-1392`

**What:** Fields added by a forward SSA patch are omitted from the reverse patch (line 1378 `continue`). SSA with `ForceOwnership` doesn't remove absent fields owned by the same field manager. Fields added by forward patch persist after rollback.

**Why it matters:** Known SSA limitation, verified by test at line 4555. Not documented anywhere as a known limitation.

**Fix:** Document as known limitation. Users needing full reversal should use Update type instead of Patch.

**Effort:** S

---

### L8. CLI duplicate recovery setup code

**Where:** `cmd/janus/main.go:325-372` (runRecoverPlan), `cmd/janus/main.go:374-443` (runRecoverApply)

**What:** ~28 lines of identical preamble: load Transaction, build rbCMName, build txnItems map, load rollback ConfigMap, call BuildPlan.

**Fix:** Extract `loadRecoverContext()` helper.

**Effort:** S

---

### L9. Missing test coverage for `handleDeletion` protected-state paths

**Where:** `internal/controller/transaction_controller.go:340-362`

**What:** `handleDeletion` at 73.1% coverage (per TODO.md). Uncovered: deletion during non-terminal phases with unrolled commits but no automatic-rollback annotation, and retry-rollback annotation during Failed phase deletion.

**Fix:** Already tracked in TODO.md.

**Effort:** S

---

## Strengths

- **Crash resilience** — one-item-per-reconcile with persisted progress, consistently applied across all phase handlers
- **Self-write detection** — distinguishes Janus-authored writes from external modifications after crash-restart
- **Typed errors at boundaries** — `ResourceOpError`, `ErrConflictDetected`, `ErrRollbackConflict`, `ErrAlreadyLocked`, `ErrLockExpired`, all with `Unwrap()`
- **Test suite** — ~5500 lines of envtest-backed tests covering all state transitions, conflict paths, crash-retry idempotency, lease contention, timeout handling
- **SSA field manager scoped per transaction** — prevents cross-transaction field conflicts
- **Rollback RV verification on every rollback path** — no silent overwrites
- **Clean separation of concerns** — rollback package independent of API types, impersonate package is stateless 28-line factory

---

## Recommended Attack Order

### Phase 1 — Safety (15 minutes)
1. **C1** — Check `json.Marshal` error on rollback metadata
2. **C2** — Add `Controller` + `BlockOwnerDeletion` to OwnerReferences

### Phase 2 — Fragility (30 minutes)
3. **H1** — Add `ConflictDetected` bool to `ItemStatus`, replace string matching
4. **H2** — Generalize `saveRollbackState` to accept nil prior state

### Phase 3 — Consistency (1-2 hours)
5. **H3** — Unify `ResourceRef` / `MetaTarget`
6. **M6** — Collapse conflict error types (enabled by H3)
7. **M1** — Replace `fmt.Errorf` in controller with typed errors

### Phase 4 — Performance + Polish (opportunistic)
8. **M3** — Add label filter to `listChanges`
9. **M4** — Split controller file
10. Everything else — fix when in the area
