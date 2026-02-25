#!/usr/bin/env bash
# Demo transaction: patches an nginx stack atomically.
#
# Prereqs: apply demo_nginx_setup.yaml first, and have the Janus operator running.
#
# Happy path (default):
#   ./demo_nginx_txn.sh
#   kubectl get transaction nginx-patch -w   # watch it complete
#
# This script is re-runnable: Create uses server-side apply, so if the
# Service already exists with matching values, it merges (idempotent).
#
# ── Break it ─────────────────────────────────────────────────────────
# After applying the setup, make one of these changes before running
# the transaction to trigger different rollback scenarios:
#
# 1. Service exists with conflicting values (Create SSA conflict → rollback)
#      kubectl create svc clusterip nginx --tcp=9999:9999 -n default
#
# 2. Deployment deleted (order-1 Patch fails → rolls back order-0 commits)
#      kubectl delete deploy nginx -n default
#
# 3. PVC deleted (order-0 Patch fails → rolls back any earlier commits)
#      kubectl delete pvc nginx-data -n default
# ─────────────────────────────────────────────────────────────────────
set -euo pipefail

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

TXN=nginx-patch
NS=default

# Clean up any previous transaction (Service persists — Create is idempotent).
kubectl delete transaction "$TXN" -n "$NS" --ignore-not-found

# ── Create the transaction ───────────────────────────────────────────

janus create "$TXN" --sa janus-txn -n "$NS"

# ── Order 0: PVC resize ─────────────────────────────────────────────

cat > "$TMPDIR/pvc.yaml" <<'EOF'
spec:
  resources:
    requests:
      storage: 10Gi
EOF

janus add "$TXN" -n "$NS" \
  --type Patch \
  --target PersistentVolumeClaim/nginx-data \
  -f "$TMPDIR/pvc.yaml" \
  --order 0

# ── Order 0: Create the Service ─────────────────────────────────────

cat > "$TMPDIR/svc.yaml" <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: nginx
  namespace: default
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "9113"
spec:
  type: ClusterIP
  selector:
    app: nginx
  ports:
    - name: http
      port: 80
      targetPort: 8080
    - name: metrics
      port: 9113
      targetPort: 9113
EOF

janus add "$TXN" -n "$NS" \
  --type Create \
  --target Service/nginx \
  -f "$TMPDIR/svc.yaml" \
  --order 0

# ── Order 1: Patch the Deployment ───────────────────────────────────

cat > "$TMPDIR/deploy.yaml" <<'EOF'
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: nginx
          image: nginx:1.27-alpine
          ports:
            - containerPort: 8080
          volumeMounts:
            - name: data
              mountPath: /usr/share/nginx/html
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: nginx-data
EOF

janus add "$TXN" -n "$NS" \
  --type Patch \
  --target Deployment/nginx \
  -f "$TMPDIR/deploy.yaml" \
  --order 1

# ── Seal and go ─────────────────────────────────────────────────────

janus seal "$TXN" -n "$NS"

echo ""
echo "Transaction sealed. Watch progress:"
echo "  kubectl get transaction $TXN -n $NS -w"
echo "  kubectl get resourcechange -n $NS -l tx.janus.io/transaction=$TXN"
