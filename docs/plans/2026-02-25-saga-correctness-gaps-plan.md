# Saga Correctness Gaps Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Fix three correctness gaps in Transaction commit and rollback paths.

**Architecture:** Three targeted changes to `transaction_controller.go` — (1) skip redundant `checkConflict` for SSA operations and add self-write detection for Update/Delete, (2) add RV-based conflict detection to SSA rollback paths, (3) detect external resource creation between prepare and commit. All tests use envtest with direct reconciler calls.

**Tech Stack:** Go, controller-runtime, envtest, Ginkgo/Gomega

---

### Task 1: Gap 3 — Detect external creation when `expectedRV == ""`

This is the simplest and most isolated change. Fix it first since it's a single check added to `handleCommitting`.

**Files:**
- Modify: `internal/controller/transaction_controller.go:528-592` (inside `handleCommitting` loop)
- Test: `internal/controller/transaction_controller_test.go`

**Step 1: Write the failing test**

Add a new `Context` block inside the `"conflict detection"` context (after line 2830).
The test creates a ConfigMap externally between prepare and commit for a Create operation, then verifies the transaction fails with a conflict.

```go
It("should detect conflict when resource is created externally between prepare and commit", func() {
    cmContent := map[string]any{
        "apiVersion": "v1",
        "kind":       "ConfigMap",
        "metadata":   map[string]any{"name": "ext-create-cm", "namespace": testNamespace},
        "data":       map[string]any{"key": "from-janus"},
    }
    raw, err := json.Marshal(cmContent)
    Expect(err).NotTo(HaveOccurred())

    txn := minimalTxn("ext-create-txn")
    Expect(k8sClient.Create(ctx, txn)).To(Succeed())
    rc := createChange("ext-create-txn", "ext-create-cm-change", backupv1alpha1.ResourceChangeSpec{
        Target: backupv1alpha1.ResourceRef{
            APIVersion: "v1",
            Kind:       "ConfigMap",
            Name:       "ext-create-cm",
            Namespace:  testNamespace,
        },
        Type:    backupv1alpha1.ChangeTypeCreate,
        Content: runtime.RawExtension{Raw: raw},
    }, txn.UID)
    Expect(k8sClient.Create(ctx, rc)).To(Succeed())

    reconcileToPhase(reconciler, "ext-create-txn", backupv1alpha1.TransactionPhasePrepared)

    // External actor creates the resource between prepare and commit.
    extCM := &corev1.ConfigMap{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "ext-create-cm",
            Namespace: testNamespace,
        },
        Data: map[string]string{"key": "from-external"},
    }
    Expect(k8sClient.Create(ctx, extCM)).To(Succeed())

    // Reconcile — should detect conflict and fail.
    for range 10 {
        Expect(k8sClient.Get(ctx, nn("ext-create-txn"), txn)).To(Succeed())
        if txn.Status.Phase == backupv1alpha1.TransactionPhaseFailed ||
            txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted {
            break
        }
        _, err := reconciler.Reconcile(ctx, req("ext-create-txn"))
        Expect(err).NotTo(HaveOccurred())
    }
    Expect(k8sClient.Get(ctx, nn("ext-create-txn"), txn)).To(Succeed())
    Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))

    // Clean up.
    Expect(k8sClient.Delete(ctx, extCM)).To(Succeed())
})
```

**Step 2: Run the test to verify it fails**

Run: `make test`
Expected: FAIL — the test expects `Failed` but gets `Committed` because `checkConflict` skips Create with empty RV.

**Step 3: Implement the fix**

In `handleCommitting` (line 550-559 area), add a check for Create-of-new-resource before calling `applyChange`. Insert this block after the `ns :=` line (550) and before the existing `checkConflict` call (552):

```go
// For Create: if the resource didn't exist at prepare time,
// verify it still doesn't before applying.
if change.Type == backupv1alpha1.ChangeTypeCreate && item.ResourceVersion == "" {
    _, err := r.getResource(ctx, userClient, change.Target, ns)
    if err == nil {
        // Resource was created externally between prepare and commit.
        txmetrics.ItemOperations.WithLabelValues("commit", "conflict").Inc()
        log.Error(nil, "conflict detected: resource created externally", "resourcechange", rc.Name)
        r.event(txn, corev1.EventTypeWarning, "ConflictDetected",
            "%s: %s/%s was created externally since prepare", rc.Name, change.Target.Kind, change.Target.Name)
        return ctrl.Result{}, r.setFailed(ctx, txn,
            (&ErrConflictDetected{Ref: change.Target}).Error())
    }
    if !apierrors.IsNotFound(err) {
        return ctrl.Result{}, err
    }
}
```

