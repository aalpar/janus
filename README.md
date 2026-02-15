# Janus

A Kubernetes operator that executes atomic multi-resource changes using the
[Saga pattern](https://dl.acm.org/doi/10.1145/38714.38742) with
Lease-based advisory locking.

## How It Works

Janus defines a single CRD — `Transaction` — containing an ordered list of
resource mutations (create, update, patch, delete). The controller processes
them as a Saga: each step is paired with a compensating action so the entire
sequence can be rolled back on failure.

```
┌──────────────────────────────────────────────────────────┐
│  Transaction CR                                          │
│  spec.changes: [{target, type, content}, ...]            │
└────────────────────────┬─────────────────────────────────┘
                         │
                         ▼
┌──────────────────────────────────────────────────────────┐
│  TransactionReconciler (state machine)                   │
│                                                          │
│  Pending ──► Preparing ──► Prepared ──► Committing       │
│                                           │     │        │
│                                           │     ▼        │
│                                           │  Committed   │
│                                           ▼              │
│                                      RollingBack         │
│                                        │     │           │
│                                        ▼     ▼           │
│                                   RolledBack Failed      │
└────────────┬─────────────────────────────────────────────┘
             │ uses
             ▼
┌──────────────────────────────────────────────────────────┐
│  Lock Manager (internal/lock)                            │
│  Acquire/Release Lease objects (coordination.k8s.io/v1)  │
│  Advisory: available to all, enforced by convention      │
└──────────────────────────────────────────────────────────┘
```

**Prepare phase** — For each resource: acquire a Lease lock, read current
state into a rollback ConfigMap. This builds the compensating actions the
Saga needs if it must abort.

**Commit phase** — For each resource: verify the lock is still held, apply
the mutation. If any step fails, the Saga reverses through committed items
in reverse order, restoring each from the rollback ConfigMap.

One item is processed per reconcile cycle. Progress is persisted to the API
server after each item, so the controller can resume from where it left off
after a crash.

## Example

```yaml
apiVersion: backup.janus.io/v1alpha1
kind: Transaction
metadata:
  name: deploy-v2
spec:
  lockTimeout: 5m
  changes:
    - target:
        apiVersion: v1
        kind: ConfigMap
        name: app-config
      type: Patch
      content:
        data:
          version: "2.0"

    - target:
        apiVersion: apps/v1
        kind: Deployment
        name: web-server
      type: Patch
      content:
        spec:
          template:
            spec:
              containers:
                - name: web
                  image: myapp:v2.0

    - target:
        apiVersion: v1
        kind: Secret
        name: old-api-key
      type: Delete
```

If the Deployment patch fails, the ConfigMap patch is automatically reverted
to its prior state.

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+
- kubectl version v1.11.3+
- Access to a Kubernetes v1.11.3+ cluster

### Deploy

```sh
make docker-build docker-push IMG=<some-registry>/janus:tag
make install
make deploy IMG=<some-registry>/janus:tag
```

### Try it out

```sh
kubectl apply -k config/samples/
```

### Uninstall

```sh
kubectl delete -k config/samples/
make uninstall
make undeploy
```

## References

See [BIBLIOGRAPHY](BIBLIOGRAPHY) for the foundational papers on Sagas,
two-phase commit, and atomic commitment protocols.

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
