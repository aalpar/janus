# Saga Engine Library — Contract Design

Status: Design conversation in progress
Date: 2025-02-25

## Scope

**Library A**: K8s-independent saga engine. No K8s imports. Operates on abstract
steps with pluggable interfaces. Could drive database migrations, file operations,
anything with rollback semantics.

Separate from **Library B** (Janus K8s client library that talks to CRDs) and the
**CRD adapter** (operator that plugs K8s primitives into A's interfaces).

## Prior Art

Design informed by established transaction processing literature:

- **Garcia-Molina & Salem, "Sagas" (1987)** — foundational paper. A saga is a
  sequence T1...Tn with compensating transactions C1...Cn-1. Compensation is
  *semantic* — restores to "an acceptable approximation" of prior state, not
  necessarily exact state. The Saga Execution Component (SEC) owns execution
  and relies on a persistent saga log. Save points mark progress between steps.
- **X/Open XA / DTP model (1991)** — industry standard for distributed 2PC.
  Resource Manager (RM) interface: start, end, prepare, commit, rollback,
  recover, forget. Key insight: prepare is RM-owned — the TM asks "can you
  commit?" and the RM does whatever it needs internally.
- **ARIES recovery algorithm** — before-images (undo) and after-images (redo)
  in log records. Compensation Log Records (CLRs) ensure crash-during-undo
  doesn't re-undo. Maps to our checkpoint-after-each-rollback-step requirement.
- **Jim Gray, "Transaction Processing: Concepts and Techniques" (1993)** —
  foundational textbook on TM/RM interfaces and 2PC protocol.
- **PostgreSQL 2PC** — PREPARE TRANSACTION makes state durable in WAL.
  Prepared state is "detachable" — another session can commit/rollback.
  Maps to our RetrySignal model (user can walk away, engine keeps going).
- **Jakarta Transactions 2.0 (JTA)** — Java mapping of XA. XAResource interface
  mirrors X/Open exactly. Provides the most complete modern API reference.

## Engine Responsibilities

The engine owns:
- Phase state machine: Pending → Preparing → Prepared → Committing → Committed
  (or → RollingBack → RolledBack / Failed)
- One-item-per-call processing (caller drives the loop)
- Lock acquisition before Prepare, lock renewal before Forward
- Ordering (forward) and reverse ordering (rollback)
- Error interpretation (nil / Skippable / error → retry)
- Retry decision via RetrySignal (not context cancellation)
- Timeout enforcement
- PreCommitCheck as a protocol step (enforces that the check happens)
- Calling Capture during Prepare for uncaptured items

The engine does NOT own:
- The reconcile/iteration loop — caller drives it
- Sleep/backoff timing — engine returns a duration, caller sleeps
- Context cancellation semantics — ctx is propagated, not interpreted
- State persistence — engine returns new state, caller persists
- Rollback data storage — caller's responsibility (Janus may provide
  a default implementation later, but not in v1)
- Conflict detection logic — executor performs the check, engine
  enforces the protocol step and interprets the result
- Before-image capture timing — user can Capture early or let
  the engine trigger it during Prepare

## Core API Shape

```go
// Capture: optional, user-initiated before-image acquisition.
// Not in XA (XA's RM captures internally during prepare).
// Our extension: allows user-controlled timing for before-image.
// Items not captured before Step() are auto-captured during Prepare.
Capture(ctx context.Context, state State, item ItemID, deps Dependencies) (State, error)

// Step: advance the transaction by one item. Stateless.
// Maps to XA's TM driving the protocol; each call is one RM interaction.
Step(ctx context.Context, state State, deps Dependencies) (State, Result, error)
```

**Stateless.** The engine takes full transaction state on each call and returns
the new state. No hidden state between calls.