**Step 4: Run the test to verify it passes**

Run: `make test`
Expected: ALL PASS (new test passes, existing "should not check for conflicts on Create" test at line 2779 still passes since no external resource is created in that test)

**Step 5: Commit**

```bash
git add internal/controller/transaction_controller.go internal/controller/transaction_controller_test.go
git commit -m "fix: detect external resource creation between prepare and commit

Create operations with no prior resource (expectedRV == \"\") previously
skipped conflict detection entirely. Now handleCommitting verifies the
target resource still doesn't exist before applying a Create. If an
external actor created it between prepare and commit, the transaction
fails with ErrConflictDetected."
```

---

### Task 2: Gap 1 — Skip `checkConflict` for SSA, add self-write detection

**Files:**
- Modify: `internal/controller/transaction_controller.go` — `handleCommitting` (520-613), `checkConflict` (731-753), `updateRollbackRV` condition (582)
- Test: `internal/controller/transaction_controller_test.go`

**Step 1: Write the failing test for self-write detection**

This test simulates the crash-retry scenario: commit an Update, then manually revert `item.Committed` to false (simulating crash before status update), and verify the retry succeeds instead of false-failing.

Add a new `Context` block after the conflict detection tests:

```go
Context("crash-retry idempotency", func() {
    It("should detect self-write on Update retry and continue instead of false-failing", func() {
        // Create a ConfigMap to update.
        cm := &corev1.ConfigMap{
            ObjectMeta: metav1.ObjectMeta{
                Name:      "selfwrite-cm",
                Namespace: testNamespace,
            },
            Data: map[string]string{"key": "original"},
        }
        Expect(k8sClient.Create(ctx, cm)).To(Succeed())

        updatedContent := map[string]any{
            "apiVersion": "v1",
            "kind":       "ConfigMap",
            "metadata":   map[string]any{"name": "selfwrite-cm", "namespace": testNamespace},
            "data":       map[string]any{"key": "updated"},
        }
        raw, err := json.Marshal(updatedContent)
        Expect(err).NotTo(HaveOccurred())

        txn := minimalTxn("selfwrite-txn")
        Expect(k8sClient.Create(ctx, txn)).To(Succeed())
        rc := createChange("selfwrite-txn", "selfwrite-cm-change", backupv1alpha1.ResourceChangeSpec{
            Target: backupv1alpha1.ResourceRef{
                APIVersion: "v1",
                Kind:       "ConfigMap",
                Name:       "selfwrite-cm",
                Namespace:  testNamespace,
            },
            Type:    backupv1alpha1.ChangeTypeUpdate,
            Content: runtime.RawExtension{Raw: raw},
        }, txn.UID)
        Expect(k8sClient.Create(ctx, rc)).To(Succeed())

        // Reconcile through Prepared.
        reconcileToPhase(reconciler, "selfwrite-txn", backupv1alpha1.TransactionPhasePrepared)

        // Reconcile one more time to commit item 0.
        _, err = reconciler.Reconcile(ctx, req("selfwrite-txn"))
        Expect(err).NotTo(HaveOccurred())

        // Verify the item was committed and updateRollbackRV ran.
        Expect(k8sClient.Get(ctx, nn("selfwrite-txn"), txn)).To(Succeed())
        Expect(txn.Status.Items[0].Committed).To(BeTrue())

        // Simulate crash: revert Committed flag while leaving the
        // target resource and rollback ConfigMap as-is.
        txn.Status.Items[0].Committed = false
        Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

        // Retry reconcile — should detect self-write via rollback CM
        // post-commit RV and continue to Committed, not fail.
        for range 10 {
            Expect(k8sClient.Get(ctx, nn("selfwrite-txn"), txn)).To(Succeed())
            if txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted ||
                txn.Status.Phase == backupv1alpha1.TransactionPhaseFailed {
                break
            }
            _, err = reconciler.Reconcile(ctx, req("selfwrite-txn"))
            Expect(err).NotTo(HaveOccurred())
        }
        Expect(k8sClient.Get(ctx, nn("selfwrite-txn"), txn)).To(Succeed())
        Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))

        // Clean up.
        Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
    })
})
```

**Step 2: Run the test to verify it fails**

