# Tutorial

This tutorial walks through Janus hands-on. You'll create transactions,
watch them execute, trigger a rollback, and recover from failures — using
both the `janus` CLI and raw `kubectl` YAML.

**Time:** ~15 minutes with a running cluster.

## Prerequisites

You need a Kubernetes cluster with Janus installed.

**Cluster.** Any v1.30+ cluster works. For local development:

```sh
kind create cluster
```

**cert-manager.** Required for webhook TLS:

```sh
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait --for=condition=Available deployment -n cert-manager --all --timeout=60s
```

**Janus operator:**

```sh
helm install janus oci://ghcr.io/aalpar/janus/chart --namespace janus-system --create-namespace
```

Or from a local checkout:

```sh
make install
make run  # runs the controller locally
```

**Janus CLI:**

```sh
go install github.com/aalpar/janus/cmd/janus@latest
```

Or from source: `make build && ./bin/janus`

**ServiceAccount.** Janus runs operations under a user-specified SA:

```sh
kubectl apply -f config/samples/rbac.yaml
```

This creates a `janus-txn` ServiceAccount in `default` with the `edit`
ClusterRole — enough for everything in this tutorial.

---

## Part 1: Your first transaction (CLI)

We'll create a ConfigMap, then use a Janus transaction to patch it.

### Set up a target resource

```sh
kubectl create configmap app-config \
  --from-literal=version=1.0 \
  --from-literal=log_level=info
```

Verify:

```sh
kubectl get configmap app-config -o yaml
```

You should see `version: "1.0"` and `log_level: info` under `data`.

### Build the transaction

Janus uses a three-step workflow: **create**, **add** changes, **seal**.

```sh
# 1. Create an unsealed transaction
janus create my-first-txn --sa janus-txn
```

```
my-first-txn
Transaction default/my-first-txn created (unsealed)
```

The transaction exists but the controller ignores it until sealed. This
lets you compose changes from multiple commands before committing.

```sh
# 2. Add a patch
cat > /tmp/patch.yaml <<'EOF'
data:
  version: "2.0"
  log_level: debug
  feature_flag: enabled
EOF

janus add my-first-txn --type Patch --target ConfigMap/app-config -f /tmp/patch.yaml
```

```
ResourceChange default/my-first-txn-patch-app-config created (type=Patch, target=ConfigMap/app-config, order=0)
```

```sh
# 3. Seal to begin processing
janus seal my-first-txn
```

```
Transaction default/my-first-txn sealed
```

### Watch it run

```sh
kubectl get transaction my-first-txn -w
```

```
NAME             SEALED   PHASE       AGE
my-first-txn     true     Preparing   0s
my-first-txn     true     Prepared    0s
my-first-txn     true     Committing  0s
my-first-txn     true     Committed   1s
```

The phases reflect the Saga lifecycle: lock resources and snapshot prior
state (Preparing), then apply mutations (Committing). Press Ctrl-C to
stop watching.

### Verify the result

```sh
kubectl get configmap app-config -o jsonpath='{.data}' | jq .
```

```json
{
  "feature_flag": "enabled",
  "log_level": "debug",
  "version": "2.0"
}
```

The original `version` and `log_level` values were merged (Patch uses
server-side apply), and the new `feature_flag` field was added.

### Inspect the transaction

```sh
kubectl describe transaction my-first-txn
```

This shows Events — phase transitions, per-item progress, lock
operations. Useful for debugging when things go wrong.

### Clean up Part 1

Janus adds a `rollback-protection` finalizer to every transaction. This
prevents accidental deletion (and loss of rollback data). To delete a
transaction, first remove the finalizer:

```sh
kubectl patch transaction my-first-txn --type=merge \
  -p '{"metadata":{"finalizers":null}}'
kubectl delete transaction my-first-txn
kubectl delete configmap app-config
```

Deleting the Transaction also deletes its ResourceChange CRs (via
OwnerReferences).

---

## Part 2: The same thing with kubectl

Everything the `janus` CLI does, you can do with `kubectl` and YAML.
The CLI is a convenience — it handles OwnerReference wiring and the
seal patch for you.

### Create the target

```sh
kubectl create configmap app-config \
  --from-literal=version=1.0 \
  --from-literal=log_level=info
```

### Create the Transaction

```sh
kubectl apply -f - <<'EOF'
apiVersion: tx.janus.io/v1alpha1
kind: Transaction
metadata:
  name: kubectl-txn
  namespace: default
spec:
  serviceAccountName: janus-txn
EOF
```

Note: `sealed` defaults to `false`, so the controller ignores this
until we seal it.

