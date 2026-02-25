# Janus

## Versioning

- Janus is at v0.0.1 with zero consumers. Break freely — no stability guarantees until real users exist.
- API group: `tx.janus.io/v1alpha1`

## Architecture

- Two CRDs: `Transaction` and `ResourceChange` — Saga pattern with Lease-based advisory locking
- Transaction groups ResourceChanges via OwnerReferences; `spec.sealed` triggers processing
- ResourceChanges sorted by `(spec.order, name)` for deterministic execution
- One item per reconcile cycle for crash resilience
- Server-side apply for Create/Patch operations (field manager per transaction)
- Rollback state in OwnerRef'd ConfigMap (auto-GC), keyed by ResourceChange CR name
- Saga correctness via detection + compensation, not enforced locking
- SSA Create/Patch skip RV conflict checks on commit (idempotent); Update/Delete use RV-based checks with self-write detection
- All rollback paths check post-commit RV before applying — no silent overwrites

## Commits

- Direct push to master is fine at this stage