Run: `make test`
Expected: FAIL — transaction enters `Failed` because `checkConflict` sees the RV mismatch from Janus's own commit.

**Step 3: Implement the fixes**

**3a. Change `checkConflict` signature to accept `committedRV`:**

Replace the `checkConflict` function (lines 730-753):

```go
// checkConflict verifies that a target resource has not been modified since prepare.
// committedRV, if non-empty, is the post-commit RV from a previous (crashed) attempt;
// a match indicates a self-write rather than an external modification.
func (r *TransactionReconciler) checkConflict(ctx context.Context, cl client.Client,
	change backupv1alpha1.ResourceChangeSpec, ns string, expectedRV, committedRV string) error {

	if expectedRV == "" {
		return nil
	}

	obj, err := r.getResource(ctx, cl, change.Target, ns)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &ErrConflictDetected{Ref: change.Target, Expected: expectedRV}
		}
		return err
	}
	currentRV := obj.GetResourceVersion()
	if currentRV != expectedRV {
		// Check if this is our own previous write (crash-retry).
		if committedRV != "" && currentRV == committedRV {
			return nil
		}
		return &ErrConflictDetected{
			Ref:      change.Target,
			Expected: expectedRV,
			Actual:   currentRV,
		}
	}
	return nil
}
```

**3b. Add helper to read post-commit RV from the rollback ConfigMap.**

Add this function after `updateRollbackRV` (after line 1020):

```go
// readCommittedRV reads the post-commit resourceVersion from the rollback
// ConfigMap for the given resource. Returns "" if no RV is stored or if the
// ConfigMap cannot be read (best-effort for self-write detection).
func (r *TransactionReconciler) readCommittedRV(ctx context.Context, txn *backupv1alpha1.Transaction, change backupv1alpha1.ResourceChangeSpec, ns string) string {
	rbKey := rollback.Key(change.Target.Kind, ns, change.Target.Name)
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: txn.Status.RollbackRef, Namespace: txn.Namespace}, cm); err != nil {
		return ""
	}
	raw, ok := cm.Data[rbKey]
	if !ok {
		return ""
	}
	var env rollback.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return ""
	}
	return env.ResourceVersion
}
```

**3c. Update `handleCommitting` to skip `checkConflict` for SSA and pass `committedRV` for Update/Delete.**

Replace the conflict check block (lines 552-559) with:

```go
// Conflict detection: only needed for Update/Delete (RV-based).
// SSA Create/Patch are idempotent — field ownership is the conflict mechanism.
if change.Type == backupv1alpha1.ChangeTypeUpdate || change.Type == backupv1alpha1.ChangeTypeDelete {
    committedRV := r.readCommittedRV(ctx, txn, change, ns)
    if err := r.checkConflict(ctx, userClient, change, ns, item.ResourceVersion, committedRV); err != nil {
        txmetrics.ItemOperations.WithLabelValues("commit", "conflict").Inc()
        log.Error(err, "conflict detected, failing transaction", "resourcechange", rc.Name)
        r.event(txn, corev1.EventTypeWarning, "ConflictDetected",
            "%s: %s/%s was modified externally since prepare", rc.Name, change.Target.Kind, change.Target.Name)
        return ctrl.Result{}, r.setFailed(ctx, txn, err.Error())
    }
}
```

**3d. Extend `updateRollbackRV` to run for all change types (not just Update/Patch).**

Replace line 582:
```go
if change.Type == backupv1alpha1.ChangeTypeUpdate || change.Type == backupv1alpha1.ChangeTypePatch {
```
with:
```go
if change.Type != backupv1alpha1.ChangeTypeDelete {
```

This covers Create, Update, and Patch. Delete has no post-commit resource to read.

Also update the `updateRollbackRV` early-return comment (line 1005) from:
```go
return nil // No envelope to update (e.g. Create change).
```
to:
```go
return nil // No envelope to update.
```

**Step 4: Run the test to verify it passes**

