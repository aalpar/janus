# Webhook, Timeout, Status Conditions — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a validation webhook, a transaction-level timeout, and per-phase status conditions to the Transaction CRD.

**Architecture:** Three independent features sharing the same API types file. The webhook validates spec at admission time. The timeout check runs at the top of Reconcile. Conditions are set in `transition()` for phase changes.

**Tech Stack:** Go, kubebuilder, controller-runtime, cert-manager (webhook TLS), envtest (tests)

---

## Task 1: Status Conditions in `transition()`

The simplest feature — no API changes, no new files. Good warm-up that
the other features build on.

**Files:**
- Modify: `internal/controller/transaction_controller.go` (the `transition` method, ~line 703)
- Test: `internal/controller/transaction_controller_test.go`

**Step 1: Write the failing test**

Add a new Context block in the existing Describe. The test creates a
Transaction, reconciles it through to Prepared, then asserts the
`Prepared` condition exists.

```go
Context("status conditions", func() {
    It("should set Prepared condition when all items are prepared", func() {
        cmContent := map[string]any{
            "apiVersion": "v1",
            "kind":       "ConfigMap",
            "metadata":   map[string]any{"name": "cond-test-cm", "namespace": testNamespace},
            "data":       map[string]any{"key": "value"},
        }
        raw, err := json.Marshal(cmContent)
        Expect(err).NotTo(HaveOccurred())

        txn := &backupv1alpha1.Transaction{
            ObjectMeta: metav1.ObjectMeta{
                Name:      "cond-txn",
                Namespace: testNamespace,
            },
            Spec: backupv1alpha1.TransactionSpec{
                ServiceAccountName: testSAName,
                Changes: []backupv1alpha1.ResourceChange{{
                    Target: backupv1alpha1.ResourceRef{
                        APIVersion: "v1",
                        Kind:       "ConfigMap",
                        Name:       "cond-test-cm",
                        Namespace:  testNamespace,
                    },
                    Type:    backupv1alpha1.ChangeTypeCreate,
                    Content: runtime.RawExtension{Raw: raw},
                }},
            },
        }
        Expect(k8sClient.Create(ctx, txn)).To(Succeed())

        req := reconcile.Request{NamespacedName: types.NamespacedName{
            Name: "cond-txn", Namespace: testNamespace,
        }}

        // Reconcile: finalizer
        _, err = reconciler.Reconcile(ctx, req)
        Expect(err).NotTo(HaveOccurred())

        // Reconcile: Pending → Preparing
        _, err = reconciler.Reconcile(ctx, req)
        Expect(err).NotTo(HaveOccurred())

        // Reconcile: prepare item 0
        _, err = reconciler.Reconcile(ctx, req)
        Expect(err).NotTo(HaveOccurred())

        // Reconcile: Preparing → Prepared → Committing
        // (Prepared is a transient phase — the next reconcile transitions to Committing)
        _, err = reconciler.Reconcile(ctx, req)
        Expect(err).NotTo(HaveOccurred())

        // Re-fetch and check conditions
        Expect(k8sClient.Get(ctx, req.NamespacedName, txn)).To(Succeed())
        cond := apimeta.FindStatusCondition(txn.Status.Conditions, "Prepared")
        Expect(cond).NotTo(BeNil())
        Expect(cond.Status).To(Equal(metav1.ConditionTrue))
        Expect(cond.Reason).To(Equal("AllItemsPrepared"))
    })

    It("should set Committed condition when transaction completes", func() {
        cmContent := map[string]any{
            "apiVersion": "v1",
            "kind":       "ConfigMap",
            "metadata":   map[string]any{"name": "cond-commit-cm", "namespace": testNamespace},
            "data":       map[string]any{"key": "value"},
        }
        raw, err := json.Marshal(cmContent)
        Expect(err).NotTo(HaveOccurred())

        txn := &backupv1alpha1.Transaction{
            ObjectMeta: metav1.ObjectMeta{
                Name:      "cond-commit-txn",
                Namespace: testNamespace,
            },
            Spec: backupv1alpha1.TransactionSpec{
                ServiceAccountName: testSAName,
                Changes: []backupv1alpha1.ResourceChange{{
                    Target: backupv1alpha1.ResourceRef{
                        APIVersion: "v1",
                        Kind:       "ConfigMap",
                        Name:       "cond-commit-cm",
                        Namespace:  testNamespace,
                    },
                    Type:    backupv1alpha1.ChangeTypeCreate,
                    Content: runtime.RawExtension{Raw: raw},
                }},
            },
        }
        Expect(k8sClient.Create(ctx, txn)).To(Succeed())

        req := reconcile.Request{NamespacedName: types.NamespacedName{
            Name: "cond-commit-txn", Namespace: testNamespace,
        }}

        // Drive through all phases: finalizer, pending, prepare, prepared→committing, commit, committed
        for i := 0; i < 6; i++ {
            _, err = reconciler.Reconcile(ctx, req)
            Expect(err).NotTo(HaveOccurred())
        }

        Expect(k8sClient.Get(ctx, req.NamespacedName, txn)).To(Succeed())
        Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))

        cond := apimeta.FindStatusCondition(txn.Status.Conditions, "Committed")
        Expect(cond).NotTo(BeNil())
        Expect(cond.Status).To(Equal(metav1.ConditionTrue))
        Expect(cond.Reason).To(Equal("AllItemsCommitted"))

        // Prepared condition should also still be present (monotonic)
        prepCond := apimeta.FindStatusCondition(txn.Status.Conditions, "Prepared")
        Expect(prepCond).NotTo(BeNil())
    })
})
```

