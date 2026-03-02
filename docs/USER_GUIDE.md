# Writing Transactions

This guide covers writing Transaction CRs, monitoring their progress, and
handling failures.

## What Janus does

Janus coordinates multi-resource changes in Kubernetes. When you need to
update a ConfigMap, roll out a new Deployment image, and delete a stale
Secret as a single logical operation, Janus ensures that either all changes
succeed or all committed changes are reverted.

It does this using the [Saga pattern][saga] — a technique from database
theory for long-lived transactions. A Saga breaks a large operation into a
sequence of steps, each paired with a **compensating action** that undoes
it. If the sequence fails at step *n*, the compensating actions for steps
*n-1* through *1* run in reverse order, restoring the prior state.

In Janus, each step is a Kubernetes resource mutation (create, update,
patch, or delete), and the compensating action is automatically derived:
creating a resource is compensated by deleting it, patching is compensated
by restoring the prior field values, and so on. You define the steps; Janus
builds and executes the compensations.

[saga]: https://dl.acm.org/doi/10.1145/38714.38742 "Garcia-Molina & Salem, 'Sagas', ACM SIGMOD 1987"

## Prerequisites

Janus executes resource operations under a ServiceAccount identity you
specify. Before creating a Transaction, set up the SA and grant it
permissions on the resources you want to mutate.

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: janus-txn
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: janus-txn-edit
  namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: edit
subjects:
- kind: ServiceAccount
  name: janus-txn
  namespace: default
```

The `edit` ClusterRole is a convenient default — it grants read/write on
most namespaced resources. For production, scope permissions to only the
resource types the transaction needs.

> A complete sample is at [`config/samples/rbac.yaml`](../config/samples/rbac.yaml).

## Workflow

Janus uses a two-phase workflow: first you build the transaction, then you
seal it to trigger processing. This lets you compose a transaction from
multiple kubectl commands or API calls before committing to execution.

### 1. Create the Transaction

```sh
janus create deploy-v2 --sa janus-txn [-n default]
```

Or with a server-generated name:

```sh
TXN=$(janus create -g deploy- --sa janus-txn [-n default])
```

The resolved name is printed to stdout, making it easy to capture for
subsequent commands:

```sh
TXN=$(janus create -g deploy- --sa janus-txn)
janus add "$TXN" --type Patch --target ConfigMap/app-config -f patch.yaml
janus seal "$TXN"
```

Or with kubectl:

```yaml
apiVersion: tx.janus.io/v1alpha1
kind: Transaction
metadata:
  name: deploy-v2
  namespace: default
spec:
  serviceAccountName: janus-txn
  lockTimeout: 5m
  timeout: 15m
```

The Transaction starts unsealed. The controller ignores it until sealed.

### 2. Add changes

```sh
janus add deploy-v2 --type Patch --target ConfigMap/app-config -f config-patch.yaml [-n default]
janus add deploy-v2 --type Patch --target Deployment/web-server -f deploy-patch.yaml --order 1
janus add deploy-v2 --type Delete --target Secret/old-api-key --order 2
```

Each `janus add` creates a ResourceChange CR with an OwnerReference
pointing at the Transaction. You can also create ResourceChange CRs
directly with kubectl — just set the ownerReference to the Transaction's
UID.

### 3. Seal

```sh
janus seal deploy-v2 [-n default]
```

Or with kubectl:

```sh
kubectl patch transaction deploy-v2 --type=merge -p '{"spec":{"sealed":true}}'
```

Once sealed, the Transaction's spec is immutable and the controller begins
processing. Sealing cannot be reversed.

## Transaction anatomy

A Transaction groups ResourceChange CRs under a common identity and
controls when processing begins (via the `sealed` field).

```yaml
apiVersion: tx.janus.io/v1alpha1
kind: Transaction
metadata:
  name: deploy-v2
  namespace: default
spec:
  serviceAccountName: janus-txn   # Required. SA must exist in this namespace.
  lockTimeout: 5m                 # Optional. Per-resource lock TTL (default: 5m).
  timeout: 30m                    # Optional. Overall transaction deadline (default: 30m).
  sealed: true                    # Set to true to begin processing.
