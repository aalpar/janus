# TODO

## Validation Webhook

Add an admission webhook that rejects malformed `Transaction` specs at
submission time rather than letting them fail during reconciliation.

Checks: valid `type` enum, `content` required for Create/Update/Patch,
target fields non-empty, no duplicate targets within a single transaction.

## Transaction-Level Timeout

`spec.lockTimeout` governs individual leases but nothing bounds the
overall transaction duration. A transaction stuck in Preparing or
Committing (rate-limited API, missing SA permissions) sits indefinitely.

Add a `spec.timeout` field. When elapsed, transition to RollingBack (if
any commits exist) or Failed (if none do). Use `status.startedAt` as the
reference time.

## Conflict Detection

Between prepare (read current state) and commit (apply mutation), another
actor can modify the resource. For Update this partially surfaces via
`resourceVersion` conflicts, but Patch and Delete are blind to
intervening changes.

Store `resourceVersion` during prepare. Before committing, verify the
resource's current `resourceVersion` matches. On mismatch, surface a
clear error and trigger rollback.

## Impersonate Package Tests

`internal/impersonate` has no test file. The `sync.Once` cache with
lazy initialization and error-triggered eviction is subtle enough to
warrant direct unit coverage.

## Status Conditions Expansion

`status.conditions` is only set on failure. Adding conditions for each
phase transition would enable `kubectl wait --for=condition=Prepared`
and improve observability for external tooling.