Import `apimeta "k8s.io/apimachinery/pkg/api/meta"` in the test file (it
already imports `"k8s.io/apimachinery/pkg/api/meta"` as `meta` for RESTMapper
in suite_test.go, but the test file needs the `apimeta` alias matching the
controller's convention).

**Step 2: Run test to verify it fails**

Run: `make test` or `go test ./internal/controller/ -run "Controller/status_conditions" -v`
Expected: FAIL — no `Prepared` condition set

**Step 3: Implement `conditionForPhase` and modify `transition`**

In `internal/controller/transaction_controller.go`, add a helper and call
it from `transition()`:

```go
// conditionForPhase returns a Condition to set when entering the given phase,
// or nil for phases that don't warrant a condition.
func conditionForPhase(phase backupv1alpha1.TransactionPhase) *metav1.Condition {
	now := metav1.Now()
	switch phase {
	case backupv1alpha1.TransactionPhasePrepared:
		return &metav1.Condition{
			Type:               "Prepared",
			Status:             metav1.ConditionTrue,
			Reason:             "AllItemsPrepared",
			Message:            "All items have been prepared",
			LastTransitionTime: now,
		}
	case backupv1alpha1.TransactionPhaseCommitted:
		return &metav1.Condition{
			Type:               "Committed",
			Status:             metav1.ConditionTrue,
			Reason:             "AllItemsCommitted",
			Message:            "All items have been committed",
			LastTransitionTime: now,
		}
	case backupv1alpha1.TransactionPhaseRolledBack:
		return &metav1.Condition{
			Type:               "RolledBack",
			Status:             metav1.ConditionTrue,
			Reason:             "RollbackComplete",
			Message:            "All committed items have been rolled back",
			LastTransitionTime: now,
		}
	}
	return nil
}
```

Then in the `transition` method, between phase assignment and status update:

```go
func (r *TransactionReconciler) transition(ctx context.Context, txn *backupv1alpha1.Transaction, phase backupv1alpha1.TransactionPhase) (ctrl.Result, error) {
	eventType := corev1.EventTypeNormal
	if phase == backupv1alpha1.TransactionPhaseRollingBack || phase == backupv1alpha1.TransactionPhaseFailed {
		eventType = corev1.EventTypeWarning
	}
	r.event(txn, eventType, "PhaseTransition", "transitioning to %s", phase)

	oldPhase := txn.Status.Phase
	txn.Status.Phase = phase
	txn.Status.Version++

	if c := conditionForPhase(phase); c != nil {
		apimeta.SetStatusCondition(&txn.Status.Conditions, *c)
	}

	if err := r.Status().Update(ctx, txn); err != nil {
		return ctrl.Result{}, err
	}

	recordPhaseChange(oldPhase, phase)
	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}
```

**Step 4: Run test to verify it passes**

Run: `make test`
Expected: PASS

**Step 5: Commit**

```
git add internal/controller/transaction_controller.go internal/controller/transaction_controller_test.go
git commit -m "Add per-phase status conditions (Prepared, Committed, RolledBack)"
```

---

## Task 2: Transaction-Level Timeout

**Files:**
- Modify: `api/v1alpha1/transaction_types.go` (~line 87, add `Timeout` field)
- Modify: `internal/controller/transaction_controller.go` (add `transactionTimeout`, add check in `Reconcile`)
- Test: `internal/controller/transaction_controller_test.go`

**Step 1: Add `Timeout` field to TransactionSpec**

In `api/v1alpha1/transaction_types.go`, add after `LockTimeout`:

```go
// Timeout bounds the overall transaction duration.
// When elapsed, the transaction transitions to RollingBack (if any commits
// exist) or Failed (if none do).
// Defaults to 30 minutes.
// +optional
Timeout *metav1.Duration `json:"timeout,omitempty"`
```

Run: `make generate manifests` to regenerate deepcopy and CRD YAML.

**Step 2: Write the failing test**

```go
Context("transaction timeout", func() {
    It("should fail a timed-out transaction with no commits", func() {
        cmContent := map[string]any{
            "apiVersion": "v1",
            "kind":       "ConfigMap",
            "metadata":   map[string]any{"name": "timeout-cm", "namespace": testNamespace},
            "data":       map[string]any{"key": "value"},
        }
        raw, err := json.Marshal(cmContent)
        Expect(err).NotTo(HaveOccurred())

        txn := &backupv1alpha1.Transaction{
            ObjectMeta: metav1.ObjectMeta{
                Name:      "timeout-txn",
                Namespace: testNamespace,
            },
            Spec: backupv1alpha1.TransactionSpec{
                ServiceAccountName: testSAName,
                Timeout:            &metav1.Duration{Duration: 1 * time.Nanosecond},
                Changes: []backupv1alpha1.ResourceChange{{
                    Target: backupv1alpha1.ResourceRef{
                        APIVersion: "v1",
                        Kind:       "ConfigMap",
                        Name:       "timeout-cm",
                        Namespace:  testNamespace,
                    },
                    Type:    backupv1alpha1.ChangeTypeCreate,
                    Content: runtime.RawExtension{Raw: raw},
                }},
            },
        }
        Expect(k8sClient.Create(ctx, txn)).To(Succeed())

        req := reconcile.Request{NamespacedName: types.NamespacedName{
            Name: "timeout-txn", Namespace: testNamespace,
        }}

        // Reconcile: finalizer
        _, err = reconciler.Reconcile(ctx, req)
        Expect(err).NotTo(HaveOccurred())

        // Reconcile: Pending — sets StartedAt
        _, err = reconciler.Reconcile(ctx, req)
        Expect(err).NotTo(HaveOccurred())

        // Reconcile: timeout fires (1ns has elapsed), should go to Failed
        _, err = reconciler.Reconcile(ctx, req)
        Expect(err).NotTo(HaveOccurred())

        Expect(k8sClient.Get(ctx, req.NamespacedName, txn)).To(Succeed())
        Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))

        cond := apimeta.FindStatusCondition(txn.Status.Conditions, "Failed")
        Expect(cond).NotTo(BeNil())
        Expect(cond.Message).To(ContainSubstring("timed out"))
    })

    It("should roll back a timed-out transaction with committed items", func() {
        // Create the target ConfigMap first (so the Update change has something to modify)
        existingCM := &corev1.ConfigMap{
            ObjectMeta: metav1.ObjectMeta{
                Name:      "timeout-rollback-cm",
                Namespace: testNamespace,
            },
            Data: map[string]string{"key": "original"},
        }
        Expect(k8sClient.Create(ctx, existingCM)).To(Succeed())

        updateContent := map[string]any{
            "apiVersion": "v1",
            "kind":       "ConfigMap",
            "metadata":   map[string]any{"name": "timeout-rollback-cm", "namespace": testNamespace},
            "data":       map[string]any{"key": "updated"},
        }
        raw, err := json.Marshal(updateContent)
        Expect(err).NotTo(HaveOccurred())

        // Two items: first will commit, second will be where timeout hits
        txn := &backupv1alpha1.Transaction{
            ObjectMeta: metav1.ObjectMeta{
                Name:      "timeout-rb-txn",
                Namespace: testNamespace,
            },
            Spec: backupv1alpha1.TransactionSpec{
                ServiceAccountName: testSAName,
                Timeout:            &metav1.Duration{Duration: 10 * time.Minute},
                Changes: []backupv1alpha1.ResourceChange{
                    {
                        Target: backupv1alpha1.ResourceRef{
                            APIVersion: "v1",
                            Kind:       "ConfigMap",
                            Name:       "timeout-rollback-cm",
                            Namespace:  testNamespace,
                        },
                        Type:    backupv1alpha1.ChangeTypeUpdate,
                        Content: runtime.RawExtension{Raw: raw},
                    },
                    {
                        Target: backupv1alpha1.ResourceRef{
                            APIVersion: "v1",
                            Kind:       "ConfigMap",
                            Name:       "timeout-rollback-cm2",
                            Namespace:  testNamespace,
                        },
                        Type: backupv1alpha1.ChangeTypeCreate,
                        Content: runtime.RawExtension{Raw: func() []byte {
                            b, _ := json.Marshal(map[string]any{
                                "apiVersion": "v1",
                                "kind":       "ConfigMap",
                                "metadata":   map[string]any{"name": "timeout-rollback-cm2", "namespace": testNamespace},
                                "data":       map[string]any{"key": "val"},
                            })
                            return b
                        }()},
                    },
                },
            },
        }
        Expect(k8sClient.Create(ctx, txn)).To(Succeed())

        req := reconcile.Request{NamespacedName: types.NamespacedName{
            Name: "timeout-rb-txn", Namespace: testNamespace,
        }}

        // Drive through: finalizer, pending, prepare item 0, prepare item 1,
        // Prepared→Committing, commit item 0
        for i := 0; i < 6; i++ {
            _, err = reconciler.Reconcile(ctx, req)
            Expect(err).NotTo(HaveOccurred())
        }

        // Verify item 0 is committed
        Expect(k8sClient.Get(ctx, req.NamespacedName, txn)).To(Succeed())
        Expect(txn.Status.Items[0].Committed).To(BeTrue())
        Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitting))

        // Now force the timeout by backdating StartedAt
        txn.Status.StartedAt = &metav1.Time{Time: time.Now().Add(-20 * time.Minute)}
        txn.Status.Version++
        Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

        // Next reconcile should detect timeout and transition to RollingBack
        _, err = reconciler.Reconcile(ctx, req)
        Expect(err).NotTo(HaveOccurred())

        Expect(k8sClient.Get(ctx, req.NamespacedName, txn)).To(Succeed())
        Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))
    })
})
```

**Step 3: Run test to verify it fails**

Run: `make test`
Expected: FAIL — timeout check doesn't exist yet, transactions proceed normally

**Step 4: Implement timeout check**

Add `defaultTransactionTimeout` constant and `transactionTimeout` helper
in `internal/controller/transaction_controller.go`:

```go
const (
	defaultTimeout            = 5 * time.Minute
	defaultTransactionTimeout = 30 * time.Minute
	rollbackCMSuffix          = "-rollback"
	finalizerName             = "backup.janus.io/lease-cleanup"
)

func (r *TransactionReconciler) transactionTimeout(txn *backupv1alpha1.Transaction) time.Duration {
	if txn.Spec.Timeout != nil {
		return txn.Spec.Timeout.Duration
	}
	return defaultTransactionTimeout
}
```

Add the timeout check in `Reconcile`, after the terminal-phase switch and
finalizer check (around line 130), before the phase dispatch:

```go
// Check transaction-level timeout for non-terminal, in-progress phases.
if txn.Status.StartedAt != nil && !isTerminalPhase(txn.Status.Phase) {
    deadline := txn.Status.StartedAt.Time.Add(r.transactionTimeout(&txn))
    if time.Now().After(deadline) {
        elapsed := time.Since(txn.Status.StartedAt.Time).Round(time.Second)
        if r.hasUnrolledCommits(&txn) {
            log.Info("transaction timed out with committed items, initiating rollback", "elapsed", elapsed)
            r.event(&txn, corev1.EventTypeWarning, "Timeout", "transaction timed out after %s, rolling back", elapsed)
            return r.transition(ctx, &txn, backupv1alpha1.TransactionPhaseRollingBack)
        }
        log.Info("transaction timed out with no commits", "elapsed", elapsed)
        return ctrl.Result{}, r.failAndReleaseLocks(ctx, &txn, fmt.Sprintf("transaction timed out after %s", elapsed))
    }
}
```

**Step 5: Run test to verify it passes**

Run: `make test`
Expected: PASS

**Step 6: Commit**

```
git add api/v1alpha1/transaction_types.go api/v1alpha1/zz_generated.deepcopy.go \
        config/crd/bases/backup.janus.io_transactions.yaml \
        internal/controller/transaction_controller.go \
        internal/controller/transaction_controller_test.go
git commit -m "Add transaction-level timeout with 30m default"
```

---

## Task 3: Validation Webhook

The largest feature. Creates new files and modifies kustomize config.

**Files:**
- Create: `api/v1alpha1/transaction_webhook.go`
- Create: `api/v1alpha1/transaction_webhook_test.go`
- Modify: `cmd/main.go` (~line 134, register webhook)
- Create: `config/webhook/manifests.yaml` (generated by controller-gen)
- Create: `config/webhook/kustomization.yaml`
- Create: `config/webhook/kustomizeconfig.yaml`
- Create: `config/certmanager/certificate.yaml`
- Create: `config/certmanager/kustomization.yaml`
- Create: `config/certmanager/kustomizeconfig.yaml`
- Modify: `config/default/kustomization.yaml` (enable webhook + certmanager patches)
- Modify: `config/crd/kustomization.yaml` (enable cainjection patch)

### Step 1: Write the webhook validation logic and test

Create `api/v1alpha1/transaction_webhook.go`:

```go
package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var _ webhook.CustomValidator = &TransactionCustomValidator{}

// TransactionCustomValidator implements admission validation for Transaction.
type TransactionCustomValidator struct{}

// SetupWebhookWithManager registers the validating webhook with the manager.
func SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&Transaction{}).
		WithValidator(&TransactionCustomValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-backup-janus-io-v1alpha1-transaction,mutating=false,failurePolicy=fail,sideEffects=None,groups=backup.janus.io,resources=transactions,verbs=create;update,versions=v1alpha1,name=vtransaction-v1alpha1.kb.io,admissionReviewVersions=v1

func (v *TransactionCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	txn := obj.(*Transaction)
	return nil, validateTransactionSpec(txn, nil)
}

func (v *TransactionCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldTxn := oldObj.(*Transaction)
	newTxn := newObj.(*Transaction)
	return nil, validateTransactionSpec(newTxn, oldTxn)
}

func (v *TransactionCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateTransactionSpec(txn *Transaction, old *Transaction) error {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Immutability: reject spec changes after processing begins.
	if old != nil && old.Status.Phase != "" && old.Status.Phase != TransactionPhasePending {
		if !specsEqual(old, txn) {
			allErrs = append(allErrs, field.Forbidden(specPath,
				"spec is immutable once the transaction has left Pending phase"))
			return apierrors.NewInvalid(
				schema.GroupKind{Group: GroupVersion.Group, Kind: "Transaction"},
				txn.Name, allErrs)
		}
	}

	changesPath := specPath.Child("changes")
	seen := make(map[string]int) // target key → index (for duplicate detection)

	for i, change := range txn.Spec.Changes {
		changePath := changesPath.Index(i)
		targetPath := changePath.Child("target")

		// Target fields non-empty.
		if change.Target.APIVersion == "" {
			allErrs = append(allErrs, field.Required(targetPath.Child("apiVersion"), ""))
		}
		if change.Target.Kind == "" {
			allErrs = append(allErrs, field.Required(targetPath.Child("kind"), ""))
		}
		if change.Target.Name == "" {
			allErrs = append(allErrs, field.Required(targetPath.Child("name"), ""))
		}

		// Content required for Create/Update/Patch, forbidden for Delete.
		switch change.Type {
		case ChangeTypeCreate, ChangeTypeUpdate, ChangeTypePatch:
			if len(change.Content.Raw) == 0 {
				allErrs = append(allErrs, field.Required(changePath.Child("content"),
					fmt.Sprintf("content is required for %s", change.Type)))
			}
		case ChangeTypeDelete:
			if len(change.Content.Raw) > 0 {
				allErrs = append(allErrs, field.Forbidden(changePath.Child("content"),
					"content must be empty for Delete"))
			}
		}

		// Duplicate target detection.
		ns := change.Target.Namespace
		if ns == "" {
			ns = txn.Namespace
		}
		key := fmt.Sprintf("%s/%s/%s/%s", change.Target.APIVersion, change.Target.Kind, ns, change.Target.Name)
		if prev, exists := seen[key]; exists {
			allErrs = append(allErrs, field.Duplicate(targetPath,
				fmt.Sprintf("same target as changes[%d]", prev)))
		}
		seen[key] = i
	}

	if len(allErrs) > 0 {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: GroupVersion.Group, Kind: "Transaction"},
			txn.Name, allErrs)
	}
	return nil
}

// specsEqual compares the specs of two Transactions for the immutability check.
func specsEqual(a, b *Transaction) bool {
	if a.Spec.ServiceAccountName != b.Spec.ServiceAccountName {
		return false
	}
	if len(a.Spec.Changes) != len(b.Spec.Changes) {
		return false
	}
	for i := range a.Spec.Changes {
		ac, bc := a.Spec.Changes[i], b.Spec.Changes[i]
		if ac.Type != bc.Type {
			return false
		}
		if ac.Target != bc.Target {
			return false
		}
		if string(ac.Content.Raw) != string(bc.Content.Raw) {
			return false
		}
	}
	// Compare optional duration fields.
	switch {
	case a.Spec.LockTimeout == nil && b.Spec.LockTimeout == nil:
	case a.Spec.LockTimeout != nil && b.Spec.LockTimeout != nil:
		if a.Spec.LockTimeout.Duration != b.Spec.LockTimeout.Duration {
			return false
		}
	default:
		return false
	}
	switch {
	case a.Spec.Timeout == nil && b.Spec.Timeout == nil:
	case a.Spec.Timeout != nil && b.Spec.Timeout != nil:
		if a.Spec.Timeout.Duration != b.Spec.Timeout.Duration {
			return false
		}
	default:
		return false
	}
	return true
}
```

### Step 2: Write the webhook unit tests

Create `api/v1alpha1/transaction_webhook_test.go`:

```go
package v1alpha1

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func validTxn() *Transaction {
	return &Transaction{
		ObjectMeta: metav1.ObjectMeta{Name: "test-txn", Namespace: "default"},
		Spec: TransactionSpec{
			ServiceAccountName: "test-sa",
			Changes: []ResourceChange{{
				Target: ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "my-cm",
				},
				Type:    ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap"}`)},
			}},
		},
	}
}