```

### Spec fields

| Field | Required | Default | Description |
|---|---|---|---|
| `serviceAccountName` | Yes | — | SA identity for all resource operations. Must exist in the Transaction's namespace. |
| `sealed` | No | `false` | When true, the controller begins processing. Immutable once set. |
| `lockTimeout` | No | `5m` | How long each resource's advisory lock is held before expiring. |
| `timeout` | No | `30m` | Overall deadline. If elapsed, the transaction rolls back (if any items were committed) or fails directly. |

## ResourceChange anatomy

Each mutation is a separate ResourceChange CR owned by the Transaction.

```yaml
apiVersion: tx.janus.io/v1alpha1
kind: ResourceChange
metadata:
  name: deploy-v2-patch-config
  namespace: default
  ownerReferences:
    - apiVersion: tx.janus.io/v1alpha1
      kind: Transaction
      name: deploy-v2
      uid: <transaction-uid>
spec:
  target:
    apiVersion: v1
    kind: ConfigMap
    name: app-config
  type: Patch
  order: 0
  content:
    data:
      version: "2.0"
```

### Spec fields

| Field | Required | Description |
|---|---|---|
| `target.apiVersion` | Yes | Group/version of the target resource (e.g. `v1`, `apps/v1`). |
| `target.kind` | Yes | Resource kind (e.g. `ConfigMap`, `Deployment`). |
| `target.name` | Yes | Resource name. |
| `target.namespace` | No | Target namespace. Defaults to the Transaction's namespace. |
| `type` | Yes | Mutation type: `Create`, `Update`, `Patch`, or `Delete`. |
| `content` | Depends | Required for Create, Update, Patch. Must be empty for Delete. |
| `order` | No | Execution sequence. Lower values execute first. Same value = same batch (sequential today, parallel in future). Default: 0. |

## Change types

### Create

Creates a new resource. `content` must be a **full resource manifest**
including `apiVersion`, `kind`, and `metadata`.

```yaml
spec:
  target:
    apiVersion: v1
    kind: ConfigMap
    name: new-app-config
  type: Create
  content:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: new-app-config
    data:
      database_url: "postgres://db:5432/myapp"
      log_level: "info"
```

On rollback, the created resource is deleted.

If the resource already exists when Create runs (e.g. retried after a
crash), the operation is treated as a no-op.

### Update

Replaces the target resource entirely. `content` must be a **full resource
manifest** — every field you want to keep must be present, because the
entire object is overwritten.

```yaml
spec:
  target:
    apiVersion: v1
    kind: ConfigMap
    name: app-config
  type: Update
  content:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: app-config
    data:
      database_url: "postgres://db:5432/myapp"
      log_level: "info"
```

On rollback, the prior state (captured during prepare) is restored via
`client.Update`.

Update uses optimistic concurrency: the `resourceVersion` captured during
prepare is set on the object before writing. If the resource was modified
externally between prepare and commit, the API server rejects the write
with a conflict error.

### Patch

Merges fields into an existing resource using **server-side apply**. Only
specify the fields you want to change — existing fields are preserved.

```yaml
spec:
  target:
    apiVersion: v1
    kind: ConfigMap
    name: app-config
  type: Patch
  content:
    data:
      log_level: "debug"
      feature_flag: "enabled"
```

`content` for Patch is a **partial object** — no `apiVersion`, `kind`, or
`metadata` needed. Just the fields to merge.

On rollback, a reverse patch is computed: for each field the forward patch
touched, the prior value is restored via server-side apply. Fields that
were added by the patch (not present before) are removed.

Patch uses the field manager `janus-{transactionName}`, so it coexists
cleanly with other controllers (HPA, etc.) that own different fields.

### Delete

Deletes the target resource. `content` must be empty.

```yaml
spec:
  target:
    apiVersion: v1
    kind: Secret
    name: old-api-key
  type: Delete
```

On rollback, the resource is re-created from the state captured during
prepare.

If the resource is already gone when Delete runs, the operation is a no-op.

## Ordering

ResourceChanges are sorted by `spec.order` (ascending), then by name
(alphabetically) within the same order value. Rollback reverses that
order.

Think about this when assigning order values:

- Resources that others depend on should have lower order values (created
  first, deleted last during rollback).
- If a change at order 2 fails, changes at order 1 and 0 are rolled
  back — in that order.
- Changes with the same order value are currently processed sequentially
  (sorted by name). A future version may process same-order changes in
  parallel.

Each item is processed in its own reconcile cycle. The controller persists
progress after every item, so a crash between items loses zero work.

## Namespaces

`target.namespace` defaults to the Transaction's own namespace when
omitted. You can target resources in other namespaces:

```yaml
spec:
  target:
    apiVersion: v1
    kind: ConfigMap
    name: shared-config
    namespace: other-namespace
  type: Patch
  content:
    data:
      key: "value"
