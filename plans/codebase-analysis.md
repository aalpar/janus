# Janus Codebase Evaluation

**Date**: 2026-02-16
**Scope**: Full codebase (5,554 LOC across 15 Go files)
**Method**: Structural analysis against dependency minimization, state tightness, composability, and semantic documentation principles

## Dependency Map

```
                    ┌──────────────┐
                    │     cmd      │
                    │   I = 1.0    │  (Ca=0, Ce=4)
                    └──┬──┬──┬──┬─┘
                       │  │  │  │
          ┌────────────┘  │  │  └────────────┐
          v               │  v                v
 ┌────────────────┐       │  ┌──────────┐  ┌─────────────┐
 │ api/v1alpha1   │       │  │  lock    │  │  metrics    │
 │   I = 0.0      │       │  │  I = 0.0 │  │  I = 1.0    │
 │  (Ca=2, Ce=0)  │       │  │(Ca=2,Ce=0│  │ (Ca=2,Ce=0) │
 └────────────────┘       │  └──────────┘  └─────────────┘
          ^               │       ^               ^
          │               v       │               │
          │    ┌──────────────────┴───────────────┘
          │    │   controller                     │
          └────│   I = 1.0  (Ca=1, Ce=4)          │
               └──┬───────────────────────────────┘
                  │
                  v
          ┌──────────────┐
          │ impersonate  │
          │   I = 0.0    │
          │ (Ca=1, Ce=0) │
          └──────────────┘

Internal edges: 8 total
Cycles: 0 (clean DAG)
SDP violations: 0
```

The dependency graph is a textbook clean DAG. Four leaf packages (`api`, `lock`, `impersonate`, `metrics`) with zero internal efferent coupling each. The `controller` is the sole integrator, and `cmd` is pure wiring. No strongly connected components; every package can be tested in isolation.

**Verdict: Dependency topology is correct.** No package imports something less stable than itself. The leaf packages depend only on Kubernetes client libraries (infrastructure stable). The integrator (`controller`) depends on all four leaves, which is the expected star topology for a Saga coordinator.

---

## Findings

### 1. ItemStatus: Product-Type Explosion

**Principle**: State Tightness
**Where**: `api/v1alpha1/transaction_types.go:90-106`
**Theory**: Product types multiply state spaces. Three independent booleans create 2^3 = 8 representable states. The valid states for ItemStatus are a strict subset.

**Current state**: `ItemStatus` has three booleans -- `Prepared`, `Committed`, `RolledBack` -- that represent per-item lifecycle phase.

**State space**:

| Prepared | Committed | RolledBack | Semantic? | Meaning |
|----------|-----------|------------|-----------|---------|
| F | F | F | Y | Pending |
| T | F | F | Y | Prepared |
| T | T | F | Y | Committed |
| T | T | T | Y | RolledBack |
| F | T | F | N | Committed without preparing? |
| F | F | T | N | Rolled back without preparing or committing? |
| F | T | T | N | Same |
| T | F | T | N | Prepared, not committed, but rolled back? |

**Representable: 8. Valid: 4. Precision: 50%.**

These four valid states form a strict linear order: Pending -> Prepared -> Committed -> RolledBack. They also carry an `Error` string that is meaningful only in some states.

An enum `{Pending, Prepared, Committed, RolledBack}` (or a nullable `ItemPhase` typed string to match the Kubernetes idiom) would have precision of 100% and eliminate the 4 impossible states. The boolean triple is a **product type encoding a sum type** -- the classic antipattern from Minsky's "Effective ML."

**Proposed direction**: Replace the three booleans with an `ItemPhase` typed string enum, mirroring the pattern already used for `TransactionPhase`. In the Kubernetes CRD world, a string enum is the idiomatic sum type.

**Impact**: Eliminates 4 impossible states. Removes implicit temporal ordering contracts between booleans (the invariant "Committed implies Prepared" is currently enforced only by code, not by the type). Simplifies all loop predicates that currently check boolean combinations (`item.Committed && !item.RolledBack` becomes `item.Phase == ItemPhaseCommitted`).

**Caveat**: This is a breaking API change. Given v0.0.1 with zero consumers, that's fine per project policy.

---

### 2. Reconciler God-Method Surface Area

**Principle**: Composability
**Where**: `internal/controller/transaction_controller.go` (778 lines, ~25 methods on one struct)

**Theory**: A function that does A-then-B is a **sequential composition**. If A and B have different preconditions, they belong in separate functions (Hoare logic: `{P} A {Q}; {Q} B {R}`). When A and B also operate on different data, they belong in separate *types*.