func TestValidateCreate_ValidSpec(t *testing.T) {
	v := &TransactionCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), validTxn())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidateCreate_MissingContent(t *testing.T) {
	v := &TransactionCustomValidator{}
	txn := validTxn()
	txn.Spec.Changes[0].Content = runtime.RawExtension{}

	_, err := v.ValidateCreate(context.Background(), txn)
	if err == nil {
		t.Fatal("expected error for missing content on Create")
	}
}

func TestValidateCreate_DeleteWithContent(t *testing.T) {
	v := &TransactionCustomValidator{}
	txn := validTxn()
	txn.Spec.Changes[0].Type = ChangeTypeDelete

	_, err := v.ValidateCreate(context.Background(), txn)
	if err == nil {
		t.Fatal("expected error for Delete with content")
	}
}

func TestValidateCreate_DeleteWithoutContent(t *testing.T) {
	v := &TransactionCustomValidator{}
	txn := validTxn()
	txn.Spec.Changes[0].Type = ChangeTypeDelete
	txn.Spec.Changes[0].Content = runtime.RawExtension{}

	_, err := v.ValidateCreate(context.Background(), txn)
	if err != nil {
		t.Fatalf("expected no error for Delete without content, got %v", err)
	}
}

func TestValidateCreate_EmptyTargetFields(t *testing.T) {
	v := &TransactionCustomValidator{}
	txn := validTxn()
	txn.Spec.Changes[0].Target = ResourceRef{}

	_, err := v.ValidateCreate(context.Background(), txn)
	if err == nil {
		t.Fatal("expected error for empty target fields")
	}
}

