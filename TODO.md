# TODO

## Future

- **Per-item rollback ConfigMaps**: Replace single rollback ConfigMap with one ConfigMap per before-image. Each is written atomically, has its own resourceVersion for integrity verification, and removes the ~1.5MB transaction size limit. Store ConfigMap refs + RVs in Transaction status for verification during rollback. OwnerRef'd to Transaction for GC.
- **Consolidate rollback state into Transaction CR**: For small transactions, eliminate the rollback ConfigMap entirely by storing before-images in the Transaction CR status. Atomic committed + committedRV + rollback state in a single status update. Constrained by etcd's ~1.5MB resource size limit — per-item ConfigMaps are the unbounded alternative.
