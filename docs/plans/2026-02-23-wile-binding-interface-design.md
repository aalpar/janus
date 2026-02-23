# Wile Binding Interface Design

## Context

Janus provides transactional Kubernetes operations (Saga pattern, lease-based
locking, rollback). Wile is an R7RS Scheme implementation in Go. Together they
form a "SQL + 2PC for Scheme and Kubernetes": Wile handles composition, filtering,
and control flow; Janus handles transactional execution, crash recovery, and
rollback.

## Decisions

- **Thin primitives first.** The Go binding layer exposes minimal CRUD + lifecycle
  functions. Composition, batching, and query helpers are built in Scheme on top.
  Rich batch interfaces can be added later without breaking changes.
- **Explicit transaction handle.** `janus-begin` returns a handle passed to every
  operation. No implicit state via context or dynamic parameters.
- **CLI execution model.** Wile runs as a standalone CLI tool. The user writes a
  Scheme script, the CLI embeds a Wile Engine with Janus bindings registered,
  evaluates the script, and exits.
- **Accumulate then submit.** Mutations accumulate in the handle in memory.
  `janus-prepare` submits the full Transaction CR to the cluster in one shot.
  This enables future optimization (dependency analysis, batching) without
  changing the Scheme-side API.

## Type Mapping

Wile's FFI auto-converts Go structs to Scheme alists and vice versa. Janus types
appear naturally:

```
Go struct                          Scheme alist
──────────────────────────────────────────────────────
ResourceRef{                       '((APIVersion . "v1")
  APIVersion: "v1",                   (Kind . "ConfigMap")
  Kind:       "ConfigMap",            (Name . "my-config")
  Name:       "my-config",           (Namespace . "default"))
  Namespace:  "default",
}
```

Raw struct-accepting functions live in Go. Convenience constructors (e.g., `ref`)
live in a Scheme library `(janus core)`.

## Binding Interface

### Naming Convention

- `janus-*` — transactional operations (owned by Janus)
- `kube-*` — raw Kubernetes reads (not Janus-specific)

### Transaction Lifecycle (5 functions)

| Scheme | Go signature | Notes |
|---|---|---|
| `(janus-begin name ns sa)` | `func(ctx, string, string, string) (TxnHandle, error)` | Creates handle in memory |
| `(janus-prepare txn)` | `func(ctx, Value) error` | Submits Transaction CR, blocks until Prepared |
| `(janus-commit txn)` | `func(ctx, Value) error` | Blocks until Committed |
| `(janus-rollback txn)` | `func(ctx, Value) error` | Blocks until RolledBack |
| `(janus-status txn)` | `func(ctx, Value) (StatusResult, error)` | Returns phase + item statuses |

### Resource Mutations (4 functions)

These accumulate in the handle. No API call until `janus-prepare`.

| Scheme | Go signature | Notes |
|---|---|---|
| `(janus-create txn target content)` | `func(Value, ResourceRef, string) error` | ChangeType: Create |
| `(janus-update txn target content)` | `func(Value, ResourceRef, string) error` | ChangeType: Update |
| `(janus-patch txn target content)` | `func(Value, ResourceRef, string) error` | ChangeType: Patch |
| `(janus-delete txn target)` | `func(Value, ResourceRef) error` | ChangeType: Delete |

`target` is a ResourceRef alist. `content` is a raw JSON string.

### Resource Reads (2 functions)

Non-transactional. Direct Kubernetes API calls.

| Scheme | Go signature | Notes |
|---|---|---|
| `(kube-get api-ver kind name ns)` | `func(ctx, string, string, string, string) (map[string]any, error)` | Returns resource as alist |
| `(kube-list api-ver kind ns selector)` | `func(ctx, string, string, string, string) ([]map[string]any, error)` | selector is a label selector string |

## Transaction Handle

```go
type TxnHandle struct {
    Name           string
    Namespace      string
    ServiceAccount string
    Changes        []ResourceChange
    LockTimeout    time.Duration
    Timeout        time.Duration
}
```

Maps directly to `TransactionSpec`. Passed through the FFI as an opaque
`wile.Value` — not converted to alist on each call.

## Error Handling

Go errors become Scheme exceptions via Wile's FFI. Three categories for
programmatic handling:

| Predicate | Meaning | Typical response |
|---|---|---|
| `janus-conflict-error?` | Resource modified externally between prepare and commit | Retry or abort |
| `janus-timeout-error?` | Transaction or lock timeout exceeded | Extend or abort |
| `janus-error?` | Everything else (RBAC, network, invalid ref) | Abort |

```scheme
(guard (exn
        ((janus-conflict-error? exn) ...)
        ((janus-timeout-error? exn) ...))
  (janus-prepare txn)
  (janus-commit txn))
```

## Synchronous Waiting

`janus-prepare` and `janus-commit` block until the controller completes the
phase transition (via watch/poll). `context.Context` flows through Wile's FFI
automatically — ctrl-C cancels the context, terminates the watch, raises an
exception.

## Go Registration

```go
func RegisterJanusBindings(engine *wile.Engine, client kubernetes.Interface) error {
    b := &bindings{client: client}
    return engine.RegisterFuncs(map[string]any{
        "janus-begin":    b.begin,
        "janus-prepare":  b.prepare,
        "janus-commit":   b.commit,
        "janus-rollback": b.rollback,
        "janus-status":   b.status,
        "janus-create":   b.create,
        "janus-update":   b.update,
        "janus-patch":    b.patch,
        "janus-delete":   b.delete,
        "kube-get":       b.get,
        "kube-list":      b.list,
    })
}
```

## Architecture

```
┌──────────────────────────────────────────────────┐
│  user-script.scm               (user's script)   │
├──────────────────────────────────────────────────┤
│  (janus core)                  (Scheme library)   │
│  ref, alist-ref*, etc.         convenience layer  │
├──────────────────────────────────────────────────┤
│  11 Go functions               (binding layer)    │
│  RegisterJanusBindings         ~300 lines Go      │
├──────────────────────────────────────────────────┤
│  Wile Engine                   (Scheme VM)        │
│  FFI auto-conversion           struct <-> alist   │
├──────────────────────────────────────────────────┤
│  Janus Controller              (existing)         │
│  Transaction CR, leases,       reconcile loop     │
│  rollback, phase machine                          │
├──────────────────────────────────────────────────┤
│  Kubernetes API                                   │
└──────────────────────────────────────────────────┘
```

## Future Evolution

1. **Scheme composition library** — transducers (SRFI-171) for post-fetch
   transformation, applicative combinators for declaring independent operations.
2. **Rich batch interface** — `(janus-transact program)` that accepts an
   operation graph. Janus analyzes for parallelism. Built on same primitives.
3. **Query DSL** — `(kube-query ...)` with predicate pushdown to label/field
   selectors. Built in Scheme on top of `kube-list`.