func TestValidateCreate_DuplicateTargets(t *testing.T) {
	v := &TransactionCustomValidator{}
	txn := validTxn()
	txn.Spec.Changes = append(txn.Spec.Changes, txn.Spec.Changes[0])

	_, err := v.ValidateCreate(context.Background(), txn)
	if err == nil {
		t.Fatal("expected error for duplicate targets")
	}
}

func TestValidateUpdate_ImmutableAfterProcessing(t *testing.T) {
	v := &TransactionCustomValidator{}
	oldTxn := validTxn()
	oldTxn.Status.Phase = TransactionPhasePreparing

	newTxn := validTxn()
	newTxn.Spec.ServiceAccountName = "different-sa"

	_, err := v.ValidateUpdate(context.Background(), oldTxn, newTxn)
	if err == nil {
		t.Fatal("expected error for spec change after Pending phase")
	}
}

func TestValidateUpdate_AllowSpecChangeInPending(t *testing.T) {
	v := &TransactionCustomValidator{}
	oldTxn := validTxn()
	oldTxn.Status.Phase = TransactionPhasePending

	newTxn := validTxn()
	newTxn.Spec.ServiceAccountName = "different-sa"

	_, err := v.ValidateUpdate(context.Background(), oldTxn, newTxn)
	if err != nil {
		t.Fatalf("expected no error for spec change in Pending, got %v", err)
	}
}