**Current state**: `TransactionReconciler` owns:
- State machine dispatch (`Reconcile`)
- Lock coordination (calls into `LockMgr`)
- Resource CRUD (`applyChange`, `getResource`, `unmarshalContent`)
- Rollback logic (`applyRollback`, `saveRollbackState`, `cleanForRestore`)
- Impersonation client cache management
- Status persistence (`transition`, `updateStatusAndRequeue`, `setFailed`)
- Metrics recording (`recordPhaseChange`)
- Event emission

This is *one* struct with **three orthogonal concerns**: (1) state machine transitions, (2) resource operations, (3) cache/lock management. The methods compose correctly today, but the struct is a product of concerns -- changing the resource operation strategy requires touching the same file as changing the state machine logic.

**Proposed direction**: Not necessarily a code split today (the file is 778 lines, which is manageable), but recognize that `applyChange` and `applyRollback` are a **symmetry pair** -- they form an inverse operation. If extracted behind an interface `ResourceOperator{Apply, Rollback}`, it would:
- Make the symmetry explicit in the type system
- Enable testing the state machine without real resource operations
- Prepare for dry-run mode (a `ResourceOperator` that reads but doesn't write)

This is the "sort.Slice opportunity" -- a single interface that would serve commit, rollback, and dry-run.

**Impact**: Medium. The current coupling is not causing bugs, but it will create friction when adding features like dry-run or transaction cancellation.

---

### 3. applyChange/applyRollback: Hand-Unrolled Dispatch

**Principle**: Composability
**Where**: `transaction_controller.go:456-517` and `transaction_controller.go:520-586`

**Theory**: Hand-unrolled dispatch over an enum with `switch` is the transition from **enumeration to induction**. Both `applyChange` and `applyRollback` switch on `ChangeType` and each branch follows a similar pattern: unmarshal or fetch, then mutate.

The two switches are **duals** of each other: Create<->Delete, Update<->Update(prior), Patch<->Update(prior). This inverse relationship is a **group structure** -- each operation has an inverse, composition is associative, and the identity is "no change." In abstract algebra, this makes `{Create, Delete, Update, Patch}` with their inverses a groupoid (partial group, since the inverse of Create isn't defined without the created object).

**Current state**: The pairing is implicit. Nothing in the type system connects "Create" with "its rollback is Delete." This is a **documentation gap** -- the invariant is maintained by code review, not by structure.

**Proposed direction**: Not necessarily a refactor today, but the pairing should be documented as a contract. If this ever grows (e.g., adding a `ChangeTypeStrategicMergePatch`), both switches must be updated in lockstep. A table-driven approach would make the pairing explicit:

```go
var changeOps = map[ChangeType]struct{ Apply, Rollback func(...) error }{
    Create: {apply: createResource, rollback: deleteResource},
    Delete: {apply: deleteResource, rollback: recreateFromSnapshot},
    // ...
}
```

But the current switch statements are only 4 arms each. The complexity doesn't yet justify the abstraction. Flag for when a 5th ChangeType arrives.

**Impact**: Low today, high when the enum grows.

---

### 4. sync.Map for Impersonation Cache -- Phantom Eviction

**Principle**: State Tightness
**Where**: `transaction_controller.go:71` and `transaction_controller.go:165-193`

**Theory**: `sync.Map` has type `map[any]any` internally -- it's the loosest possible product type for a key-value store. The key is `string` (namespace/saName) and the value is `*cachedClient`. The `sync.Once` inside `cachedClient` creates a **typestate** where the client is either uninitialized, initialized-success, or initialized-error. But this state is not directly observable -- you must call `once.Do` and check afterward.

**Current state**: When SA validation fails (line 175), the entry is evicted. When client creation fails (line 188), the entry is evicted. But there's a subtle race: between `LoadOrStore` (line 181) and `once.Do` (line 183), another goroutine could have already started initializing the same key. The `sync.Once` handles this correctly (only one init runs), but the **eviction** on line 188 races with concurrent readers who may have already gotten a reference to the entry.

The code handles this correctly because `LoadOrStore` returns the *existing* entry if one exists, and `sync.Once` ensures single initialization. But the eviction pattern (`Delete` then next call does `LoadOrStore` fresh) means a brief window where a stale entry could be returned before the delete propagates. In practice, this is benign for a Kubernetes controller with sequential reconciliation per object -- but it's worth noting as a **latent concern** if concurrency increases.

**Proposed direction**: Document the concurrency contract: "Safe for concurrent use because the reconciler processes one Transaction at a time per key (controller-runtime guarantee). The cache is shared across Transactions using the same SA."

**Impact**: Low. Correctness risk is minimal given controller-runtime's per-object serialization.

---

### 5. cleanForRestore Deletes Status Unconditionally

**Principle**: State Tightness / Semantic Documentation
**Where**: `transaction_controller.go:735-745`

**Theory**: `delete(obj.Object, "status")` is a blanket operation on an `unstructured.Unstructured` -- it strips *all* status, regardless of whether the original resource had status that was meaningful for restore. For resources like ConfigMaps and Secrets (no status subresource), this is harmless. For resources with status (Deployments, Pods), the status is server-managed and would be rejected on Create anyway, so stripping it is correct.

**Current state**: The function's **postcondition** is: "the returned object is suitable for `Create` or `Update`." This postcondition is satisfied, but it's not documented.

**Hoare triple**:
```
{obj is a snapshot of a live K8s resource}
  cleanForRestore(obj, ns)
{obj is suitable for Create/Update -- no server-assigned fields, correct namespace}
```

**Proposed direction**: Add a comment stating the postcondition. The function is correct; the contract is implicit.

**Impact**: Low. Documentation only.

---

### 6. recordPhaseChange Gauge Accounting Has an Implicit Invariant

**Principle**: State Tightness
**Where**: `transaction_controller.go:748-760`

**Theory**: The `ActiveTransactions` gauge is incremented when entering a non-terminal phase and decremented when leaving. This creates a **representation invariant**: for any phase P, `ActiveTransactions[P] >= 0` and the sum of all gauge values equals the number of in-flight transactions. The invariant is maintained by the structure of `recordPhaseChange` -- but it depends on being called for **every** phase transition, including the implicit `"" -> Pending` transition.

**Current state**: Line 754 has the guard `from != "" && from != TransactionPhasePending`. This means entering Pending from `""` (empty, i.e. first reconcile) increments the gauge, but the decrement condition excludes both `""` and `Pending`. This is correct: a fresh Transaction goes `"" -> Pending` (gauge +1 for Pending), then `Pending -> Preparing` (gauge -1 for Pending, +1 for Preparing), and eventually reaches a terminal state (gauge -1, no +1).

The logic is correct but **fragile** -- it depends on the empty string being semantically equivalent to Pending. If a code path calls `recordPhaseChange("Pending", "Pending")` by accident, the gauge would be wrong (-1 for Pending, +1 for Pending = net 0, but the decrement should not have happened if the phase didn't actually change).

**Proposed direction**: Add a guard `if from == to { return }` at the top. Idempotent phase transitions should be no-ops for accounting.

**Impact**: Low. Defensive hardening.

---

### 7. Metrics Package: Global State via init()

**Principle**: Dependency Minimization
**Where**: `internal/metrics/metrics.go:50-58`

**Theory**: Package-level `init()` is a **hidden temporal dependency** (Parnas, 1972). Any package that imports `metrics` (even blank-imported) triggers registration. The `init()` function runs at program startup, not at a point chosen by the caller. This is the standard Prometheus pattern, but it means the metrics package cannot be imported in unit tests without polluting the global registry.

**Current state**: `cmd/main.go` does `_ "github.com/aalpar/janus/internal/metrics"` and `internal/controller` imports it for the variable references. The controller's unit tests presumably also trigger registration.

**Proposed direction**: This is the idiomatic controller-runtime approach. Not worth changing. But be aware that if you ever need to test metrics registration logic, the global state makes it difficult. The standard mitigation is a custom `prometheus.Registry` in tests.

**Impact**: None actionable. This is Go ecosystem convention, not a design flaw.

---

### 8. RollbackDataError.Err -- Dual-Meaning nil

**Principle**: State Tightness
**Where**: `internal/controller/errors.go:47-62`

**Theory**: `RollbackDataError.Err` being nil means "missing data"; non-nil means "deserialization failure." A nil field carrying semantic meaning is **boolean blindness** (Harper, *Practical Foundations*) -- the nil/non-nil distinction encodes information that should be in the type. The `Error()` method switches on this, producing two different messages.

**State space**: `Key x (nil | error)` = string x 2 variants. The nil variant means "key not found" and the non-nil means "key found but corrupt." These are genuinely different failure modes.

**Representable: inf x 2. Valid: inf x 2. Precision: 100%.** Both states are valid. The concern is readability, not precision. At the call sites (`applyRollback`), the distinction between "missing" and "corrupt" is lost because the caller wraps it uniformly.

**Proposed direction**: Could split into `ErrRollbackKeyMissing{Key}` and `ErrRollbackCorrupt{Key, Err}` for cleaner `errors.Is/As` matching. But given only 2 call sites, this is low priority.

**Impact**: Low.

---

### 9. lock.Manager Interface -- Clean Contract (Positive)

**Principle**: Composability
**Where**: `internal/lock/manager.go:44-59`

The `Manager` interface is a 4-method contract. Each method has a clear pre/postcondition:

```
{key identifies a lockable resource}  Acquire(key, txn, timeout)  {lease exists, held by txn}
{lease exists, held by txn}           Renew(lease, txn, timeout)  {lease extended}
{lease exists}                        Release(lease)              {lease deleted}
{leases exist}                        ReleaseAll(leases)          {all leases deleted}
```

This is a well-designed interface -- it's the minimum API surface for advisory locking. `LeaseManager` implements it using Kubernetes Leases, but any backend (Redis, etcd directly, in-memory for tests) could satisfy the contract. The interface is a **projection** -- it exposes only the lock semantics, hiding the Kubernetes-specific implementation details.

`ReleaseAll` is a **fold** over `Release`: `ReleaseAll(leases) = foldl(Release, nil, leases)`. It's a convenience, not a new operation.

**Impact**: Positive. No changes needed.

---

### 10. impersonate.NewClient -- Pure Function, Minimal Surface (Positive)

**Principle**: Composability
**Where**: `internal/impersonate/client.go:31-43`

This is a **pure construction function** -- it takes inputs, produces an output, has no side effects beyond the returned client. It depends on `rest.Config`, `runtime.Scheme`, and `meta.RESTMapper` -- all immutable after construction. The function is referentially transparent: calling it twice with the same arguments produces equivalent clients.

At 13 lines with zero internal state, this is the ideal composable unit. The package exists to separate the impersonation concern from the controller, which is the right call per Parnas: the impersonation config format (`system:serviceaccount:ns:name`) is a design decision likely to change.

**Impact**: Positive. No changes needed.

---

## Opportunity: ResourceOperator Interface

**Replaces**: `applyChange` + `applyRollback` + future dry-run mode

**Core operation**: "Mutate a Kubernetes resource according to a ChangeType, or reverse the mutation"

**Algebraic structure**: **Groupoid** -- each operation has an inverse (Create<->Delete, Update<->Restore, Patch<->Restore), but the inverse requires additional data (the rollback snapshot). Not a full group because the identity ("no change") isn't explicitly represented.

**Proposed shape**:
```go
type ResourceOperator interface {
    Apply(ctx context.Context, change ResourceChange, namespace, txnName string) error
    Rollback(ctx context.Context, change ResourceChange, namespace string, snapshot *corev1.ConfigMap) error
}
```

**Reuse sites**: (1) live commits, (2) rollback, (3) dry-run mode (Apply that validates but doesn't persist), (4) unit testing the state machine in isolation.

---

## Summary

### State-Space Summary

| Type | Representable States | Valid States | Precision |
|------|---------------------|-------------|-----------|
| `TransactionPhase` | 8 enum values | 8 | 100% |
| `ChangeType` | 4 enum values | 4 | 100% |
| `ItemStatus` (3 bools) | 8 | 4 | **50%** |
| `RollbackDataError` | inf x 2 | inf x 2 | 100% |

**Combined: 256 representable boolean combinations in `ItemStatus` across a 4-item transaction (8^4 = 4096), of which only 4^4 = 256 are valid. Aggregate precision for the per-item state: 50%.**

### Dependency Count

- **8 internal edges**, 0 cycles, 0 SDP violations
- **Instability range**: [0.0, 1.0] -- leaves at 0.0, integrators at 1.0. Textbook clean.
- **Eliminable edges**: 0. All internal dependencies are necessary.
- **External module deps**: 189 (transitive). Typical for a controller-runtime project. No unnecessary external dependencies introduced.

### Top 3 Highest-Impact Changes

1. **Replace `ItemStatus` booleans with `ItemPhase` enum** -- eliminates 4 impossible states per item (50% precision -> 100%), simplifies all status-checking predicates, matches existing `TransactionPhase` pattern.

2. **Extract `ResourceOperator` interface** -- makes `applyChange`/`applyRollback` symmetry explicit, enables dry-run mode and isolated state-machine testing. Doesn't require immediate refactoring -- define the interface and have the reconciler satisfy it.

3. **Guard `recordPhaseChange` against no-op transitions** -- defensive hardening for gauge accounting correctness. One line: `if from == to { return }`.
