# Janus

Field-level atomic changes across Kubernetes resources.

Janus extends Server-Side Apply's field ownership concept across resource
boundaries. Where SSA lets a field manager own fields within a single
resource, Janus lets a transaction own fields across multiple resources
in multiple namespaces — and move them together. If any step fails,
everything rolls back.

Janus composes with existing GitOps tools. ArgoCD owns the Deployment.
Flux owns the HelmRelease. Janus patches specific fields across both as
part of an operational change. No conflict — SSA field managers partition
ownership.

## Example

```sh
# Create a transaction
janus create db-migration --sa janus-ops

# Patch connection strings across namespaces
janus add db-migration --type Patch --target ConfigMap/app-config -n frontend \
  -f connection-patch.yaml
janus add db-migration --type Patch --target ConfigMap/app-config -n backend \
  -f connection-patch.yaml --order 1
janus add db-migration --type Patch --target Deployment/api-server -n backend \
  -f env-patch.yaml --order 2

# Seal to begin processing
janus seal db-migration
```

If the Deployment patch fails, both ConfigMap patches are automatically
reverted. Each resource's prior field values are captured before mutation
and restored on rollback.

```sh
kubectl get transaction db-migration
```
```
NAME            SEALED   PHASE       AGE
db-migration    true     Committed   34s
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

Patch operations use Server-Side Apply with a field manager scoped to the
transaction. This means Janus owns only the fields it touches — other
controllers and GitOps tools retain ownership of their fields on the same
resources.

Resources are locked via advisory Kubernetes Leases during the
transaction. Locks prevent concurrent Janus transactions from colliding
but do not block direct `kubectl` writes — this is a deliberate trade-off
to avoid admission webhook overhead.

Operations execute under a user-specified ServiceAccount identity, so
RBAC boundaries are enforced per transaction.

## When to use this

Operational changes that span multiple resources or namespaces, where
the intermediate state is dangerous and GitOps doesn't apply:

- **Cross-resource field patches** — update connection strings in 3
  ConfigMaps and an env var in 2 Deployments as one unit; rollback
  all if any fails
- **Cross-namespace coordination** — tenant provisioning, service mesh
  config, or infrastructure changes that cross namespace boundaries
- **Operational changes outside GitOps** — incident response, migrations,
  or infrastructure changes that aren't templated manifests in a repo
- **Security-sensitive intermediate states** — Namespace + RBAC +
  NetworkPolicy where a partial apply creates an unprotected window

## When not to use this

- **Single-resource changes** — just use `kubectl apply`.
- **Standard deployments** — if your changes are templated manifests in
  git, use Helm, ArgoCD, or Flux. Janus is for operational changes
  that live outside the GitOps lifecycle.
- **Workflow orchestration** — Janus applies resource mutations, not
  arbitrary jobs. Use Argo Workflows or Tekton for DAGs.
- **Workloads where eventual consistency is acceptable** — standard
  Kubernetes reconciliation is simpler and more idiomatic.

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
make deploy IMG=ghcr.io/aalpar/janus:v0.2.0
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
  name: janus-ops
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: janus-ops-edit
  namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: edit
subjects:
- kind: ServiceAccount
  name: janus-ops
  namespace: default
```

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
