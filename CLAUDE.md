# Janus

## Versioning

- Janus is at v0.0.1 with zero consumers. Break freely — no stability guarantees until real users exist.
- API group: `backup.janus.io/v1alpha1`

## Architecture

- Single CRD: `Transaction` — Saga pattern with Lease-based advisory locking
- One item per reconcile cycle for crash resilience
- Server-side apply for Patch operations (field manager per transaction)
- Rollback state in OwnerRef'd ConfigMap (auto-GC)

## Commits

- Direct push to master is fine at this stage