func TestValidateUpdate_AllowStatusOnlyChange(t *testing.T) {
	v := &TransactionCustomValidator{}
	oldTxn := validTxn()
	oldTxn.Status.Phase = TransactionPhasePreparing

	newTxn := validTxn()
	newTxn.Status.Phase = TransactionPhasePrepared

	_, err := v.ValidateUpdate(context.Background(), oldTxn, newTxn)
	if err != nil {
		t.Fatalf("expected no error for status-only change, got %v", err)
	}
}
```

### Step 3: Run tests to verify they pass (pure validation logic, no infra yet)

Run: `go test ./api/v1alpha1/ -v`
Expected: PASS (the webhook logic is testable without the webhook server)

### Step 4: Commit validation logic

```
git add api/v1alpha1/transaction_webhook.go api/v1alpha1/transaction_webhook_test.go
git commit -m "Add Transaction validation webhook logic and tests"
```

### Step 5: Register webhook in main.go

In `cmd/main.go`, after the controller setup (after the
`// +kubebuilder:scaffold:builder` comment):

```go
if err := backupv1alpha1.SetupWebhookWithManager(mgr); err != nil {
    setupLog.Error(err, "unable to create webhook", "webhook", "Transaction")
    os.Exit(1)
}
```

### Step 6: Generate webhook manifests

