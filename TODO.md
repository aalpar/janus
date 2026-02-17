# TODO — Prometheus Metrics (PR #8)

## P0

- [ ] Fix dead `ItemCount.Observe` in `handlePending` (transaction_controller.go:228-230)
  - `StartedAt` is set on line 225, then nil-checked on line 228 — always false
  - Observe unconditionally, or capture `wasNew := txn.Status.StartedAt == nil` before setting

- [ ] Move metric recording after successful `Status().Update()` in `transition` and `setFailed`
  - `recordPhaseChange` fires at line 648 before `Status().Update` at line 652
  - On conflict retry, counters double-count and gauge drifts negative
  - Same pattern in `setFailed`: metrics at lines 670-672, write at line 686
  - Capture old phase before mutation, write status, then record metrics on success

## P1

- [ ] Fix `ReleaseAll` bulk error counting (lines 348-353, 416-421, 692-697)
  - Partial failure adds `len(leaseRefs)` to error counter even if most succeeded
  - Change to per-call `Add(1)` or iterate releases individually

- [ ] Fix negative `ActiveTransactions` gauge for explicit "Pending" phase
  - `recordPhaseChange("Pending", "Preparing")` decrements "Pending" but it was never incremented
  - Only happens when `Status.Phase == "Pending"` (not `""`)
  - Either increment "Pending" in the normalization path, or skip decrement for untracked phases

## P2

- [ ] Clean up unrelated changes bundled in PR #8
  - Impersonation Groups removal (`internal/impersonate/client.go`)
  - RBAC list/watch on serviceaccounts (`config/rbac/role.yaml`, controller RBAC annotation)
  - staticcheck suppression in `cmd/main.go`
  - Stray `godebug` dependency in `go.mod`
  - Either document in PR body or split into separate commit
