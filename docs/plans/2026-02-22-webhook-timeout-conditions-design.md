# Validation Webhook, Transaction Timeout, Status Conditions

Design for three features that improve Transaction safety, bounded execution,
and observability.

## 1. Validation Webhook

Validating admission webhook using kubebuilder's `webhook.CustomValidator`
interface with cert-manager for TLS.

### Validation Rules (CREATE)

| Rule | Rationale |
|------|-----------|
| `type` ∈ {Create, Update, Patch, Delete} | Shift-left; better error than reconcile-time failure |
| `content` required for Create/Update/Patch | Currently fails at commit time |
| `content` empty for Delete | Ignored content is a smell |
| `target.apiVersion`, `target.kind`, `target.name` non-empty | Fail fast |
| No duplicate targets (apiVersion+kind+name+namespace) | Would deadlock on lock acquisition |

### Validation Rules (UPDATE)

- All CREATE rules apply to the new spec.
- If `status.phase != "" && status.phase != "Pending"`, reject any `spec`
  change. Spec is immutable once processing begins.

### Infrastructure

- `api/v1alpha1/transaction_webhook.go` — `CustomValidator` implementation
- `config/webhook/` — `ValidatingWebhookConfiguration` manifests
- `config/certmanager/` — cert-manager `Certificate` + `Issuer`
- `config/default/kustomization.yaml` patches to wire webhook + cert-manager

## 2. Transaction-Level Timeout

### API Change

```go
// In TransactionSpec:
// Timeout bounds the overall transaction duration.
// Defaults to 30 minutes.
// +optional
Timeout *metav1.Duration `json:"timeout,omitempty"`
```

### Mechanism

Check at the top of `Reconcile`, before dispatching to phase handlers:

```
elapsed = now - status.startedAt
if elapsed > timeout:
    if hasUnrolledCommits(txn) → transition to RollingBack
    else                       → setFailed("transaction timed out")
```

### Design Decisions

- **Check location:** Top of Reconcile, not inside each handler. One check
  covers Preparing, Prepared, Committing, and RollingBack.
- **Default:** 30 minutes via `transactionTimeout()` helper (same pattern as
  `lockTimeout()`).
- **RollingBack is also subject to timeout.** A stuck rollback transitions
  straight to Failed — no infinite retry loop.
- **RequeueAfter:** Use `min(existing, timeRemaining)` so stuck transactions
  (no external events) still get checked before the deadline.
- **No new status field for timeout reason.** The Failed condition message
  is sufficient: `"transaction timed out after 30m0s"`.

## 3. Status Conditions Expansion

### Condition Types

| Condition | Set when | Status | Reason |
|-----------|----------|--------|--------|
| `Prepared` | Phase → Prepared | True | `AllItemsPrepared` |
| `Committed` | Phase → Committed | True | `AllItemsCommitted` |
| `RolledBack` | Phase → RolledBack | True | `RollbackComplete` |
| `Failed` | Phase → Failed | True | `TransactionFailed` |

### What We Skip

No conditions for Preparing, Committing, or RollingBack. These in-progress
states are observable via `.status.phase` and don't enable useful
`kubectl wait` workflows.

### Implementation

Add a `conditionForPhase(phase) *metav1.Condition` helper. `transition()`
calls it and sets the condition if non-nil.

`setFailed()` already sets the Failed condition with a custom message and
does not call `transition()`. Leave it as-is — no duplication risk since
Failed phase never goes through `transition()`.

Conditions are monotonic: once `Prepared=True`, it stays True even after
the phase advances. This is what makes `kubectl wait --for=condition=Prepared`
work.
