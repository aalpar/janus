# Conflict Detection Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Detect when a target resource is modified between prepare and commit, and fail the transaction with a clear error instead of silently overwriting.

**Architecture:** Two-phase defense. Phase 1 (Approach A): pre-commit GET + compare resourceVersion. Phase 2 (Approach C): native Kubernetes preconditions for Update/Delete to close the TOCTOU gap. Both phases produce the same `ErrConflictDetected` error type.

**Tech Stack:** controller-runtime client, Kubernetes coordination API, envtest for testing.

**Design doc:** `docs/plans/2026-02-22-conflict-detection-design.md`

---

### Task 1: Add ErrConflictDetected error type

**Files:**
- Modify: `internal/controller/errors.go`

**Step 1: Write the error type**

Add to `internal/controller/errors.go` after the `RollbackDataError` type:

```go
// ErrConflictDetected indicates a target resource was modified between prepare and commit.
type ErrConflictDetected struct {
	Ref      backupv1alpha1.ResourceRef
	Expected string // resourceVersion at prepare time
	Actual   string // resourceVersion at commit time (empty if resource was deleted)
}

func (e *ErrConflictDetected) Error() string {
	if e.Actual == "" {
		return fmt.Sprintf("conflict on %s/%s: resource was deleted (expected resourceVersion %s)",
			e.Ref.Kind, e.Ref.Name, e.Expected)
	}
	return fmt.Sprintf("conflict on %s/%s: resourceVersion changed from %s to %s",
		e.Ref.Kind, e.Ref.Name, e.Expected, e.Actual)
}
```

Add the import for `backupv1alpha1` at the top of the file:

```go
import (
	"errors"
	"fmt"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
)
```

**Step 2: Verify it compiles**

Run: `go build ./internal/controller/...`
Expected: clean build, no errors.

**Step 3: Commit**

```
git add internal/controller/errors.go
git commit -m "Add ErrConflictDetected error type for conflict detection"
```

---

### Task 2: Add ResourceVersion field to ItemStatus

**Files:**
- Modify: `api/v1alpha1/transaction_types.go`
- Regenerate: `api/v1alpha1/zz_generated.deepcopy.go`

**Step 1: Add the field**

In `api/v1alpha1/transaction_types.go`, add `ResourceVersion` to `ItemStatus` after `LeaseNamespace`:

```go
type ItemStatus struct {
	// LockLease is the name of the Lease object used to lock this resource.
	// +optional
	LockLease string `json:"lockLease,omitempty"`
	// LeaseNamespace is the namespace of the Lease object.
	// +optional
	LeaseNamespace string `json:"leaseNamespace,omitempty"`
	// ResourceVersion is the target resource's version at prepare time.
	// Used to detect external modifications before commit.
	// +optional
	ResourceVersion string `json:"resourceVersion,omitempty"`
	// Prepared indicates whether the resource's prior state has been captured.
	Prepared bool `json:"prepared"`
	// Committed indicates whether the mutation has been applied.
	Committed bool `json:"committed"`
	// RolledBack indicates whether a committed change has been reverted.
	RolledBack bool `json:"rolledBack"`
	// Error records any error encountered during processing of this item.
	// +optional
	Error string `json:"error,omitempty"`
}
```

**Step 2: Regenerate deepcopy**

Run: `make generate`
Expected: `zz_generated.deepcopy.go` updated (string field, no functional change to deepcopy).