Why stateless:
- **Crash resilience is free.** Caller loads last checkpoint, passes it in.
  No separate "recover" path — normal path handles it.
  (Maps to XA's xa_recover: discover in-doubt transactions after restart.)
- **No stale cached state.** State may change between calls (another process,
  config change, crash recovery). Engine always works from current truth.
- **Testability.** Construct any state directly, test any phase transition
  without driving through N prior steps.
- **Mirrors the K8s reconciler pattern.** Each Reconcile() fetches fresh state.
  The library makes this explicit.

Result tells the caller:
- "I progressed one item, call me again"
- "Phase complete, call me again for the next phase"
- "Terminal state, done"
- "Retrying, call me again after backoff duration X"

Minimum caller workflow:
```go
state := NewTransaction(items, config)
// optionally: state, _ = Capture(ctx, state, itemID, deps)
for {
    state, result, err = Step(ctx, state, deps)
    persist(state)
    if result.Terminal { break }
    sleep(result.RequeueAfter)
}
```

## Interfaces the Engine Requires

### StepExecutor

Maps to XA's Resource Manager interface. Six methods:

```go
type StepExecutor interface {
    // Capture: before-image acquisition. Called by Capture() API or by
    // engine during Prepare for uncaptured items. The executor reads
    // current state and stores rollback data however it wants.
    // (ARIES: "before-image"; XA: internal to RM during prepare)
    Capture(ctx context.Context, item Item) error

    // Prepare: XA's xa_prepare. The executor does whatever it needs
    // to guarantee it can commit or rollback if asked. Engine acquires
    // lock first. For items already captured, Capture is skipped.
    Prepare(ctx context.Context, item Item) error

    // PreCommitCheck: not in XA (XA assumes prepare is sufficient).
    // Added because our prepare and commit can be separated by
    // arbitrary time. Verifies state hasn't changed since prepare.
    PreCommitCheck(ctx context.Context, item Item) error

    // Forward: XA's xa_commit. Apply the mutation.
    Forward(ctx context.Context, item Item) error

    // Reverse: XA's xa_rollback / Garcia-Molina's compensating transaction.
    // Semantic undo — not necessarily exact state restoration.
    // (Garcia-Molina: "an acceptable approximation to the state before")
    Reverse(ctx context.Context, item Item) error

    // Forget: XA's xa_forget. Clean up executor-side resources for an item
    // that has reached its per-item terminal state. Called after:
    //   - Forward succeeds and transaction reaches Committed
    //   - Reverse succeeds (item rolled back)
    //   - Manual resolution of a Skippable item (via recovery)
    // Failure is non-fatal — orphaned data, not a correctness issue.
    Forget(ctx context.Context, item Item) error
}
```

**Capture error contract:**
| Return         | Engine action                          |
|----------------|----------------------------------------|
| `nil`          | Item marked as captured                |
| `error`        | Fail (in Capture API) or failAndReleaseLocks (in Prepare) |

**Prepare error contract:**
| Return         | Engine action                          |
|----------------|----------------------------------------|
| `nil`          | Item prepared, ready for commit        |
| `error`        | Fail transaction, release locks        |

**PreCommitCheck error contract:**
| Return         | Engine action                          |
|----------------|----------------------------------------|
| `nil`          | Proceed with commit                    |
| `Skippable`    | Fail transaction, no rollback          |
| other `error`  | Fail transaction, no rollback          |

PreCommitCheck errors are always fatal to the transaction — if you can't
verify the world hasn't changed, you can't safely commit.

**Forward executor error contract:**
| Return         | Engine action                          |
|----------------|----------------------------------------|
| `nil`          | Step committed                         |
| `Skippable`    | Fail transaction, no rollback          |
| other `error`  | Fail step, trigger rollback            |

**Reverse executor error contract:**
| Return         | Engine action                          |
|----------------|----------------------------------------|
| `nil`          | Step rolled back                       |
| `Skippable`    | Record reason, move to next item       |
| other `error`  | Check RetrySignal → retry or stop      |

**Forget error contract:**
| Return         | Engine action                          |
|----------------|----------------------------------------|
| `nil`          | Item's executor-side resources cleaned up |
| `error`        | Log warning, continue. Non-fatal.      |

Forget is best-effort. Failure means orphaned data (rollback envelopes,
undo logs, backup files) but does not affect transaction correctness.

Error types are **instructions to the engine**, not descriptions of what happened.
The executor tells the engine what to *do*; the error message is for humans.

`Skippable` subsumes both conflict detection and fatal/unrecoverable errors.
Both mean "this item won't be completed, move on, human attention required."

### RetrySignal

```go
type RetrySignal interface {
    ShouldRetry() bool
}
```

Polled by the engine at the start of a retry Step() call (not in a sleep loop).
- `true` → engine attempts the retry, returns backoff duration if it fails again
- `false` → engine treats the item as Skippable, moves on

Binary signal: keep trying or stop. The engine does not decide when to give up —
the user does (via this signal) or the timeout does.

K8s adapter: implements as "does the retry-rollback annotation still exist?"
Library consumers: channel, callback, flag, always-true, always-false — their choice.

Context cancellation is deliberately excluded from retry control. A consumer can
build ctx-to-RetrySignal if they want, but the engine doesn't conflate infrastructure
shutdown with retry policy. (PostgreSQL parallel: prepared state is detachable from
the originating session — the user can walk away, the engine keeps going.)

### LockManager

Advisory locking per resource. String keys, opaque lock references.

```go
type LockManager interface {
    Acquire(ctx context.Context, key string, holder string, timeout time.Duration) (lockRef string, err error)
    Release(ctx context.Context, lockRef string, holder string) error
    ReleaseAll(ctx context.Context, lockRefs []string, holder string) error
    Renew(ctx context.Context, lockRef string, holder string, timeout time.Duration) error
}
```

- `key`: opaque string identifying the resource to lock. K8s adapter uses
  "Namespace/Kind/Name". Database adapter might use "table/row".
- `holder`: transaction ID. Same holder can re-acquire idempotently.
- `lockRef`: returned by Acquire, stored in ItemState.LockRef.

K8s adapter: Lease-based (current implementation, wrapping string keys).

## Conflict Detection — Layered Design

Conflict detection is executor-owned, engine-enforced.

**Why the executor owns the logic:**
The engine can't do meaningful conflict detection in the general case. K8s uses
resource versions; databases use row versions or checksums; file systems use
modification times. The engine would need to pass opaque blobs through a generic
interface — at which point it's just calling a function the executor could call itself.

**Why the engine enforces the protocol step:**
Conflict detection is part of the saga protocol. The engine guarantees it happens
before every commit by calling PreCommitCheck. A consumer can't accidentally skip it.

**Why PreCommitCheck exists (not in standard XA):**
In XA, prepare and commit happen in quick succession within the same session.
In our model, arbitrary time can pass between prepare and commit (one-item-per-call,
caller-driven loop, possible crash recovery). PreCommitCheck bridges that gap.

**Layering:**
1. **Engine**: calls PreCommitCheck before each commit, interprets the result
2. **Janus K8s library**: provides a default PreCommitCheck that does resource
   version comparison (what `checkConflict` does today)
3. **Consumer**: uses the K8s default, overrides it, or composes their own

## Capture — Before-Image Timing

In ARIES, before-images are captured during operation logging. In XA, the RM
captures internally during prepare. Our design is more flexible — we allow the
before-image (Capture) to be taken at a user-controlled time.

**Two paths:**
1. **Early capture (user-initiated):** User calls `Capture()` API before sealing.
   Snapshots state at a time they control. Item starts with just an ID + order;
   Capture adds rollback data.
2. **Late capture (engine-initiated):** During Prepare, engine sees item hasn't
   been captured → calls executor's Capture, then proceeds with Prepare.

**Why Prepare is the right place for late capture (not earlier):**
In K8s, resource versions change constantly (controllers, webhooks, status updates).
A resource version from when the user ran `kubectl get` is guaranteed stale by
prepare time. The lock is what makes the captured state meaningful — once you
hold the lock, you own the window between prepare and commit.

## Interfaces Eliminated

### CheckpointStore — eliminated

The stateless design eliminates this. The engine returns new state; the caller
persists it however they want. The engine doesn't need to know how state is stored.

### RollbackStore — caller's responsibility

Not an engine interface. The caller is responsible for providing rollback data
(e.g., prior state) to the reverse executor. How that data is saved/loaded is
outside the engine's concern.

Janus may provide a default RollbackStore implementation in a future version,
but the engine doesn't depend on it.

### StateReader — eliminated

Reading current state is the executor's responsibility, not the engine's.
Conflict detection is performed by the executor via PreCommitCheck. The engine
doesn't need to read external state directly.

## Guarantees to Consumers

Regardless of backend:
1. **Atomicity** — all items commit or all committed items are rolled back
2. **Isolation** — locks prevent concurrent modification during the transaction window
3. **Crash resilience** — progress is durable; recovery resumes from last checkpoint
   (maps to XA's xa_recover + ARIES CLR pattern)
4. **Ordering** — execute in declared order, rollback in reverse
5. **Conflict detection** — PreCommitCheck enforced before every commit; executor
   provides backend-appropriate implementation
6. **Semantic compensation** — reverse is not required to restore exact prior state,
   only an acceptable approximation (per Garcia-Molina)

## Transaction Configuration

```
Timeout         time.Duration    // global deadline
LockTimeout     time.Duration    // per-lock expiry
AutoRollback    bool             // rollback on forward failure
RetryRollback   bool             // enable retry with exponential backoff
```

AutoRollback and RetryRollback map to annotations in the CRD adapter.
The engine reads these from the transaction state, not from K8s-specific mechanisms.

RetryRollback is a **binary control signal**, not a policy parameter. It means
"keep trying" or "stop." The engine retries with exponential backoff until:
1. The reverse executor succeeds
2. The user flips RetryRollback to false (via RetrySignal)
3. The global Timeout expires

The decision to stop retrying is always the user's or the timeout's, never the
engine's heuristic. Rollback failure is on the knife edge of requiring human
intervention — the engine doesn't try to be clever about it.

## Concrete Types

### State

```go
type State struct {
    Phase       Phase
    Items       []ItemState
    Config      Config
    StartedAt   *time.Time
    CompletedAt *time.Time
}

type Config struct {
    Timeout      time.Duration
    LockTimeout  time.Duration
    AutoRollback bool
}

type ItemState struct {
    ID         string   // stable, unique within transaction
    Order      int      // sort key: (Order, ID) for forward, reverse for rollback
    LockKey    string   // opaque, passed to LockManager.Acquire
    Captured   bool
    Prepared   bool
    Committed  bool
    RolledBack bool
    Error      string   // last error, for diagnostics
    LockRef    string   // opaque, returned by LockManager.Acquire
}
```

Design notes:
- **No Version field.** Optimistic concurrency for checkpoints is the caller's
  concern, not the engine's.
- **No rollback data.** Executor owns it, stored externally keyed by item ID.
  State is engine state, not executor state.
- **RetryRollback absent from Config.** Controlled by RetrySignal interface,
  not stored in state.
- **Item list immutability not enforced.** Caller's responsibility not to
  mutate items between Step() calls. Engine trusts what it receives.

### Result

```go
type Result struct {
    Terminal     bool
    RequeueAfter time.Duration
}
```

- `Terminal`: true when phase is Committed, RolledBack, or Failed with no more work.
- `RequeueAfter`: non-zero only during retry backoff. Zero means "call again immediately."
- Caller diffs old/new State for observability (logging, metrics, events).

### Phase

All eight current phases survive extraction:

```go
type Phase string

const (
    PhasePending     Phase = "Pending"
    PhasePreparing   Phase = "Preparing"
    PhasePrepared    Phase = "Prepared"
    PhaseCommitting  Phase = "Committing"
    PhaseCommitted   Phase = "Committed"
    PhaseRollingBack Phase = "RollingBack"
    PhaseRolledBack  Phase = "RolledBack"
    PhaseFailed      Phase = "Failed"
)
```

- **Pending**: initial state. First Step() transitions to Preparing.
- **Prepared**: meaningful protocol milestone — all locks held, all before-images
  captured. Observable between Preparing and Committing.
- **Failed**: single phase. "Failed with unrolled commits" vs "failed clean" is
  computed from item state (any Committed && !RolledBack), not a sub-phase.

### Dependencies

```go
type Dependencies struct {
    Executor    StepExecutor
    LockManager LockManager
    RetrySignal RetrySignal
}
```

### Observability

No observability interface in v1. The engine emits nothing — no logs, no metrics,
no events. The caller owns observability entirely via State diffs between Step() calls.

May add an optional observer callback in a future version if real users need it.

## Open Questions

None. All design items resolved.

## XA Mapping Reference

| XA Concept       | Our Equivalent              | Notes                           |
|------------------|-----------------------------|---------------------------------|
| `xa_start`       | Transaction creation        | Caller creates state with items |
| `xa_end`         | (implicit)                  | Work defined by items, not session |
| `xa_prepare`     | `Prepare` on executor       | RM-owned; includes Capture if needed |
| `xa_commit`      | `Forward` on executor       | Apply the mutation              |
| `xa_rollback`    | `Reverse` on executor       | Compensating transaction        |
| `xa_recover`     | Load checkpoint + Step()    | Stateless recovery              |
| `xa_forget`      | `Forget` on executor        | Per-item cleanup of executor resources |
| (none in XA)     | `Capture` API               | User-controlled before-image timing |
| (none in XA)     | `PreCommitCheck`            | Bridges prepare-commit time gap |

## Key Design Decisions Made

- **Stateless engine.** Step() takes full state, returns new state. Caller owns
  persistence. Crash recovery = load last checkpoint + call Step().
- **Separate Capture API.** Before-image acquisition can happen at user-controlled
  time (not in XA). Uncaptured items are auto-captured during Prepare.
- **Six-method executor.** Capture, Prepare, PreCommitCheck, Forward, Reverse,
  Forget. Maps to XA's RM interface with extensions for our use case.
- **Error types are control flow signals.** The type tells the engine what to do;
  the message tells humans what happened.
- **RetrySignal over context cancellation.** More precise, separates infrastructure
  lifecycle from retry policy.
- **Caller-driven loop.** Engine is a step function. Returns backoff durations,
  doesn't sleep. Composable with any event loop.
- **Rollback failure requires human intervention.** The engine either retries
  (when signaled) or stops and preserves state for debugging. It never silently
  gives up.
- **Minimal interface surface.** Only StepExecutor, RetrySignal, and LockManager.
  CheckpointStore, RollbackStore, and StateReader eliminated by pushing
  responsibility to the caller and executor.
- **Conflict detection is executor-owned, engine-enforced.** The engine defines
  PreCommitCheck as a protocol step and guarantees it's called. The executor
  implements backend-specific conflict detection. Janus provides a K8s default
  (resource version comparison) that's easy to override.
- **Semantic compensation per Garcia-Molina.** Reverse is not required to restore
  exact prior state — only an acceptable approximation.
- **Prepare is the right time for late capture.** Lock makes the captured state
  meaningful. Resource versions between user intent and prepare are guaranteed
  stale in K8s.
- **String lock keys.** Opaque strings for LockManager keys, opaque lock refs
  returned by Acquire. No structured key type — backend decides the format.
- **Minimal Result type.** Terminal + RequeueAfter only. Caller diffs State for
  observability. No Action enum.
- **All eight phases retained.** Pending, Preparing, Prepared, Committing,
  Committed, RollingBack, RolledBack, Failed. All carry protocol meaning.
- **No Version on State.** Checkpoint concurrency is the caller's concern.
- **No observability in v1.** Engine emits nothing. Caller diffs State.

## References

- Garcia-Molina & Salem, "Sagas" (1987) — ACM SIGMOD
- X/Open XA / DTP specification (1991)
- Jim Gray & Reuter, "Transaction Processing: Concepts and Techniques" (1993)
- ARIES: Mohan et al., "ARIES: A Transaction Recovery Method" (1992)
- Jakarta Transactions 2.0 specification
- PostgreSQL Two-Phase Transactions documentation