### Get the Transaction UID

ResourceChange CRs need an OwnerReference with the Transaction's UID.
The CLI does this automatically; with kubectl, you fetch it:

```sh
TXN_UID=$(kubectl get transaction kubectl-txn -o jsonpath='{.metadata.uid}')
echo "$TXN_UID"
```

### Create the ResourceChange

```sh
kubectl apply -f - <<EOF
apiVersion: tx.janus.io/v1alpha1
kind: ResourceChange
metadata:
  name: kubectl-txn-patch-config
  namespace: default
  ownerReferences:
    - apiVersion: tx.janus.io/v1alpha1
      kind: Transaction
      name: kubectl-txn
      uid: ${TXN_UID}
spec:
  target:
    apiVersion: v1
    kind: ConfigMap
    name: app-config
  type: Patch
  content:
    data:
      version: "2.0"
      log_level: debug
      feature_flag: enabled
EOF
```

### Seal the Transaction

```sh
kubectl patch transaction kubectl-txn --type=merge -p '{"spec":{"sealed":true}}'
```

Once sealed, the spec is immutable and the controller begins processing.

### Watch and verify

```sh
kubectl get transaction kubectl-txn -w
```

Same progression as before: Preparing → Prepared → Committing →
Committed. Verify with:

```sh
kubectl get configmap app-config -o jsonpath='{.data}' | jq .
```

### When to use kubectl vs. the CLI

The **CLI** is better for scripting and interactive use — it handles
UID wiring, auto-generates ResourceChange names, and guesses
`apiVersion` from the Kind.

**kubectl YAML** is better for GitOps workflows where you want
transactions defined declaratively in version control. The trade-off is
manually managing UIDs (or using a templating tool like Kustomize).

### Clean up Part 2

```sh
kubectl patch transaction kubectl-txn --type=merge \
  -p '{"metadata":{"finalizers":null}}'
kubectl delete transaction kubectl-txn
kubectl delete configmap app-config
```

---

## Part 3: Multi-resource transaction

Real transactions touch multiple resources. This demo patches an nginx
stack atomically: resize a PVC, create a Service, and update a
Deployment — all as one unit.

### Set up the demo environment

```sh
kubectl apply -f config/samples/demo_nginx_setup.yaml
```

This creates:
- `janus-txn` ServiceAccount + RoleBinding (if not already present)
- `nginx-data` PersistentVolumeClaim (5Gi)
- `nginx` Deployment (1 replica, nginx:1.26)

Verify:

```sh
kubectl get deploy nginx -o jsonpath='{.spec.template.spec.containers[0].image}'
# nginx:1.26
```

### Run the transaction

The demo script builds a three-item transaction with ordering:

```sh
./config/samples/demo_nginx_txn.sh
```

Or step through it manually:

```sh
janus create nginx-patch --sa janus-txn

# Order 0: resize PVC to 10Gi
cat > /tmp/pvc.yaml <<'EOF'
spec:
  resources:
    requests:
      storage: 10Gi
EOF
janus add nginx-patch --type Patch --target PersistentVolumeClaim/nginx-data \
  -f /tmp/pvc.yaml --order 0

# Order 0: create a Service (same order = same batch)
cat > /tmp/svc.yaml <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: nginx
  namespace: default
spec:
  type: ClusterIP
  selector:
    app: nginx
  ports:
    - name: http
      port: 80
      targetPort: 8080
EOF
janus add nginx-patch --type Create --target Service/nginx \
  -f /tmp/svc.yaml --order 0

# Order 1: update Deployment image and replicas
cat > /tmp/deploy.yaml <<'EOF'
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: nginx
          image: nginx:1.27-alpine
          ports:
            - containerPort: 8080
EOF
janus add nginx-patch --type Patch --target Deployment/nginx \
  -f /tmp/deploy.yaml --order 1

janus seal nginx-patch
```

### Watch the ordering

```sh
kubectl get transaction nginx-patch -w
```

Items within order 0 (PVC patch and Service create) execute first. The
order 1 Deployment patch runs only after all order-0 items succeed.

Check per-item progress:

```sh
kubectl get transaction nginx-patch -o jsonpath='{range .status.items[*]}{.name}: prepared={.prepared} committed={.committed}{"\n"}{end}'
```

### Verify

```sh
kubectl get deploy nginx -o jsonpath='{.spec.template.spec.containers[0].image}'
# nginx:1.27-alpine

kubectl get svc nginx
# NAME    TYPE        CLUSTER-IP     EXTERNAL-IP   PORT(S)   AGE
# nginx   ClusterIP   10.96.x.x     <none>        80/TCP    10s
```