```

The ServiceAccount must have permissions in the target namespace for this
to work.

## Monitoring a transaction

### Phase

```sh
kubectl get transaction deploy-v2
```
```
NAME        SEALED   PHASE       AGE
deploy-v2   true     Committing  12s
```

The phase tells you where the transaction is in its lifecycle:

| Phase | Meaning |
|---|---|
| `Pending` | Unsealed — waiting for changes to be added and the transaction to be sealed. |
| `Preparing` | Acquiring locks and snapshotting prior state, one item at a time. |
| `Prepared` | All items locked and snapshotted. Transitions to Committing immediately. |
| `Committing` | Applying mutations, one item at a time. |
| `Committed` | All mutations applied. Terminal — done. |
| `RollingBack` | Reversing committed changes in reverse order. |
| `RolledBack` | All committed changes reversed. Terminal. |
| `Failed` | Something went wrong. Terminal (but see [Recovery](#recovery)). |

### Per-item status

```sh
kubectl get transaction deploy-v2 -o jsonpath='{range .status.items[*]}{@}{"\n"}{end}'
```

Each item in `status.items[]` has:

| Field | Meaning |
|---|---|
| `name` | Name of the ResourceChange CR this status tracks. |
| `prepared` | Lock acquired and prior state captured. |
| `committed` | Mutation applied. |
| `rolledBack` | Committed change was reverted. |
| `error` | Error message if this item failed. |

### Conditions

```sh
kubectl get transaction deploy-v2 -o jsonpath='{.status.conditions}'
```

Conditions are set at significant milestones:

| Condition | Set when |
|---|---|
| `Prepared` | All items have been prepared. |
| `Committed` | All items have been committed. |
| `RolledBack` | All committed items have been rolled back. |
| `Failed` | The transaction failed (message contains the reason). |

### Events

```sh
kubectl describe transaction deploy-v2
```

The controller emits Kubernetes Events for phase transitions, per-item
progress, lock operations, and errors. These appear in `kubectl describe`
output and can be forwarded to your logging system.

## Understanding failures

### Conflict detection

If a resource is modified by something outside Janus between prepare and
commit, the `resourceVersion` won't match. The transaction fails with a
conflict error rather than silently overwriting external changes.

```
item deploy-v2-patch-config: ConfigMap/app-config was modified externally since prepare
```

**What to do:** Check what modified the resource. Re-create the
Transaction once the external change is settled.

### Lock contention

If another Janus transaction holds a lock on a resource you need, lock
acquisition fails and the transaction is marked Failed.

```
item deploy-v2-patch-config lock failed: resource already locked by transaction "other-txn"
```

**What to do:** Wait for the other transaction to complete, or check
whether it's stuck (timed out locks auto-expire after `lockTimeout`).

### ServiceAccount errors

If the SA doesn't exist or lacks permissions:

```
ServiceAccount "missing-sa" not found in namespace "default"
```

**What to do:** Create the SA and ensure it has a RoleBinding granting
access to the target resources.

### Timeout

If the transaction exceeds `spec.timeout`:

- If any items were committed: the transaction transitions to
  `RollingBack` to undo committed changes.
- If no items were committed: the transaction goes directly to `Failed`.

```
transaction timed out after 30m0s, rolling back
```

**What to do:** Investigate why the transaction was slow (lock contention?
slow API server?). Increase `spec.timeout` if the workload legitimately
takes longer.

### Rollback failures

During rollback, if a resource was modified externally since Janus wrote
it, the rollback detects a conflict and skips that item:

```
item deploy-v2-patch-config: rollback conflict — manual intervention required via janus recover
```

Items that can't be rolled back are left in a `committed` but not
`rolledBack` state. The transaction lands in `Failed` with the rollback
ConfigMap preserved for manual recovery.

## Recovery

### Automatic recovery

When the controller encounters a `Failed` transaction that has un-rolled-back
commits **and** the rollback ConfigMap still exists, it automatically
retries by transitioning back to `RollingBack`. This handles transient
errors (e.g. a brief API server unavailability during rollback).

No action required — the controller retries on each reconcile.

### Rolling back a committed transaction

A committed transaction can be rolled back by adding the
`request-rollback` annotation:

```sh
kubectl annotate transaction deploy-v2 tx.janus.io/request-rollback=true
```

The controller consumes the annotation and transitions the transaction
from `Committed` to `RollingBack`. The standard rollback machinery
handles the rest — RV conflict detection, per-item progress, and the
`Failed`-with-conflicts terminal state if any items can't be reverted.

If all items roll back cleanly, the transaction reaches `RolledBack`.
If any items conflict (the resource was modified externally since
commit), those items are skipped and the transaction lands in `Failed`.
Use `janus recover` to handle the remaining items.

### Manual recovery with `janus recover`

For conflict cases where automatic rollback can't proceed (the resource was
modified externally), use the `janus recover` CLI.

**Preview the recovery plan:**

```sh
janus recover plan <transaction-name> [-n namespace]
```

This reads the rollback ConfigMap and the Transaction status, then prints
which items need recovery and what operation would be performed (restore,
delete, reverse-patch).

**Apply the recovery:**

```sh
janus recover apply <transaction-name> [-n namespace]
```

This executes the pending recovery operations. Items with
resourceVersion conflicts are skipped (reported as `CONFLICT`).

**Force past conflicts:**

```sh
janus recover apply <transaction-name> [-n namespace] --force
```

The `--force` flag overrides resourceVersion checks, applying the stored
prior state regardless of what the resource currently looks like. Use this
when you've verified the current state is wrong and the rollback state is
what you want.

> `janus recover` works from the rollback ConfigMap, so it can operate even
> if the Transaction CR has been deleted.

## Multi-operation example

A realistic transaction that updates config, rolls out a new image, and
cleans up a stale secret — with rollback safety across all three:

```sh
# Create the transaction
janus create deploy-v2 --sa janus-txn

