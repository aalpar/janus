# generateName Support for Transactions

## Problem

Transaction creation requires an explicit name. This blocks programmatic
consumers (restore pipelines, Wile compositions) that want server-generated
names, and adds friction for interactive CLI users who don't care about the
name.

## Scope

The controller already works with generateName — it uses UID-based owner
reference filtering (`listChanges` at `transaction_controller.go:1217`) and
computes all name-derived values (rollback ConfigMap name, SSA field manager)
post-creation from `txn.Name`. No controller changes needed.

The work is in the CLI (`cmd/janus/main.go`) and docs.

## Design

### 1. `janus create`: add `--generate-name` flag

Add `--generate-name` / `-g` flag accepting a prefix string. Make it mutually
exclusive with the positional name argument — exactly one must be provided.

When `--generate-name` is set, populate `ObjectMeta.GenerateName` instead of
`ObjectMeta.Name`. The API server appends a random suffix and returns the
resolved name.

### 2. `janus create`: stdout/stderr output split

Change output convention for all creation modes:

- **stdout**: resolved name only (e.g. `restore-x7k2f`)
- **stderr**: human-readable message (e.g. `Transaction default/restore-x7k2f created (unsealed)`)

This makes `TXN=$(janus create ...)` work uniformly:

```bash
# Named
TXN=$(janus create my-txn --sa my-sa)

# Generated
TXN=$(janus create -g restore- --sa my-sa)

# Both cases: subsequent commands use $TXN
janus add "$TXN" --type Patch --target ConfigMap/foo -f patch.yaml
janus seal "$TXN"
```

### 3. `janus recover`: use `txn.Status.RollbackRef`

Both `runRecoverPlan` and `runRecoverApply` hardcode rollback ConfigMap name
derivation:

```go
rbCMName := txnName + "-rollback"
```

This duplicates the controller's logic (`transaction_controller.go:372`) and
would break if the naming convention ever changed.

Fix: read `txn.Status.RollbackRef` from the fetched Transaction CR. Fall back
to the hardcoded derivation only when the Transaction CR is not found (the
recover flow already handles this — it proceeds from ConfigMap only).

### 4. `janus add`: use fetched name for RC auto-naming

`runAdd` line 209 uses the CLI argument `txnName` for auto-generating
ResourceChange names:

```go
rcName = fmt.Sprintf("%s-%s-%s", txnName, ...)
```

These are identical in practice (the GET would fail if they differed), but use
`txn.Name` from the fetched object for consistency.

### 5. Documentation

Update `docs/USER_GUIDE.md` to show both creation modes side by side.

## Changes

| File | Function | Change |
|------|----------|--------|
| `cmd/janus/main.go` | `runCreate` | Add `--generate-name`/`-g`, mutual exclusion, stdout/stderr split |
| `cmd/janus/main.go` | `runAdd` | Use `txn.Name` for RC auto-name |
| `cmd/janus/main.go` | `runRecoverPlan` | Use `txn.Status.RollbackRef` with fallback |
| `cmd/janus/main.go` | `runRecoverApply` | Same |
| `docs/USER_GUIDE.md` | CLI examples | Add generateName examples |

## What doesn't change

- **Controller**: already compatible (UID-based ownership, post-creation name resolution)
- **`janus add`**: takes resolved name as positional arg, works as-is
- **`janus seal`**: same
- **CRD schema**: no changes
- **Tests**: e2e tests create Transactions via raw YAML, not the CLI
