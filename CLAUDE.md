# Janus

## Versioning

- Janus is at v0.0.1 with zero consumers. Break freely — no stability guarantees until real users exist.
- API group: `tx.janus.io/v1alpha1`

## Architecture

Two CRDs: `Transaction` and `ResourceChange` — Saga pattern with Lease-based advisory locking.

### State Machine

```
Pending → Preparing → Prepared → Committing → Committed
                                      ↓
                              RollingBack ← (failure/timeout/deletion)
                                      ↓
                              RolledBack | Failed
```

- `spec.sealed` triggers processing (Pending → Preparing)
- One item per reconcile cycle for crash resilience
- Timeout detection: overall txn timeout → RollingBack (if commits exist) or Failed (if none)

### CRDs

**Transaction** — orchestrator CR. Key spec fields: `serviceAccountName`, `sealed`, `lockTimeout`, `timeout`. Supports `metadata.generateName` for server-generated names. Status tracks `phase`, `version` (stale-write detection), `items[]` (per-resource progress), `rollbackRef` (ConfigMap name).

**ResourceChange** — individual mutation. Key spec fields: `target` (apiVersion/kind/name/namespace), `type` (Create|Update|Patch|Delete), `content` (manifest/patch JSON), `order` (execution sequence). Grouped under Transaction via OwnerReferences; sorted by `(spec.order, name)`.

### Key Design Decisions

- **SSA for Patch operations** — field manager per transaction name; idempotent on re-commit
- **RV-based conflict detection** — Update/Delete check resourceVersion at commit against prepare-time snapshot; self-write retry on "object modified" errors
- **Rollback storage** — OwnerRef'd ConfigMap (`{txn}-rollback`), keyed by `{kind}_{namespace}_{name}`. Contains `Envelope` per item with prior state + captured RV. All rollback paths verify current RV before applying — no silent overwrites
- **Lease-based advisory locking** — expire on timeout; non-cooperative actors not blocked. Locks are per-resource in target namespace
- **SA impersonation** — resource ops execute under user-specified ServiceAccount identity; cached per `namespace/saName` with `sync.Map` + `sync.Once`

### Annotations & Finalizers

- `tx.janus.io/automatic-rollback` — present by default; remove to skip rollback on Transaction deletion
- `tx.janus.io/retry-rollback` — one-shot trigger; controller removes after attempt
- `tx.janus.io/lease-cleanup` — controller-managed finalizer; stripped in terminal states
- `tx.janus.io/rollback-protection` — controller adds, never removes; user strips to allow deletion

### Webhooks

Two validating webhooks (no mutating):
- **Transaction** — cannot unseal; spec immutable once sealed or non-Pending
- **ResourceChange** — target fields required; content required for Create/Update/Patch, forbidden for Delete

## Package Map

| Package | Key Types | Purpose |
|---------|-----------|---------|
| `api/v1alpha1/` | Transaction, ResourceChange, ItemStatus | CRD definitions + webhook validators |
| `internal/controller/` | TransactionReconciler | State machine, orchestration (~1400 lines) |
| `internal/controller/errors.go` | ResourceOpError, RollbackDataError, ErrConflictDetected, ErrRollbackConflict | Typed errors at boundaries |
| `internal/lock/` | Manager (interface), LeaseManager | Advisory locking; ErrAlreadyLocked, ErrLockExpired |
| `internal/impersonate/` | NewClient | SA impersonation client factory |
| `internal/rollback/` | Envelope, Meta | Rollback ConfigMap schema |
| `internal/recover/` | Plan, PlanItem, ApplyItem | Offline recovery CLI logic |
| `internal/metrics/` | (counters, histograms, gauges) | Prometheus: phase transitions, duration, active txns, item ops, lock ops |
| `internal/scheme/` | Scheme | Kubernetes scheme with CRDs registered |
| `cmd/controller/` | main | Controller manager entry point |
| `cmd/janus/` | main | CLI: `janus create\|add\|seal\|recover` |

## Error Handling

- Typed errors at boundaries: `ResourceOpError`, `ErrConflictDetected`, `ErrRollbackConflict`, `RollbackDataError` (controller); `ErrAlreadyLocked`, `ErrLockExpired`, `LeaseOpError` (lock)
- Sentinels internally: `errUnknownChangeType`, `errSelfWrite`
- No `fmt.Errorf` in production — always project error types with Unwrap()
- Pattern: `&ResourceOpError{Op: "fetching", Ref: ref.String(), Err: err}`

## Testing

- **Unit/integration**: Ginkgo v2 + envtest (`internal/controller/transaction_controller_test.go`, ~4600 lines)
- **E2E**: Kind (default) or k0s cluster; tests create/patch/delete/multi-item/rollback/bad-SA scenarios
- **After changes**: `make lint && make && make test`

## Commits

- Direct push to master is fine at this stage