# Add changes with ordering
janus add deploy-v2 --type Patch --target ConfigMap/app-config \
  -f config-patch.yaml

janus add deploy-v2 --type Patch --target Deployment/web-server \
  -f deploy-patch.yaml --order 1

janus add deploy-v2 --type Delete --target Secret/old-api-key \
  --order 2

# Seal to begin processing
janus seal deploy-v2
```

With a generated name:

```sh
TXN=$(janus create -g deploy-v2- --sa janus-txn)

janus add "$TXN" --type Patch --target ConfigMap/app-config \
  -f config-patch.yaml

janus add "$TXN" --type Patch --target Deployment/web-server \
  -f deploy-patch.yaml --order 1

janus add "$TXN" --type Delete --target Secret/old-api-key \
  --order 2

janus seal "$TXN"
```

Or equivalently with kubectl (showing the ResourceChange YAML):

```yaml
# Transaction
apiVersion: tx.janus.io/v1alpha1
kind: Transaction
metadata:
  name: deploy-v2
spec:
  serviceAccountName: janus-txn
  lockTimeout: 5m
  timeout: 15m
---
# After creating the Transaction and reading its UID:

# 1. Update application config (order 0)
apiVersion: tx.janus.io/v1alpha1
kind: ResourceChange
metadata:
  name: deploy-v2-patch-config
  ownerReferences:
    - apiVersion: tx.janus.io/v1alpha1
      kind: Transaction
      name: deploy-v2
      uid: <transaction-uid>
spec:
  target:
    apiVersion: v1
    kind: ConfigMap
    name: app-config
  type: Patch
  content:
    data:
      version: "2.0"
      feature_flags: "new-ui=true"
---
# 2. Roll out new image (order 1)
apiVersion: tx.janus.io/v1alpha1
kind: ResourceChange
metadata:
  name: deploy-v2-patch-deploy
  ownerReferences:
    - apiVersion: tx.janus.io/v1alpha1
      kind: Transaction
      name: deploy-v2
      uid: <transaction-uid>
spec:
  target:
    apiVersion: apps/v1
    kind: Deployment
    name: web-server
  type: Patch
  order: 1
  content:
    spec:
      template:
        spec:
          containers:
            - name: web
              image: myapp:v2.0
---
# 3. Remove stale secret (order 2)
apiVersion: tx.janus.io/v1alpha1
kind: ResourceChange
metadata:
  name: deploy-v2-delete-secret
  ownerReferences:
    - apiVersion: tx.janus.io/v1alpha1
      kind: Transaction
      name: deploy-v2
      uid: <transaction-uid>
spec:
  target:
    apiVersion: v1
    kind: Secret
    name: old-api-key
  type: Delete
  order: 2
```

If the Deployment patch (order 1) fails, the ConfigMap patch (order 0) is
automatically reverted. If the Secret delete (order 2) fails, both order 1
and order 0 are reverted.
