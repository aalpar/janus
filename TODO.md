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

## Status Conditions Expansion

`status.conditions` is only set on failure. Adding conditions for each
phase transition would enable `kubectl wait --for=condition=Prepared`
and improve observability for external tooling.