Run: `make manifests`

This generates `config/webhook/manifests.yaml` from the kubebuilder marker
on the webhook.

### Step 7: Add kustomize webhook infrastructure

Create `config/webhook/kustomization.yaml`:
```yaml
resources:
  - manifests.yaml
```

Create `config/webhook/kustomizeconfig.yaml`:
```yaml
# the following config is for teaching kustomize where to look for webhook name references
nameReference:
  - kind: Service
    version: v1
    fieldSpecs:
      - kind: ValidatingWebhookConfiguration
        group: admissionregistration.k8s.io
        path: webhooks/clientConfig/service/name

namespace:
  - kind: ValidatingWebhookConfiguration
    group: admissionregistration.k8s.io
    path: webhooks/clientConfig/service/namespace
    create: true

varReference:
  - path: webhooks/clientConfig/service/namespace
    kind: ValidatingWebhookConfiguration
    group: admissionregistration.k8s.io
```

Create `config/webhook/service.yaml`:
```yaml
apiVersion: v1
kind: Service
metadata:
  name: webhook-service
  namespace: system
spec:
  ports:
    - port: 443
      protocol: TCP
      targetPort: 9443
  selector:
    control-plane: controller-manager
```

Update `config/webhook/kustomization.yaml` to include the service:
```yaml
resources:
  - manifests.yaml
  - service.yaml
configurations:
  - kustomizeconfig.yaml
```

