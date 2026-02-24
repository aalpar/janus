# Janus

## Versioning

- Janus is at v0.0.1 with zero consumers. Break freely — no stability guarantees until real users exist.
- API group: `tx.janus.io/v1alpha1`

## Architecture

- Two CRDs: `Transaction` and `ResourceChange` — Saga pattern with Lease-based advisory locking
- Transaction groups ResourceChanges via OwnerReferences; `spec.sealed` triggers processing
- ResourceChanges sorted by `(spec.order, name)` for deterministic execution
- One item per reconcile cycle for crash resilience
- Server-side apply for Patch operations (field manager per transaction)
- Rollback state in OwnerRef'd ConfigMap (auto-GC), keyed by ResourceChange CR name

## Commits

- Direct push to master is fine at this stage