Run: `make test`
Expected: ALL PASS — self-write test passes, existing conflict detection tests still pass (external modifications still detected because `committedRV` won't match).

**Step 5: Commit**

```bash
git add internal/controller/transaction_controller.go internal/controller/transaction_controller_test.go
git commit -m "fix: make commit idempotent for crash-retry scenarios

Skip checkConflict for SSA Create/Patch (idempotent by design).
Add self-write detection for Update/Delete: read post-commit RV from
rollback ConfigMap and recognize Janus's own previous write. Extend
updateRollbackRV to also store post-commit RV for Create operations."
```

---

### Task 3: Gap 2 — Add RV check before SSA rollback

**Files:**
- Modify: `internal/controller/transaction_controller.go` — `applyRollback` (864-985)
- Test: `internal/controller/transaction_controller_test.go`

**Step 1: Write the failing test for Patch rollback conflict**

Add inside a new `Context` after `"rollback conflict detection"`:

```go
It("should detect rollback conflict on Patch when resource modified after commit", func() {
    // Create target ConfigMap.
    cm := &corev1.ConfigMap{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "rb-patch-conflict-cm",
            Namespace: testNamespace,
        },
        Data: map[string]string{"key": "original", "other": "untouched"},
    }
    Expect(k8sClient.Create(ctx, cm)).To(Succeed())

    // Two-item transaction: Patch item 0, then item 1 (blocker) fails to trigger rollback.
    txn := minimalTxn("rb-patch-conflict-txn")
    Expect(k8sClient.Create(ctx, txn)).To(Succeed())

    patchRC := createChange("rb-patch-conflict-txn", "rb-patch-conflict-cm-change", backupv1alpha1.ResourceChangeSpec{
        Target: backupv1alpha1.ResourceRef{
            APIVersion: "v1",
            Kind:       "ConfigMap",
            Name:       "rb-patch-conflict-cm",
            Namespace:  testNamespace,
        },
        Type:    backupv1alpha1.ChangeTypePatch,
        Content: runtime.RawExtension{Raw: []byte(`{"data":{"key":"patched"}}`)},
    }, txn.UID)
    Expect(k8sClient.Create(ctx, patchRC)).To(Succeed())

    blocker := blockerChange("rb-patch-conflict-txn", "rb-patch-conflict-blocker", txn.UID)
    Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

    // Use a lock manager that fails Renew on the second item to trigger rollback.
    reconciler.LockMgr = &fakeLockMgr{
        renewFn: func(_ context.Context, lease lock.LeaseRef, _ string, _ time.Duration) error {
            // Fail renewal for the blocker item only.
            if lease.Name != "" && lease.Name == lock.LeaseName(lock.ResourceKey{
                Namespace: testNamespace,
                Kind:      "ConfigMap",
                Name:      "rb-patch-conflict-blocker",
            }) {
                return &lock.ErrLockExpired{LeaseName: lease.Name}
            }
            return nil
        },
    }

    // Reconcile to Committing: item 0 commits, item 1 triggers rollback.
    reconcileToPhase(reconciler, "rb-patch-conflict-txn", backupv1alpha1.TransactionPhaseRollingBack)

    // External actor modifies the patched field after Janus committed.
    Expect(k8sClient.Get(ctx, nn("rb-patch-conflict-cm"), cm)).To(Succeed())
    cm.Data["key"] = "externally-modified-after-commit"
    Expect(k8sClient.Update(ctx, cm)).To(Succeed())

    // Reconcile rollback — should detect conflict and fail.
    for range 10 {
        Expect(k8sClient.Get(ctx, nn("rb-patch-conflict-txn"), txn)).To(Succeed())
        if txn.Status.Phase == backupv1alpha1.TransactionPhaseFailed ||
            txn.Status.Phase == backupv1alpha1.TransactionPhaseRolledBack {
            break
        }
        _, err := reconciler.Reconcile(ctx, req("rb-patch-conflict-txn"))
        Expect(err).NotTo(HaveOccurred())
    }
    Expect(k8sClient.Get(ctx, nn("rb-patch-conflict-txn"), txn)).To(Succeed())
    Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))

    // Clean up.
    Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
    blocker_cm := &corev1.ConfigMap{}
    _ = client.IgnoreNotFound(k8sClient.Delete(ctx, blocker_cm))
})
```

**Step 2: Write the failing test for Create rollback conflict (fresh create)**

```go
It("should detect rollback conflict on Create (fresh) when resource modified after commit", func() {
    // Two-item transaction: Create item 0, item 1 (blocker) fails to trigger rollback.
    cmContent := map[string]any{
        "apiVersion": "v1",
        "kind":       "ConfigMap",
        "metadata":   map[string]any{"name": "rb-create-conflict-cm", "namespace": testNamespace},
        "data":       map[string]any{"key": "from-janus"},
    }
    raw, err := json.Marshal(cmContent)
    Expect(err).NotTo(HaveOccurred())

    txn := minimalTxn("rb-create-conflict-txn")
    Expect(k8sClient.Create(ctx, txn)).To(Succeed())

    createRC := createChange("rb-create-conflict-txn", "rb-create-conflict-cm-change", backupv1alpha1.ResourceChangeSpec{
        Target: backupv1alpha1.ResourceRef{
            APIVersion: "v1",
            Kind:       "ConfigMap",
            Name:       "rb-create-conflict-cm",
            Namespace:  testNamespace,
        },
        Type:    backupv1alpha1.ChangeTypeCreate,
        Content: runtime.RawExtension{Raw: raw},
    }, txn.UID)
    Expect(k8sClient.Create(ctx, createRC)).To(Succeed())

    blocker := blockerChange("rb-create-conflict-txn", "rb-create-conflict-blocker", txn.UID)
    Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

    reconciler.LockMgr = &fakeLockMgr{
        renewFn: func(_ context.Context, lease lock.LeaseRef, _ string, _ time.Duration) error {
            if lease.Name == lock.LeaseName(lock.ResourceKey{
                Namespace: testNamespace,
                Kind:      "ConfigMap",
                Name:      "rb-create-conflict-blocker",
            }) {
                return &lock.ErrLockExpired{LeaseName: lease.Name}
            }
            return nil
        },
    }

    reconcileToPhase(reconciler, "rb-create-conflict-txn", backupv1alpha1.TransactionPhaseRollingBack)

    // External actor modifies the resource after Janus created it.
    cm := &corev1.ConfigMap{}
    Expect(k8sClient.Get(ctx, nn("rb-create-conflict-cm"), cm)).To(Succeed())
    cm.Data["key"] = "externally-modified"
    Expect(k8sClient.Update(ctx, cm)).To(Succeed())

    // Reconcile rollback — should detect conflict.
    for range 10 {
        Expect(k8sClient.Get(ctx, nn("rb-create-conflict-txn"), txn)).To(Succeed())
        if txn.Status.Phase == backupv1alpha1.TransactionPhaseFailed ||
            txn.Status.Phase == backupv1alpha1.TransactionPhaseRolledBack {
            break
        }
        _, err = reconciler.Reconcile(ctx, req("rb-create-conflict-txn"))
        Expect(err).NotTo(HaveOccurred())
    }
    Expect(k8sClient.Get(ctx, nn("rb-create-conflict-txn"), txn)).To(Succeed())
    Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))

    // Clean up.
    Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
})
```

**Step 3: Run tests to verify they fail**

Run: `make test`
Expected: FAIL — SSA rollback currently applies without checking RV.

**Step 4: Implement the fix**

Modify `applyRollback` to add RV checks before SSA operations.

**4a. Add a helper at the top of `applyRollback` to do the RV check:**

No helper needed — inline the check in each case since the logic differs slightly.

**4b. Patch rollback (lines 919-948) — add RV check:**

Replace the Patch case with:

```go
case backupv1alpha1.ChangeTypePatch:
    // Reverse of Patch = SSA-apply prior values for the patched fields.
    obj, storedRV, err := loadRollbackState(rbCM, rbKey, namespace)
    if err != nil {
        return err
    }

    // Check for external modifications since commit.
    if storedRV != "" {
        existing, err := r.getResource(ctx, cl, change.Target, namespace)
        if err != nil {
            if apierrors.IsNotFound(err) {
                return &ErrRollbackConflict{Ref: change.Target, StoredRV: storedRV}
            }
            return err
        }
        if existing.GetResourceVersion() != storedRV {
            return &ErrRollbackConflict{
                Ref:       change.Target,
                StoredRV:  storedRV,
                CurrentRV: existing.GetResourceVersion(),
            }
        }
    }

    // Parse the forward patch content to know which fields were touched.
    var contentMap map[string]any
    if err := json.Unmarshal(change.Content.Raw, &contentMap); err != nil {
        return &ResourceOpError{Op: "unmarshaling patch content for reverse", Err: err}
    }

    // Compute reverse: same field paths, prior values.
    reversePatch := computeReversePatch(contentMap, obj.Object)

    // SSA requires identity fields.
    gv, err := schema.ParseGroupVersion(change.Target.APIVersion)
    if err != nil {
        return &ResourceOpError{Op: "parsing apiVersion for rollback patch", Ref: change.Target.APIVersion, Err: err}
    }
    patchObj := &unstructured.Unstructured{Object: reversePatch}
    patchObj.SetGroupVersionKind(schema.GroupVersionKind{
        Group: gv.Group, Version: gv.Version, Kind: change.Target.Kind,
    })
    patchObj.SetName(change.Target.Name)
    patchObj.SetNamespace(namespace)

    ac := client.ApplyConfigurationFromUnstructured(patchObj)
    return cl.Apply(ctx, ac, client.FieldOwner("janus-"+txnName), client.ForceOwnership)
```

**4c. Create rollback — fresh create branch (lines 875-884) — add RV check:**

Replace the `obj == nil` branch:

```go
if obj == nil {
    // Fresh create — delete to reverse.
    existing, err := r.getResource(ctx, cl, change.Target, namespace)
    if err != nil {
        if apierrors.IsNotFound(err) {
            return nil
        }
        return err
    }
    // Check for external modifications since commit.
    if storedRV != "" && existing.GetResourceVersion() != storedRV {
        return &ErrRollbackConflict{
            Ref:       change.Target,
            StoredRV:  storedRV,
            CurrentRV: existing.GetResourceVersion(),
        }
    }
    return cl.Delete(ctx, existing)
}
```

Note: `storedRV` is now available because Task 2 extended `updateRollbackRV` to run for Create. The `loadRollbackState` call at line 871 already returns `storedRV` as its second return value.

**4d. Create rollback — resource-existed branch (lines 886-903) — add RV check:**

Replace the "Resource existed before" block:

```go
// Resource existed before — restore prior field values via SSA.
// Check for external modifications since commit.
if storedRV != "" {
    existing, err := r.getResource(ctx, cl, change.Target, namespace)
    if err != nil {
        if apierrors.IsNotFound(err) {
            return &ErrRollbackConflict{Ref: change.Target, StoredRV: storedRV}
        }
        return err
    }
    if existing.GetResourceVersion() != storedRV {
        return &ErrRollbackConflict{
            Ref:       change.Target,
            StoredRV:  storedRV,
            CurrentRV: existing.GetResourceVersion(),
        }
    }
}
var contentMap map[string]any
if err := json.Unmarshal(change.Content.Raw, &contentMap); err != nil {
    return &ResourceOpError{Op: "unmarshaling create content for reverse", Err: err}
}
reversePatch := computeReversePatch(contentMap, obj.Object)
gv, err := schema.ParseGroupVersion(change.Target.APIVersion)
if err != nil {
    return &ResourceOpError{Op: "parsing apiVersion for rollback create", Ref: change.Target.APIVersion, Err: err}
}
patchObj := &unstructured.Unstructured{Object: reversePatch}
patchObj.SetGroupVersionKind(schema.GroupVersionKind{
    Group: gv.Group, Version: gv.Version, Kind: change.Target.Kind,
})
patchObj.SetName(change.Target.Name)
patchObj.SetNamespace(namespace)
ac := client.ApplyConfigurationFromUnstructured(patchObj)
return cl.Apply(ctx, ac, client.FieldOwner("janus-"+txnName), client.ForceOwnership)
```

**Step 5: Run tests to verify they pass**

Run: `make test`
Expected: ALL PASS — new rollback conflict tests pass, existing rollback tests still pass (no external modification in those tests, so stored RV matches).

**Step 6: Commit**

```bash
git add internal/controller/transaction_controller.go internal/controller/transaction_controller_test.go
git commit -m "fix: add conflict detection to SSA rollback paths

Patch and Create rollback now check the resource's current RV against
the post-commit RV stored in the rollback ConfigMap before applying.
If they differ, ErrRollbackConflict is returned, consistent with the
existing Update rollback behavior. This prevents silently overwriting
external modifications during rollback."
```

---

### Task 4: Verify all existing tests pass and clean up

**Step 1: Run full test suite**

Run: `make test`
Expected: ALL PASS

**Step 2: Run linter if available**

Run: `make lint` or `golangci-lint run ./...`
Expected: No new issues

**Step 3: Verify the existing "should not check for conflicts on Create" test**

This test (line 2779) creates a Create transaction where no external actor intervenes. After all three fixes:
- Gap 3: The new existence check in `handleCommitting` calls `getResource` → `NotFound` → proceeds normally.
- Gap 1: `checkConflict` is skipped for SSA Create.
- The test should still reach `Committed` with empty `ResourceVersion`.

No changes needed to this test.

**Step 4: Final commit (if any cleanup needed)**

Only commit if linter or tests revealed issues that need addressing.
