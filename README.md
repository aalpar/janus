# Janus

Atomic multi-resource changes for Kubernetes.

Apply a group of resources together. If any step fails, everything that
was committed rolls back.

## Example

```sh
# Create a transaction
janus create deploy-v2 --sa janus-txn

# Add changes (ordered by --order, default 0)
janus add deploy-v2 --type Patch --target ConfigMap/app-config -f config-patch.yaml
janus add deploy-v2 --type Patch --target Deployment/web-server -f deploy-patch.yaml --order 1
janus add deploy-v2 --type Delete --target Secret/old-api-key --order 2

# Seal to begin processing
janus seal deploy-v2
```

If the Deployment patch fails, the ConfigMap patch is automatically
reverted. If the Secret delete fails, both prior changes are reverted.

```sh
kubectl get transaction deploy-v2
```
```
NAME        SEALED   PHASE       AGE
deploy-v2   true     Committed   34s
```

## How it works

Janus implements the [Saga pattern](https://dl.acm.org/doi/10.1145/38714.38742)
— each resource mutation is paired with a compensating action derived
automatically from the prior state. If the sequence fails at step *n*,
steps *n-1* through *1* are undone in reverse order.

Two CRDs:
- **Transaction** — groups changes under a common identity, controls when
  processing begins (`spec.sealed`), and tracks progress.
- **ResourceChange** — a single mutation (Create, Update, Patch, or Delete)
  owned by a Transaction.

Resources are locked via advisory Kubernetes Leases during the
transaction. Locks prevent concurrent Janus transactions from colliding
but do not block direct `kubectl` writes — this is a deliberate trade-off
to avoid admission webhook overhead.

Operations execute under a user-specified ServiceAccount identity, so
RBAC boundaries are enforced per transaction.

## Install

### Prerequisites
- Kubernetes v1.30+
- kubectl v1.30+
- [cert-manager](https://cert-manager.io/docs/installation/) (for webhook TLS)

### Helm (recommended)

```sh
helm install janus oci://ghcr.io/aalpar/janus/chart --namespace janus-system --create-namespace
```

Or from source:

```sh
helm install janus chart/ --namespace janus-system --create-namespace
```

### Container image

Pre-built multi-arch images (amd64 + arm64) are published to
`ghcr.io/aalpar/janus` on every release.

### Kustomize

```sh
make install
make deploy IMG=ghcr.io/aalpar/janus:v0.0.1
```

### Install the CLI

```sh
go install github.com/aalpar/janus/cmd/janus@latest
```

Or download a binary from the
[Releases](https://github.com/aalpar/janus/releases) page.

### Set up a ServiceAccount

Janus runs resource operations under a ServiceAccount you specify. Create
one with permissions on the resources your transactions will touch:

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

## When to use this

You have multiple Kubernetes resources that must change together, and a
partial update is worse than no update. Examples:

- ConfigMap + Deployment + Secret as a single deploy step
- Namespace + RBAC + NetworkPolicy where the intermediate state is a
  security gap
- Any coordinated create/update/delete where you want automatic rollback

## When not to use this

- Single-resource changes — just use `kubectl apply`.
- Workloads where eventual consistency is acceptable — standard
  Kubernetes reconciliation is simpler and more idiomatic.
- Workflow orchestration — Janus applies resource mutations, not
  arbitrary jobs. Use Argo Workflows or Tekton for DAGs.

## Documentation

- **[Tutorial](docs/TUTORIAL.md)** — hands-on walkthrough: create, rollback,
  and recover transactions using both the CLI and kubectl.
- **[User Guide](docs/USER_GUIDE.md)** — complete reference: change types, ordering,
  monitoring, failure handling, recovery.
- **[Design](docs/DESIGN.md)** — architecture, state machine, lock manager,
  rollback storage, reconciliation loop.
- **[Invariants](docs/INVARIANTS.md)** — safety and liveness guarantees.
- **[Contributing](CONTRIBUTING.md)** — how to build, test, and submit changes.
- **[Bibliography](BIBLIOGRAPHY.md)** — annotated references on Sagas,
  two-phase commit, and crash recovery.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