Create `config/certmanager/certificate.yaml`:
```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: selfsigned-issuer
  namespace: system
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: serving-cert
  namespace: system
spec:
  dnsNames:
    - $(SERVICE_NAME).$(SERVICE_NAMESPACE).svc
    - $(SERVICE_NAME).$(SERVICE_NAMESPACE).svc.cluster.local
  issuerRef:
    kind: Issuer
    name: selfsigned-issuer
  secretName: webhook-server-cert
```

Create `config/certmanager/kustomization.yaml`:
```yaml
resources:
  - certificate.yaml
configurations:
  - kustomizeconfig.yaml
```

Create `config/certmanager/kustomizeconfig.yaml`:
```yaml
nameReference:
  - kind: Issuer
    group: cert-manager.io
    fieldSpecs:
      - kind: Certificate
        group: cert-manager.io
        path: spec/issuerRef/name

varReference:
  - path: spec/dnsNames
    kind: Certificate
    group: cert-manager.io
```

### Step 8: Wire into default kustomization

Modify `config/default/kustomization.yaml` to uncomment/add webhook and
cert-manager resources and patches. The exact edits depend on what kubebuilder
scaffolded — check the file and enable:
- `../webhook` resource
- `../certmanager` resource
- `manager_webhook_patch.yaml` (add webhook port to manager deployment)
- `webhookcainjection_patch.yaml` (inject cert-manager CA into webhook config)