If `make generate` is not available or fails, verify manually:
Run: `go build ./api/...`
Expected: clean build. (String fields don't affect deepcopy logic.)

**Step 3: Verify full build**

Run: `go build ./...`
Expected: clean build.

**Step 4: Commit**

```
git add api/v1alpha1/transaction_types.go api/v1alpha1/zz_generated.deepcopy.go
git commit -m "Add ResourceVersion field to ItemStatus for conflict detection"
```

---

### Task 3: Capture resourceVersion during prepare

**Files:**
- Modify: `internal/controller/transaction_controller.go` (`handlePreparing`)

**Step 1: Write the failing test**

In `internal/controller/transaction_controller_test.go`, add a new Context block inside the top-level Describe:

```go
Context("conflict detection", func() {
	It("should store resourceVersion in ItemStatus during prepare", func() {
		// Create a target ConfigMap.
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "conflict-rv-cm",
				Namespace: testNamespace,
			},
			Data: map[string]string{"key": "value"},
		}
		Expect(k8sClient.Create(ctx, cm)).To(Succeed())

		// Read back to get the resourceVersion.
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "conflict-rv-cm", Namespace: testNamespace,
		}, cm)).To(Succeed())
		expectedRV := cm.ResourceVersion

		txn := &backupv1alpha1.Transaction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rv-capture-txn",
				Namespace: testNamespace,
			},
			Spec: backupv1alpha1.TransactionSpec{
				ServiceAccountName: testSAName,
				Changes: []backupv1alpha1.ResourceChange{{
					Target: backupv1alpha1.ResourceRef{
						APIVersion: "v1",
						Kind:       "ConfigMap",
						Name:       "conflict-rv-cm",
						Namespace:  testNamespace,
					},
					Type:    backupv1alpha1.ChangeTypePatch,
					Content: runtime.RawExtension{Raw: []byte(`{"data":{"key":"patched"}}`)},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, txn)).To(Succeed())

		// Reconcile until Prepared.
		for range 10 {
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "rv-capture-txn", Namespace: testNamespace,
			}, txn)).To(Succeed())
			if txn.Status.Phase == backupv1alpha1.TransactionPhasePrepared {
				break
			}
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: "rv-capture-txn", Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
		}

		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "rv-capture-txn", Namespace: testNamespace,
		}, txn)).To(Succeed())
		Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhasePrepared))
		Expect(txn.Status.Items[0].ResourceVersion).To(Equal(expectedRV))

		// Clean up.
		Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
	})
})
```

**Step 2: Verify test fails**

Run: `go test ./internal/controller/... -run TestControllers -count=1 -v`
Expected: FAIL — `ResourceVersion` field is empty string, does not equal `expectedRV`.

Note: If envtest binaries are not installed, this will fail in BeforeSuite. In that case, run `make setup-envtest` first, or verify by reading the code path that `ResourceVersion` is never set.

**Step 3: Implement — capture resourceVersion in handlePreparing**

In `internal/controller/transaction_controller.go`, in `handlePreparing`, after the `getResource` call (around line 286), add one line:

```go
// Read current state and store in rollback ConfigMap.
if change.Type != backupv1alpha1.ChangeTypeCreate {
	obj, err := r.getResource(ctx, userClient, change.Target, ns)
	if err != nil {
		txmetrics.ItemOperations.WithLabelValues("prepare", "error").Inc()
		return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("item %d: reading current state: %v", i, err))
	}
	txn.Status.Items[i].ResourceVersion = obj.GetResourceVersion()
	if err := r.saveRollbackState(ctx, txn, change, obj); err != nil {
		txmetrics.ItemOperations.WithLabelValues("prepare", "error").Inc()
		return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("item %d: saving rollback state: %v", i, err))
	}
}
```

**Step 4: Verify test passes**

Run: `go test ./internal/controller/... -run TestControllers -count=1 -v`
Expected: PASS.

**Step 5: Commit**

```
git add internal/controller/transaction_controller.go internal/controller/transaction_controller_test.go
git commit -m "Capture resourceVersion in ItemStatus during prepare phase"
```

---

### Task 4: Add checkConflict and wire into handleCommitting (Phase 1)

**Files:**
- Modify: `internal/controller/transaction_controller.go`

**Step 1: Write the failing test — conflict detected on Patch**

Add to the `conflict detection` Context:

```go
It("should fail the transaction when resource is modified between prepare and commit (Patch)", func() {
	// Create a target ConfigMap.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflict-patch-cm",
			Namespace: testNamespace,
		},
		Data: map[string]string{"key": "original"},
	}
	Expect(k8sClient.Create(ctx, cm)).To(Succeed())

	txn := &backupv1alpha1.Transaction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflict-patch-txn",
			Namespace: testNamespace,
		},
		Spec: backupv1alpha1.TransactionSpec{
			ServiceAccountName: testSAName,
			Changes: []backupv1alpha1.ResourceChange{{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "conflict-patch-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: []byte(`{"data":{"key":"patched"}}`)},
			}},
		},
	}
	Expect(k8sClient.Create(ctx, txn)).To(Succeed())

	// Reconcile until Prepared (but not yet Committing).
	for range 10 {
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "conflict-patch-txn", Namespace: testNamespace,
		}, txn)).To(Succeed())
		if txn.Status.Phase == backupv1alpha1.TransactionPhasePrepared {
			break
		}
		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name: "conflict-patch-txn", Namespace: testNamespace,
			},
		})
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhasePrepared))

	// External actor modifies the resource.
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: "conflict-patch-cm", Namespace: testNamespace,
	}, cm)).To(Succeed())
	cm.Data["key"] = "externally-modified"
	Expect(k8sClient.Update(ctx, cm)).To(Succeed())

	// Reconcile through Committing — should detect conflict and fail.
	for range 10 {
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "conflict-patch-txn", Namespace: testNamespace,
		}, txn)).To(Succeed())
		if txn.Status.Phase == backupv1alpha1.TransactionPhaseFailed ||
			txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted {
			break
		}
		_, _ = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name: "conflict-patch-txn", Namespace: testNamespace,
			},
		})
	}

	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: "conflict-patch-txn", Namespace: testNamespace,
	}, txn)).To(Succeed())
	Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))

	// Verify ConflictDetected condition.
	var conflictCondition *metav1.Condition
	for i := range txn.Status.Conditions {
		if txn.Status.Conditions[i].Type == "Failed" {
			conflictCondition = &txn.Status.Conditions[i]
			break
		}
	}
	Expect(conflictCondition).NotTo(BeNil())
	Expect(conflictCondition.Message).To(ContainSubstring("conflict"))
	Expect(conflictCondition.Message).To(ContainSubstring("resourceVersion"))

	// Clean up.
	Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
})
```

**Step 2: Verify test fails**

Run: `go test ./internal/controller/... -run TestControllers -count=1 -v`
Expected: FAIL — transaction reaches Committed instead of Failed.

**Step 3: Implement checkConflict**

Add to `internal/controller/transaction_controller.go`, before the `--- Resource operations ---` comment:

```go
// checkConflict verifies that a target resource has not been modified since prepare.
// Returns ErrConflictDetected if the resourceVersion has changed.
func (r *TransactionReconciler) checkConflict(ctx context.Context, cl client.Client,
	change backupv1alpha1.ResourceChange, ns string, expectedRV string) error {

	if change.Type == backupv1alpha1.ChangeTypeCreate || expectedRV == "" {
		return nil
	}

	obj, err := r.getResource(ctx, cl, change.Target, ns)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &ErrConflictDetected{Ref: change.Target, Expected: expectedRV}
		}
		return err
	}
	if obj.GetResourceVersion() != expectedRV {
		return &ErrConflictDetected{
			Ref:      change.Target,
			Expected: expectedRV,
			Actual:   obj.GetResourceVersion(),
		}
	}
	return nil
}
```

**Step 4: Wire checkConflict into handleCommitting**

In `handleCommitting`, after the lock renewal block and before `applyChange`, add the conflict check:

```go
// Check for external modifications since prepare.
ns := r.resolveNamespace(change.Target, txn.Namespace)
if err := r.checkConflict(ctx, userClient, change, ns, txn.Status.Items[i].ResourceVersion); err != nil {
	txmetrics.ItemOperations.WithLabelValues("commit", "conflict").Inc()
	log.Error(err, "conflict detected, failing transaction", "item", i)
	r.event(txn, corev1.EventTypeWarning, "ConflictDetected",
		"item %d: %s/%s was modified externally since prepare", i, change.Target.Kind, change.Target.Name)
	return ctrl.Result{}, r.setFailed(ctx, txn, err.Error())
}
```

Note: The existing `ns := r.resolveNamespace(...)` call on line 329 should be moved up before the conflict check (or the conflict check placed after it). Make sure `ns` is computed once, before both the conflict check and `applyChange`.

**Step 5: Verify test passes**

Run: `go test ./internal/controller/... -run TestControllers -count=1 -v`
Expected: PASS.

**Step 6: Commit**

```
git add internal/controller/transaction_controller.go internal/controller/transaction_controller_test.go
git commit -m "Add pre-commit conflict detection (Phase 1 / Approach A)"
```

---

### Task 5: Write conflict detection test for Delete

**Files:**
- Modify: `internal/controller/transaction_controller_test.go`

**Step 1: Write the test — conflict detected on Delete**

Add to the `conflict detection` Context:

```go
It("should fail the transaction when resource is modified between prepare and commit (Delete)", func() {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflict-delete-cm",
			Namespace: testNamespace,
		},
		Data: map[string]string{"key": "value"},
	}
	Expect(k8sClient.Create(ctx, cm)).To(Succeed())

	txn := &backupv1alpha1.Transaction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflict-delete-txn",
			Namespace: testNamespace,
		},
		Spec: backupv1alpha1.TransactionSpec{
			ServiceAccountName: testSAName,
			Changes: []backupv1alpha1.ResourceChange{{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "conflict-delete-cm",
					Namespace:  testNamespace,
				},
				Type: backupv1alpha1.ChangeTypeDelete,
			}},
		},
	}
	Expect(k8sClient.Create(ctx, txn)).To(Succeed())

	// Reconcile until Prepared.
	for range 10 {
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "conflict-delete-txn", Namespace: testNamespace,
		}, txn)).To(Succeed())
		if txn.Status.Phase == backupv1alpha1.TransactionPhasePrepared {
			break
		}
		_, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name: "conflict-delete-txn", Namespace: testNamespace,
			},
		})
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhasePrepared))

	// External actor modifies the resource.
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: "conflict-delete-cm", Namespace: testNamespace,
	}, cm)).To(Succeed())
	cm.Data["key"] = "externally-modified"
	Expect(k8sClient.Update(ctx, cm)).To(Succeed())

	// Reconcile through Committing — should detect conflict.
	for range 10 {
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "conflict-delete-txn", Namespace: testNamespace,
		}, txn)).To(Succeed())
		if txn.Status.Phase == backupv1alpha1.TransactionPhaseFailed ||
			txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted {
			break
		}
		_, _ = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name: "conflict-delete-txn", Namespace: testNamespace,
			},
		})
	}

	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: "conflict-delete-txn", Namespace: testNamespace,
	}, txn)).To(Succeed())
	Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))

	// Clean up.
	Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
})
```

**Step 2: Verify test passes (no new code needed — Task 4 covers all types)**

Run: `go test ./internal/controller/... -run TestControllers -count=1 -v`
Expected: PASS (checkConflict already handles Delete).

**Step 3: Commit**

```
git add internal/controller/transaction_controller_test.go
git commit -m "Add conflict detection test for Delete change type"
```

---

### Task 6: Write test for no conflict on Create

**Files:**
- Modify: `internal/controller/transaction_controller_test.go`

**Step 1: Write the test — Create skips conflict check**

Add to the `conflict detection` Context:

```go
It("should not check for conflicts on Create (no prior resource)", func() {
	cmContent := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "conflict-create-cm",
			"namespace": testNamespace,
		},
		"data": map[string]any{"key": "value"},
	}
	raw, err := json.Marshal(cmContent)
	Expect(err).NotTo(HaveOccurred())

	txn := &backupv1alpha1.Transaction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflict-create-txn",
			Namespace: testNamespace,
		},
		Spec: backupv1alpha1.TransactionSpec{
			ServiceAccountName: testSAName,
			Changes: []backupv1alpha1.ResourceChange{{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "conflict-create-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw},
			}},
		},
	}
	Expect(k8sClient.Create(ctx, txn)).To(Succeed())

	// Reconcile to completion.
	for range 15 {
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "conflict-create-txn", Namespace: testNamespace,
		}, txn)).To(Succeed())
		if txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted {
			break
		}
		_, err = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name: "conflict-create-txn", Namespace: testNamespace,
			},
		})
		Expect(err).NotTo(HaveOccurred())
	}

	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: "conflict-create-txn", Namespace: testNamespace,
	}, txn)).To(Succeed())
	Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))
	// ResourceVersion should be empty for Create items.
	Expect(txn.Status.Items[0].ResourceVersion).To(BeEmpty())

	// Clean up.
	cm := &corev1.ConfigMap{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: "conflict-create-cm", Namespace: testNamespace,
	}, cm)).To(Succeed())
	Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
})
```

**Step 2: Verify test passes**

Run: `go test ./internal/controller/... -run TestControllers -count=1 -v`
Expected: PASS.

**Step 3: Commit**

```
git add internal/controller/transaction_controller_test.go
git commit -m "Add test verifying Create change type skips conflict detection"
```

---

### Task 7: Native preconditions for Update (Phase 2)

**Files:**
- Modify: `internal/controller/transaction_controller.go` (`applyChange`, Update case)

**Step 1: Write the failing test — Update uses stored resourceVersion**

Add to the `conflict detection` Context:

```go
It("should fail via API conflict when resource changes between check and Update", func() {
	// This test verifies the Phase 2 (Approach C) behavior for Update:
	// applyChange uses the stored resourceVersion, so the API server rejects
	// if it changed. We test this by modifying the resource AFTER prepare
	// and verifying the transaction fails.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflict-update-cm",
			Namespace: testNamespace,
		},
		Data: map[string]string{"key": "original"},
	}
	Expect(k8sClient.Create(ctx, cm)).To(Succeed())

	updateContent, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "conflict-update-cm",
			"namespace": testNamespace,
		},
		"data": map[string]any{"key": "updated-by-txn"},
	})
	Expect(err).NotTo(HaveOccurred())

	txn := &backupv1alpha1.Transaction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "conflict-update-txn",
			Namespace: testNamespace,
		},
		Spec: backupv1alpha1.TransactionSpec{
			ServiceAccountName: testSAName,
			Changes: []backupv1alpha1.ResourceChange{{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "conflict-update-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeUpdate,
				Content: runtime.RawExtension{Raw: updateContent},
			}},
		},
	}
	Expect(k8sClient.Create(ctx, txn)).To(Succeed())

	// Reconcile until Prepared.
	for range 10 {
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "conflict-update-txn", Namespace: testNamespace,
		}, txn)).To(Succeed())
		if txn.Status.Phase == backupv1alpha1.TransactionPhasePrepared {
			break
		}
		_, err = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name: "conflict-update-txn", Namespace: testNamespace,
			},
		})
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhasePrepared))

	// External actor modifies the resource.
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: "conflict-update-cm", Namespace: testNamespace,
	}, cm)).To(Succeed())
	cm.Data["key"] = "externally-modified"
	Expect(k8sClient.Update(ctx, cm)).To(Succeed())

	// Reconcile — should fail (Phase 1 catches it; Phase 2 is belt-and-suspenders).
	for range 10 {
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: "conflict-update-txn", Namespace: testNamespace,
		}, txn)).To(Succeed())
		if txn.Status.Phase == backupv1alpha1.TransactionPhaseFailed ||
			txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted {
			break
		}
		_, _ = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name: "conflict-update-txn", Namespace: testNamespace,
			},
		})
	}

	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: "conflict-update-txn", Namespace: testNamespace,
	}, txn)).To(Succeed())
	Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))

	// Clean up.
	Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
})
```

**Step 2: Verify test passes (Phase 1 already catches this)**

Run: `go test ./internal/controller/... -run TestControllers -count=1 -v`
Expected: PASS (Phase 1 checkConflict catches it before applyChange).

**Step 3: Modify applyChange — Update case uses stored resourceVersion**

Change the `ChangeTypeUpdate` case in `applyChange` to accept and use the stored resourceVersion instead of fetching a fresh one. This requires changing the `applyChange` signature to accept the stored resourceVersion:

Update `applyChange` signature:

```go
func (r *TransactionReconciler) applyChange(ctx context.Context, cl client.Client,
	change backupv1alpha1.ResourceChange, ns, txnName, storedRV string) error {
```

Update the `ChangeTypeUpdate` case:

```go
case backupv1alpha1.ChangeTypeUpdate:
	obj, err := r.unmarshalContent(change.Content, change.Target)
	if err != nil {
		return err
	}
	obj.SetNamespace(namespace)
	obj.SetResourceVersion(storedRV)
	if err := cl.Update(ctx, obj); err != nil {
		if apierrors.IsConflict(err) {
			return &ErrConflictDetected{Ref: change.Target, Expected: storedRV}
		}
		return err
	}
	return nil
```

Update call sites in `handleCommitting`:

```go
if err := r.applyChange(ctx, userClient, change, ns, txn.Name, txn.Status.Items[i].ResourceVersion); err != nil {
```

Update call site in `applyRollback` (pass empty string — rollback doesn't use conflict detection):

```go
// No change needed if applyRollback calls applyChange indirectly.
// Actually, applyRollback has its own implementation, not calling applyChange.
// So no call site update needed there.
```

**Step 4: Verify all tests pass**

Run: `go test ./internal/controller/... -run TestControllers -count=1 -v`
Expected: PASS.

**Step 5: Commit**

```
git add internal/controller/transaction_controller.go internal/controller/transaction_controller_test.go
git commit -m "Use stored resourceVersion for Update (Phase 2 / Approach C)"
```

---

### Task 8: Native preconditions for Delete (Phase 2)

**Files:**
- Modify: `internal/controller/transaction_controller.go` (`applyChange`, Delete case)

**Step 1: Modify applyChange — Delete case with preconditions**

Update the `ChangeTypeDelete` case:

```go
case backupv1alpha1.ChangeTypeDelete:
	existing, err := r.getResource(ctx, cl, change.Target, namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // Already gone.
		}
		return &ResourceOpError{Op: "fetching for delete", Err: err}
	}
	opts := []client.DeleteOption{}
	if storedRV != "" {
		opts = append(opts, client.Preconditions{ResourceVersion: &storedRV})
	}
	if err := cl.Delete(ctx, existing, opts...); err != nil {
		if apierrors.IsConflict(err) {
			return &ErrConflictDetected{Ref: change.Target, Expected: storedRV}
		}
		return err
	}
	return nil
```

Note: `client.Preconditions` implements `client.DeleteOption`. If it doesn't compile, use `client.PropagationPolicy` pattern or check the controller-runtime API. The key is passing `metav1.Preconditions{ResourceVersion: &storedRV}` through the delete options.

Verify the exact API: controller-runtime's `client.DeleteOptions` has a `Preconditions` field. The idiomatic way:

```go
deleteOpts := &client.DeleteOptions{
	Preconditions: &metav1.Preconditions{
		ResourceVersion: &storedRV,
	},
}
if err := cl.Delete(ctx, existing, deleteOpts); err != nil {
```

**Step 2: Verify all tests pass**

Run: `go test ./internal/controller/... -run TestControllers -count=1 -v`
Expected: PASS.

Run: `go build ./...`
Expected: clean build.

**Step 3: Commit**

```
git add internal/controller/transaction_controller.go
git commit -m "Add resourceVersion precondition to Delete (Phase 2 / Approach C)"
```

---

### Task 9: Translate API 409 errors in handleCommitting

**Files:**
- Modify: `internal/controller/transaction_controller.go` (`handleCommitting`)

**Step 1: Add error translation after applyChange**

In `handleCommitting`, the existing error handling after `applyChange` triggers rollback. For conflict errors, we want to fail (not rollback). Update the error handling:

```go
if err := r.applyChange(ctx, userClient, change, ns, txn.Name, txn.Status.Items[i].ResourceVersion); err != nil {
	var conflictErr *ErrConflictDetected
	if errors.As(err, &conflictErr) {
		txmetrics.ItemOperations.WithLabelValues("commit", "conflict").Inc()
		log.Error(err, "conflict detected during apply, failing transaction", "item", i)
		r.event(txn, corev1.EventTypeWarning, "ConflictDetected",
			"item %d: %s/%s was modified externally", i, change.Target.Kind, change.Target.Name)
		return ctrl.Result{}, r.setFailed(ctx, txn, err.Error())
	}
	txmetrics.ItemOperations.WithLabelValues("commit", "error").Inc()
	txn.Status.Items[i].Error = err.Error()
	log.Error(err, "commit failed, initiating rollback", "item", i)
	r.event(txn, corev1.EventTypeWarning, "CommitFailed",
		"item %d: %s %s/%s failed: %v", i, change.Type, change.Target.Kind, change.Target.Name, err)
	return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRollingBack)
}
```

Add `"errors"` to the imports if not already present.

**Step 2: Verify all tests pass**

Run: `go test ./internal/controller/... -run TestControllers -count=1 -v`
Expected: PASS.

**Step 3: Commit**

```
git add internal/controller/transaction_controller.go
git commit -m "Translate API conflict errors to ErrConflictDetected in commit phase"
```

---

### Task 10: Verify existing tests still pass, final build check

**Step 1: Full build**

Run: `go build ./...`
Expected: clean build.

**Step 2: Full test suite**

Run: `go test ./... -count=1 -v`
Expected: All packages pass (controller tests require envtest binaries).

**Step 3: Lint**

Run: `golangci-lint run ./...`
Expected: no new issues.

**Step 4: Update TODO.md — remove the conflict detection item**

Remove the "Conflict Detection" section from `TODO.md`.

**Step 5: Commit**

```
git add TODO.md
git commit -m "Remove conflict detection from TODO — implemented"
```
