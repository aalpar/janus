# TODO

## Next

- **`generateName` CLI support**: Add `--generate-name` / `-g` flag to `janus create`. Split stdout (resolved name only) / stderr (human messages). Update `runAdd` and `runRecover` to work with server-generated names. Plan at `docs/plans/2026-02-24-generate-name-plan.md`. Controller already compatible — CLI only.
- **EventRecorder API migration**: `cmd/controller/main.go:118` uses deprecated `mgr.GetEventRecorderFor()`. Migrate to modern `events.EventRecorder`.
- **Test coverage gaps**: `applyChange` at 58.6%, `handleDeletion` at 73.1%. Add edge cases for Delete-with-conflicts and deletion-with-protected-state paths.
- **CLI output convention uniformity**: Standardize stdout/stderr split across `add` and `seal` to match the convention established by `generateName`. Do together with generateName.
- **Documentation gaps**: Add deployment guide, troubleshooting section, and feature `janus recover` more prominently in main docs.

## Future

- **Per-item rollback ConfigMaps**: Replace single rollback ConfigMap with one ConfigMap per before-image. Each is written atomically, has its own resourceVersion for integrity verification, and removes the ~1.5MB transaction size limit. Store ConfigMap refs + RVs in Transaction status for verification during rollback. OwnerRef'd to Transaction for GC.
- **Consolidate rollback state into Transaction CR**: For small transactions, eliminate the rollback ConfigMap entirely by storing before-images in the Transaction CR status. Atomic committed + committedRV + rollback state in a single status update. Constrained by etcd's ~1.5MB resource size limit — per-item ConfigMaps are the unbounded alternative.