### Clean up Part 3

```sh
kubectl patch transaction nginx-patch --type=merge \
  -p '{"metadata":{"finalizers":null}}'
kubectl delete transaction nginx-patch
kubectl delete svc nginx --ignore-not-found
```

---

## Part 4: Rollback

What happens when a step fails? Janus automatically rolls back all
previously committed changes in reverse order.

### Reset the demo

Re-apply the setup to restore the Deployment:

```sh
kubectl apply -f config/samples/demo_nginx_setup.yaml
```

### Break something

Delete the Deployment so the order-1 Patch will fail:

```sh
kubectl delete deploy nginx
```

### Run the transaction again

```sh
./config/samples/demo_nginx_txn.sh
```

### Watch the rollback

```sh
kubectl get transaction nginx-patch -w
```

```
NAME          SEALED   PHASE         AGE
nginx-patch   true     Preparing     0s
nginx-patch   true     Committing    1s
nginx-patch   true     RollingBack   2s
nginx-patch   true     RolledBack    3s
```

What happened:
1. Order-0 items (PVC patch + Service create) prepared and committed
   successfully.
2. Order-1 Deployment patch failed — the Deployment doesn't exist.
3. Janus entered RollingBack: the Service was deleted (compensating
   the Create) and the PVC was restored to 5Gi (compensating the Patch).

### Verify the rollback

```sh
# Service should be gone (rolled back)
kubectl get svc nginx
# Error from server (NotFound): services "nginx" not found

# PVC should be back at 5Gi
kubectl get pvc nginx-data -o jsonpath='{.spec.resources.requests.storage}'
# 5Gi
```

Check `kubectl describe transaction nginx-patch` for Events showing
each rollback step.

### Clean up Part 4

```sh
kubectl patch transaction nginx-patch --type=merge \
  -p '{"metadata":{"finalizers":null}}'
kubectl delete transaction nginx-patch --ignore-not-found
```

---

## Part 5: Rolling back and recovery

A committed transaction can be rolled back via kubectl, and `janus
recover` handles the cases where rollback conflicts with external
modifications.

### Set up two ConfigMaps

```sh
kubectl create configmap cm-alpha \
  --from-literal=color=red \
  --from-literal=size=small

kubectl create configmap cm-beta \
  --from-literal=env=staging \
  --from-literal=debug=off
```

Verify:

```sh
kubectl get configmap cm-alpha -o jsonpath='{.data}'
# {"color":"red","size":"small"}

kubectl get configmap cm-beta -o jsonpath='{.data}'
# {"debug":"off","env":"staging"}
```

### Run a transaction that patches both

```sh
cat > /tmp/alpha-patch.yaml <<'EOF'
data:
  color: blue
  size: large
EOF

cat > /tmp/beta-patch.yaml <<'EOF'
data:
  env: production
  debug: "on"
EOF

janus create recover-demo --sa janus-txn
janus add recover-demo --type Patch --target ConfigMap/cm-alpha -f /tmp/alpha-patch.yaml
janus add recover-demo --type Patch --target ConfigMap/cm-beta -f /tmp/beta-patch.yaml
janus seal recover-demo
```

Wait for it to commit:

```sh
kubectl get transaction recover-demo -w
```

```
NAME           SEALED   PHASE       AGE
recover-demo   true     Committed   1s
```

Verify the patches were applied:

```sh
kubectl get configmap cm-alpha -o jsonpath='{.data}'
# {"color":"blue","size":"large"}

kubectl get configmap cm-beta -o jsonpath='{.data}'
# {"debug":"on","env":"production"}
```

### Request rollback via kubectl

To undo a committed transaction, add the `request-rollback` annotation:

```sh
kubectl annotate transaction recover-demo tx.janus.io/request-rollback=true
```

The controller consumes the annotation and transitions the transaction
to RollingBack. Watch it:

```sh
kubectl get transaction recover-demo -w
```

```
NAME           SEALED   PHASE         AGE
recover-demo   true     RollingBack   0s
recover-demo   true     RolledBack    1s
```

Both ConfigMaps are restored to their pre-transaction values:

```sh
kubectl get configmap cm-alpha -o jsonpath='{.data}'
# {"color":"red","size":"small"}

kubectl get configmap cm-beta -o jsonpath='{.data}'
# {"debug":"off","env":"staging"}
```

That's the clean case — no external modifications, no conflicts.

### Set up again for the conflict scenario

Re-apply the patches so we can demonstrate what happens when rollback
hits a conflict:

```sh
kubectl patch transaction recover-demo --type=merge \
  -p '{"metadata":{"finalizers":null}}'
kubectl delete transaction recover-demo
kubectl delete configmap cm-alpha cm-beta

kubectl create configmap cm-alpha \
  --from-literal=color=red \
  --from-literal=size=small

kubectl create configmap cm-beta \
  --from-literal=env=staging \
  --from-literal=debug=off

cat > /tmp/alpha-patch.yaml <<'EOF'
data:
  color: blue
  size: large
EOF

cat > /tmp/beta-patch.yaml <<'EOF'
data:
  env: production
  debug: "on"
EOF

janus create recover-demo --sa janus-txn
janus add recover-demo --type Patch --target ConfigMap/cm-alpha -f /tmp/alpha-patch.yaml
janus add recover-demo --type Patch --target ConfigMap/cm-beta -f /tmp/beta-patch.yaml
janus seal recover-demo
```

Wait for `Committed`, then continue.

### Simulate an external modification

Imagine another team member or controller modifies `cm-beta` after the
transaction committed:

```sh
kubectl patch configmap cm-beta --type=merge -p '{"data":{"debug":"verbose"}}'
```

Now `cm-beta` has a different `resourceVersion` than what Janus recorded.
`cm-alpha` is untouched.

### Preview the recovery plan

```sh
janus recover plan recover-demo
```

```
Transaction: recover-demo (namespace: default)

Rollback plan:
  - [pending]  RESTORE ConfigMap/default/cm-alpha (recover-demo-patch-cm-alpha)
  - [pending]  RESTORE ConfigMap/default/cm-beta (recover-demo-patch-cm-beta)
```

Both items show as `[pending]` — committed but not rolled back.

### Apply the recovery

```sh
janus recover apply recover-demo
```

```
  - RESTORE ConfigMap/default/cm-alpha (recover-demo-patch-cm-alpha) ... OK
  - RESTORE ConfigMap/default/cm-beta (recover-demo-patch-cm-beta) ... CONFLICT (stored RV 12345, current RV 12350)

1 item(s) failed. Re-run with --force to override conflicts.
```

`cm-alpha` was restored to its prior state (`color: red`, `size: small`).
`cm-beta` was skipped — its `resourceVersion` changed since the
transaction committed, meaning someone else modified it. Janus refuses
to silently overwrite their changes.

### Verify the partial recovery

```sh
kubectl get configmap cm-alpha -o jsonpath='{.data}'
# {"color":"red","size":"small"}    ← restored

kubectl get configmap cm-beta -o jsonpath='{.data}'
# {"debug":"verbose","env":"production"}    ← still has external modification
```

### Force past the conflict

You've checked `cm-beta` and decided the pre-transaction state is what
you want. Use `--force` to override the conflict:

```sh
janus recover apply recover-demo --force
```

```
  - RESTORE ConfigMap/default/cm-alpha (recover-demo-patch-cm-alpha) ... OK
  - RESTORE ConfigMap/default/cm-beta (recover-demo-patch-cm-beta) ... OK

All pending items applied successfully.
```

### Verify full recovery

```sh
kubectl get configmap cm-alpha -o jsonpath='{.data}'
# {"color":"red","size":"small"}

kubectl get configmap cm-beta -o jsonpath='{.data}'
# {"debug":"off","env":"staging"}    ← restored to pre-transaction state
```

Both ConfigMaps are back to their original values.

### Clean up Part 5

```sh
kubectl patch transaction recover-demo --type=merge \
  -p '{"metadata":{"finalizers":null}}'
kubectl delete transaction recover-demo
kubectl delete configmap cm-alpha cm-beta
```

---

## Cleanup

Remove everything created during this tutorial:

```sh
# Remove finalizers from any remaining transactions
for txn in my-first-txn kubectl-txn nginx-patch recover-demo; do
  kubectl patch transaction "$txn" --type=merge \
    -p '{"metadata":{"finalizers":null}}' 2>/dev/null
done

# Delete all tutorial resources
kubectl delete transaction my-first-txn kubectl-txn nginx-patch recover-demo --ignore-not-found
kubectl delete -f config/samples/demo_nginx_teardown.yaml --ignore-not-found
kubectl delete configmap app-config cm-alpha cm-beta --ignore-not-found
```

---

## Next steps

- **[User Guide](USER_GUIDE.md)** — complete reference for all change
  types, ordering, namespaces, monitoring, and failure modes.
- **[Design](DESIGN.md)** — architecture, state machine, lock manager,
  and rollback storage internals.
- **[Invariants](INVARIANTS.md)** — safety and liveness guarantees.