Create `config/default/manager_webhook_patch.yaml`:
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
  namespace: system
spec:
  template:
    spec:
      containers:
        - name: manager
          ports:
            - containerPort: 9443
              name: webhook-server
              protocol: TCP
          volumeMounts:
            - mountPath: /tmp/k8s-webhook-server/serving-certs
              name: cert
              readOnly: true
      volumes:
        - name: cert
          secret:
            defaultMode: 420
            secretName: webhook-server-cert
```

Create `config/default/webhookcainjection_patch.yaml`:
```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: validating-webhook-configuration
  annotations:
    cert-manager.io/inject-ca-from: $(CERTIFICATE_NAMESPACE)/$(CERTIFICATE_NAME)
```

### Step 9: Enable CRD CA injection

In `config/crd/kustomization.yaml`, uncomment `cainjection` patches if
present, or add:
```yaml
configurations:
  - kustomizeconfig.yaml
```

### Step 10: Verify build and manifests

Run:
```
make manifests generate
make build
```
Expected: clean build, generated webhook config in `config/webhook/manifests.yaml`

### Step 11: Commit webhook infrastructure

```
git add cmd/main.go \
        config/webhook/ config/certmanager/ \
        config/default/kustomization.yaml \
        config/default/manager_webhook_patch.yaml \
        config/default/webhookcainjection_patch.yaml \
        config/crd/kustomization.yaml \
        api/v1alpha1/transaction_webhook.go
git commit -m "Add webhook infrastructure with cert-manager TLS"
```

---

## Task 4: Update TODO.md

**Files:**
- Modify: `TODO.md`

Remove the three completed items. If nothing remains, delete the file or
leave a placeholder.

**Step 1: Update TODO.md**

```
git add TODO.md
git commit -m "Remove completed items from TODO"
```

---

## Execution Order

Tasks 1 and 2 are independent and can be implemented in parallel.
Task 3 depends on Task 2 (the `Timeout` field must exist for `specsEqual`).
Task 4 is a final cleanup.

```
Task 1 (conditions) ─────────────────┐
                                      ├─→ Task 3 (webhook) → Task 4 (cleanup)
Task 2 (timeout) ────────────────────┘
```

**Step 5: Run tests to verify it passes**

Run: `make test`
Expected: PASS

**Step 6: Commit**

```
git add internal/controller/transaction_controller.go internal/controller/transaction_controller_test.go
git commit -m "Add per-phase status conditions (Prepared, Committed, RolledBack)"
```
