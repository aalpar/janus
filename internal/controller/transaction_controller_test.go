/*
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
*/

package controller

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
	"github.com/aalpar/janus/internal/lock"
	txmetrics "github.com/aalpar/janus/internal/metrics"
	"github.com/aalpar/janus/internal/rollback"
)

const (
	testNamespace = "default"
	testSAName    = "janus-test-sa"
	unprivSAName  = "janus-unpriv-sa"
)

// fakeLockMgr is a controllable mock for lock.Manager used to trigger rollback paths.
type fakeLockMgr struct {
	acquireFn    func(ctx context.Context, key lock.ResourceKey, txnName string, timeout time.Duration) (string, error)
	releaseFn    func(ctx context.Context, lease lock.LeaseRef, txnName string) error
	releaseAllFn func(ctx context.Context, leases []lock.LeaseRef, txnName string) error
	renewFn      func(ctx context.Context, lease lock.LeaseRef, txnName string, timeout time.Duration) error
}

func (f *fakeLockMgr) Acquire(ctx context.Context, key lock.ResourceKey, txnName string, timeout time.Duration) (string, error) {
	if f.acquireFn != nil {
		return f.acquireFn(ctx, key, txnName, timeout)
	}
	return lock.LeaseName(key), nil
}

func (f *fakeLockMgr) Release(ctx context.Context, lease lock.LeaseRef, txnName string) error {
	if f.releaseFn != nil {
		return f.releaseFn(ctx, lease, txnName)
	}
	return nil
}

func (f *fakeLockMgr) ReleaseAll(ctx context.Context, leases []lock.LeaseRef, txnName string) error {
	if f.releaseAllFn != nil {
		return f.releaseAllFn(ctx, leases, txnName)
	}
	return nil
}

func (f *fakeLockMgr) Renew(ctx context.Context, lease lock.LeaseRef, txnName string, timeout time.Duration) error {
	if f.renewFn != nil {
		return f.renewFn(ctx, lease, txnName, timeout)
	}
	return nil
}

var _ = Describe("Transaction Controller", func() {
	const (
		txnName  = "test-txn"
		timeout  = 10 * time.Second
		interval = 250 * time.Millisecond
	)

	var (
		reconciler *TransactionReconciler
	)

	BeforeEach(func() {
		reconciler = &TransactionReconciler{
			Client:  k8sClient,
			Scheme:  k8sClient.Scheme(),
			BaseCfg: cfg,
			Mapper:  testMapper,
			LockMgr: &lock.LeaseManager{Client: k8sClient},
		}
		txmetrics.PhaseTransitions.Reset()
		txmetrics.Duration.Reset()
		txmetrics.ActiveTransactions.Reset()
		txmetrics.ItemOperations.Reset()
		txmetrics.LockOperations.Reset()
		// ItemCount is prometheus.Histogram (interface) — no Reset(). Not asserted in tests.
	})

	AfterEach(func() {
		// Clean up transactions — strip finalizers first so envtest GC can proceed.
		txnList := &backupv1alpha1.TransactionList{}
		Expect(k8sClient.List(ctx, txnList, client.InNamespace(testNamespace))).To(Succeed())
		for i := range txnList.Items {
			t := &txnList.Items[i]
			changed := controllerutil.RemoveFinalizer(t, finalizerName)
			changed = controllerutil.RemoveFinalizer(t, rollbackProtectionFinalizer) || changed
			if changed {
				Expect(k8sClient.Update(ctx, t)).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, t))).To(Succeed())
		}

		// Clean up ResourceChanges.
		rcList := &backupv1alpha1.ResourceChangeList{}
		Expect(k8sClient.List(ctx, rcList, client.InNamespace(testNamespace))).To(Succeed())
		for i := range rcList.Items {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &rcList.Items[i]))).To(Succeed())
		}

		// Clean up ConfigMaps created by the controller.
		cmList := &corev1.ConfigMapList{}
		Expect(k8sClient.List(ctx, cmList, client.InNamespace(testNamespace),
			client.MatchingLabels{"app.kubernetes.io/managed-by": "janus"})).To(Succeed())
		for i := range cmList.Items {
			Expect(k8sClient.Delete(ctx, &cmList.Items[i])).To(Succeed())
		}

		// Clean up Leases created by the lock manager.
		leaseList := &coordinationv1.LeaseList{}
		Expect(k8sClient.List(ctx, leaseList, client.InNamespace(testNamespace),
			client.MatchingLabels{"app.kubernetes.io/managed-by": "janus"})).To(Succeed())
		for i := range leaseList.Items {
			Expect(k8sClient.Delete(ctx, &leaseList.Items[i])).To(Succeed())
		}
	})

	Context("when creating a Transaction to create a ConfigMap", func() {
		It("should progress through phases to Committed", func() {
			cmContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "txn-created-cm",
					"namespace": testNamespace,
				},
				"data": map[string]any{
					"key": "value",
				},
			}
			raw, err := json.Marshal(cmContent)
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      txnName,
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange(txnName, "txn-created-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "txn-created-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Reconcile: adds rollback-protection finalizer.
			result, err := reconciler.Reconcile(ctx, req(txnName))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Expect(k8sClient.Get(ctx, nn(txnName), txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(rollbackProtectionFinalizer))

			// Reconcile: adds lease-cleanup finalizer.
			result, err = reconciler.Reconcile(ctx, req(txnName))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Expect(k8sClient.Get(ctx, nn(txnName), txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(finalizerName))

			// Reconcile: Pending → Preparing
			result, err = reconciler.Reconcile(ctx, req(txnName))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Re-fetch to see updated status.
			Expect(k8sClient.Get(ctx, nn(txnName), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhasePreparing))

			// Reconcile: Preparing → Prepared (single item, no prior state for Create)
			result, err = reconciler.Reconcile(ctx, req(txnName))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn(txnName), txn)).To(Succeed())
			// Should be Prepared or already Committing after the lock+prepare step.
			Expect(txn.Status.Items[0].Prepared).To(BeTrue())

			// Keep reconciling until committed.
			for range 10 {
				Expect(k8sClient.Get(ctx, nn(txnName), txn)).To(Succeed())
				if txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted {
					break
				}
				_, err = reconciler.Reconcile(ctx, req(txnName))
				Expect(err).NotTo(HaveOccurred())
			}

			Expect(k8sClient.Get(ctx, nn(txnName), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))
			Expect(txn.Status.Items[0].Committed).To(BeTrue())

			// Verify metrics.
			Expect(testutil.ToFloat64(txmetrics.PhaseTransitions.WithLabelValues("Pending", "Preparing"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.PhaseTransitions.WithLabelValues("Preparing", "Prepared"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.PhaseTransitions.WithLabelValues("Prepared", "Committing"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.PhaseTransitions.WithLabelValues("Committing", "Committed"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.ItemOperations.WithLabelValues("prepare", "success"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.ItemOperations.WithLabelValues("commit", "success"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.LockOperations.WithLabelValues("acquire", "success"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.LockOperations.WithLabelValues("renew", "success"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.LockOperations.WithLabelValues("release", "success"))).To(Equal(1.0))

			// Verify the ConfigMap was actually created.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("txn-created-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("value"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("when updating an existing ConfigMap", func() {
		It("should update the resource and be able to rollback", func() {
			// Create the target ConfigMap first.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "existing-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"original": "data"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, err := json.Marshal(map[string]any{
				"data": map[string]any{
					"original": "modified",
					"added":    "new-value",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "update-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("update-txn", "existing-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "existing-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Reconcile through all phases.
			for range 15 {
				Expect(k8sClient.Get(ctx, nn("update-txn"), txn)).To(Succeed())
				if txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted {
					break
				}
				_, err = reconciler.Reconcile(ctx, req("update-txn"))
				Expect(err).NotTo(HaveOccurred())
			}

			Expect(k8sClient.Get(ctx, nn("update-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))

			// Verify the ConfigMap was patched.
			Expect(k8sClient.Get(ctx, nn("existing-cm"), cm)).To(Succeed())
			Expect(cm.Data["original"]).To(Equal("modified"))
			Expect(cm.Data["added"]).To(Equal("new-value"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("when a transaction is unsealed", func() {
		It("should be ignored by the controller", func() {
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "unsealed-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             false,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// First reconcile adds rollback-protection finalizer.
			result, err := reconciler.Reconcile(ctx, req("unsealed-txn"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Expect(k8sClient.Get(ctx, nn("unsealed-txn"), txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(rollbackProtectionFinalizer))

			// Second reconcile: unsealed → no-op.
			result, err = reconciler.Reconcile(ctx, req("unsealed-txn"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// Verify no phase change.
			Expect(k8sClient.Get(ctx, nn("unsealed-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(BeEmpty())
		})
	})

	Context("when deleting an existing ConfigMap", func() {
		It("should delete the resource and reach Committed", func() {
			// Create the target ConfigMap.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "delete-target-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"keep": "me"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "delete-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("delete-txn", "delete-target-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "delete-target-cm",
					Namespace:  testNamespace,
				},
				Type: backupv1alpha1.ChangeTypeDelete,
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			reconcileToPhase(reconciler, "delete-txn", backupv1alpha1.TransactionPhaseCommitted)

			// Verify the ConfigMap is gone.
			err := k8sClient.Get(ctx, nn("delete-target-cm"), &corev1.ConfigMap{})
			Expect(err).To(HaveOccurred())
			Expect(client.IgnoreNotFound(err)).To(Succeed())
		})
	})

	Context("when updating an existing ConfigMap (full replace)", func() {
		It("should fully replace the resource data", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "update-replace-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"old-key": "old-val"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			updateContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "update-replace-cm",
					"namespace": testNamespace,
				},
				"data": map[string]any{
					"new-key": "new-val",
				},
			}
			raw, err := json.Marshal(updateContent)
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "update-replace-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("update-replace-txn", "update-replace-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "update-replace-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeUpdate,
				Content: runtime.RawExtension{Raw: raw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			reconcileToPhase(reconciler, "update-replace-txn", backupv1alpha1.TransactionPhaseCommitted)

			// Verify full replacement.
			Expect(k8sClient.Get(ctx, nn("update-replace-cm"), cm)).To(Succeed())
			Expect(cm.Data).To(HaveKey("new-key"))
			Expect(cm.Data).NotTo(HaveKey("old-key"))

			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("when a lock is lost during committing (triggers rollback of committed items)", func() {
		It("should commit item 0, fail on item 1, then rollback item 0 via applyRollback", func() {
			// Item 0: Patch an existing ConfigMap (will be committed, then rolled back).
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-patch-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, err := json.Marshal(map[string]any{
				"data": map[string]any{
					"key": "patched",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-multi-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("rb-multi-txn", "rb-patch-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rb-patch-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("rb-multi-txn", "rb-new-cm", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			// Use real lock manager through prepare and first commit.
			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Reconcile through Pending → Preparing → Prepared → Committing.
			reconcileToPhase(reconciler, "rb-multi-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Reconcile once more: commits item 0 (patch).
			_, err = reconciler.Reconcile(ctx, req("rb-multi-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Verify item 0 is committed.
			Expect(k8sClient.Get(ctx, nn("rb-multi-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(txn.Status.Items[1].Committed).To(BeFalse())

			// Verify the ConfigMap was actually patched.
			Expect(k8sClient.Get(ctx, nn("rb-patch-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("patched"))

			// Now swap to fake lock manager: item 1's lock check fails.
			reconciler.LockMgr = failingRenewLockMgr()

			// Reconcile: Committing → RollingBack (item 1 lock check fails).
			_, err = reconciler.Reconcile(ctx, req("rb-multi-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("rb-multi-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Reconcile: RollingBack → rolls back item 0's patch → RolledBack.
			reconcileToPhase(reconciler, "rb-multi-txn", backupv1alpha1.TransactionPhaseRolledBack)

			Expect(k8sClient.Get(ctx, nn("rb-multi-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRolledBack))
			Expect(txn.Status.CompletedAt).NotTo(BeNil())
			Expect(txn.Status.Items[0].RolledBack).To(BeTrue())

			// Verify metrics: commit success for item 0, lock renew error for item 1, rollback success.
			Expect(testutil.ToFloat64(txmetrics.ItemOperations.WithLabelValues("commit", "success"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.LockOperations.WithLabelValues("renew", "error"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.ItemOperations.WithLabelValues("rollback", "success"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.PhaseTransitions.WithLabelValues("Committing", "RollingBack"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.PhaseTransitions.WithLabelValues("RollingBack", "RolledBack"))).To(Equal(1.0))

			// Verify the ConfigMap was restored to its original state.
			Expect(k8sClient.Get(ctx, nn("rb-patch-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("original"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("when lock acquisition fails during preparing", func() {
		It("should fail the transaction and release acquired locks", func() {
			// Use a fake lock manager that fails on the first Acquire.
			calls := 0
			reconciler.LockMgr = &fakeLockMgr{
				acquireFn: func(_ context.Context, key lock.ResourceKey, _ string, _ time.Duration) (string, error) {
					calls++
					return "", &lock.ErrAlreadyLocked{LeaseName: lock.LeaseName(key), Holder: "other-txn"}
				},
			}

			cmContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "lock-fail-cm", "namespace": testNamespace},
				"data":       map[string]any{"k": "v"},
			}
			raw, _ := json.Marshal(cmContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "lock-fail-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("lock-fail-txn", "lock-fail-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "lock-fail-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Drive through finalizers + phases until Preparing, then let lock fail.
			reconcileToPhase(reconciler, "lock-fail-txn", backupv1alpha1.TransactionPhasePreparing)

			// Preparing → Failed (lock acquisition fails, triggers failAndReleaseLocks).
			_, err := reconciler.Reconcile(ctx, req("lock-fail-txn"))
			// setFailed returns the status update error, which should be nil on success.
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("lock-fail-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))
			Expect(txn.Status.CompletedAt).NotTo(BeNil())
			Expect(txn.Status.Conditions).NotTo(BeEmpty())
			Expect(txn.Status.Conditions[0].Type).To(Equal("Failed"))

			// Verify metrics: lock acquire error and prepare error recorded.
			Expect(testutil.ToFloat64(txmetrics.LockOperations.WithLabelValues("acquire", "error"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.ItemOperations.WithLabelValues("prepare", "error"))).To(Equal(1.0))
			Expect(testutil.ToFloat64(txmetrics.PhaseTransitions.WithLabelValues("Preparing", "Failed"))).To(Equal(1.0))
		})
	})

	Context("lease contention between two transactions", func() {
		It("should fail the second transaction when the first holds the lock", func() {
			// Target resource both transactions will contend over.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "contention-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "contention-cm", "namespace": testNamespace},
				"data":       map[string]any{"key": "patched"},
			}
			raw, err := json.Marshal(patchContent)
			Expect(err).NotTo(HaveOccurred())

			changeSpec := backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "contention-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: raw},
			}

			// Transaction A: prepare with real lock manager → acquires lease.
			txnA := minimalTxn("contention-txn-a")
			Expect(k8sClient.Create(ctx, txnA)).To(Succeed())
			rcA := createChange("contention-txn-a", "contention-cm-change-a", changeSpec, txnA.UID)
			Expect(k8sClient.Create(ctx, rcA)).To(Succeed())

			reconcileToPhase(reconciler, "contention-txn-a", backupv1alpha1.TransactionPhasePrepared)

			// Verify the lease exists and is held by txn A.
			lease := &coordinationv1.Lease{}
			leaseName := lock.LeaseName(lock.ResourceKey{
				Namespace: testNamespace, Kind: "ConfigMap", Name: "contention-cm",
			})
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: leaseName, Namespace: testNamespace,
			}, lease)).To(Succeed())
			Expect(*lease.Spec.HolderIdentity).To(Equal("contention-txn-a"))

			// Transaction B: targets the same resource → lock acquisition should fail.
			txnB := minimalTxn("contention-txn-b")
			Expect(k8sClient.Create(ctx, txnB)).To(Succeed())
			rcB := createChange("contention-txn-b", "contention-cm-change-b", changeSpec, txnB.UID)
			Expect(k8sClient.Create(ctx, rcB)).To(Succeed())

			// Drive B through to Preparing, then let it hit the lock.
			reconcileToPhase(reconciler, "contention-txn-b", backupv1alpha1.TransactionPhasePreparing)
			_, err = reconciler.Reconcile(ctx, req("contention-txn-b"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("contention-txn-b"), txnB)).To(Succeed())
			Expect(txnB.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))

			// Transaction A should still be Prepared, unaffected.
			Expect(k8sClient.Get(ctx, nn("contention-txn-a"), txnA)).To(Succeed())
			Expect(txnA.Status.Phase).To(Equal(backupv1alpha1.TransactionPhasePrepared))

			// Clean up: commit A so leases are released, then delete CM.
			reconcileToPhase(reconciler, "contention-txn-a", backupv1alpha1.TransactionPhaseCommitted)
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("lease expiry and takeover", func() {
		It("should roll back txn A when txn B takes over its expired lease", func() {
			// Target resource both transactions will contend over.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "expiry-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "expiry-cm", "namespace": testNamespace},
				"data":       map[string]any{"key": "patched"},
			}
			raw, err := json.Marshal(patchContent)
			Expect(err).NotTo(HaveOccurred())

			changeSpec := backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "expiry-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: raw},
			}

			// Transaction A: prepare → Prepared (acquires lease via real lock manager).
			txnA := minimalTxn("expiry-txn-a")
			Expect(k8sClient.Create(ctx, txnA)).To(Succeed())
			rcA := createChange("expiry-txn-a", "expiry-cm-change-a", changeSpec, txnA.UID)
			Expect(k8sClient.Create(ctx, rcA)).To(Succeed())

			reconcileToPhase(reconciler, "expiry-txn-a", backupv1alpha1.TransactionPhasePrepared)

			// Verify the lease exists and is held by txn A.
			leaseName := lock.LeaseName(lock.ResourceKey{
				Namespace: testNamespace, Kind: "ConfigMap", Name: "expiry-cm",
			})
			lease := &coordinationv1.Lease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: leaseName, Namespace: testNamespace,
			}, lease)).To(Succeed())
			Expect(*lease.Spec.HolderIdentity).To(Equal("expiry-txn-a"))

			// Backdate the lease so it appears expired.
			pastTime := metav1.NewMicroTime(time.Now().Add(-10 * time.Second))
			durationSec := int32(1)
			lease.Spec.RenewTime = &pastTime
			lease.Spec.LeaseDurationSeconds = &durationSec
			Expect(k8sClient.Update(ctx, lease)).To(Succeed())

			// Transaction B: targets the same resource → should take over the expired lease.
			txnB := minimalTxn("expiry-txn-b")
			Expect(k8sClient.Create(ctx, txnB)).To(Succeed())
			rcB := createChange("expiry-txn-b", "expiry-cm-change-b", changeSpec, txnB.UID)
			Expect(k8sClient.Create(ctx, rcB)).To(Succeed())

			reconcileToPhase(reconciler, "expiry-txn-b", backupv1alpha1.TransactionPhasePrepared)

			// Verify the lease is now held by txn B.
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: leaseName, Namespace: testNamespace,
			}, lease)).To(Succeed())
			Expect(*lease.Spec.HolderIdentity).To(Equal("expiry-txn-b"))

			// Transaction A: Prepared → Committing → Renew fails (holder is B) → RollingBack → RolledBack.
			reconcileToPhase(reconciler, "expiry-txn-a", backupv1alpha1.TransactionPhaseRolledBack)

			// Assert: A is RolledBack, B is Prepared, lease held by B.
			Expect(k8sClient.Get(ctx, nn("expiry-txn-a"), txnA)).To(Succeed())
			Expect(txnA.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRolledBack))

			Expect(k8sClient.Get(ctx, nn("expiry-txn-b"), txnB)).To(Succeed())
			Expect(txnB.Status.Phase).To(Equal(backupv1alpha1.TransactionPhasePrepared))

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: leaseName, Namespace: testNamespace,
			}, lease)).To(Succeed())
			Expect(*lease.Spec.HolderIdentity).To(Equal("expiry-txn-b"))

			// Clean up: commit B, delete CM.
			reconcileToPhase(reconciler, "expiry-txn-b", backupv1alpha1.TransactionPhaseCommitted)
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("when rolling back a Create (reverse = Delete the created resource)", func() {
		It("should delete the created resource during rollback", func() {
			// Item 0: Create a new ConfigMap (will be committed).
			createContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "rb-created-cm",
					"namespace": testNamespace,
				},
				"data": map[string]any{"k": "created"},
			}
			createRaw, _ := json.Marshal(createContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-create-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("rb-create-txn", "rb-created-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rb-created-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: createRaw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("rb-create-txn", "rb-created-cm-2", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			// Use real lock manager through prepare and first commit.
			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Reconcile to Committing.
			reconcileToPhase(reconciler, "rb-create-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, req("rb-create-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Verify item 0 created the ConfigMap.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("rb-created-cm"), cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("created"))

			// Fail item 1's lock check.
			reconciler.LockMgr = failingRenewLockMgr()

			// Committing → RollingBack.
			_, err = reconciler.Reconcile(ctx, req("rb-create-txn"))
			Expect(err).NotTo(HaveOccurred())

			// RollingBack → RolledBack (reverses item 0's Create by Deleting).
			reconcileToPhase(reconciler, "rb-create-txn", backupv1alpha1.TransactionPhaseRolledBack)

			// Verify the created ConfigMap was deleted during rollback.
			err = k8sClient.Get(ctx, nn("rb-created-cm"), cm)
			Expect(err).To(HaveOccurred())
			Expect(client.IgnoreNotFound(err)).To(Succeed())
		})
	})

	Context("when rolling back a Delete (reverse = re-Create from rollback state)", func() {
		It("should re-create the deleted resource during rollback", func() {
			// Create the ConfigMap that will be deleted then restored.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-deleted-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"preserved": "data"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-delete-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("rb-delete-txn", "rb-deleted-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rb-deleted-cm",
					Namespace:  testNamespace,
				},
				Type: backupv1alpha1.ChangeTypeDelete,
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("rb-delete-txn", "rb-delete-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Reconcile to Committing.
			reconcileToPhase(reconciler, "rb-delete-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0 (Delete).
			_, err := reconciler.Reconcile(ctx, req("rb-delete-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Verify ConfigMap was deleted.
			err = k8sClient.Get(ctx, nn("rb-deleted-cm"), cm)
			Expect(err).To(HaveOccurred())
			Expect(client.IgnoreNotFound(err)).To(Succeed())

			// Fail item 1's lock check.
			reconciler.LockMgr = failingRenewLockMgr()

			// Committing → RollingBack.
			_, err = reconciler.Reconcile(ctx, req("rb-delete-txn"))
			Expect(err).NotTo(HaveOccurred())

			// RollingBack → RolledBack (reverses item 0's Delete by re-Creating).
			reconcileToPhase(reconciler, "rb-delete-txn", backupv1alpha1.TransactionPhaseRolledBack)

			// Verify the ConfigMap was re-created from rollback state.
			Expect(k8sClient.Get(ctx, nn("rb-deleted-cm"), cm)).To(Succeed())
			Expect(cm.Data["preserved"]).To(Equal("data"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("when reconciling a non-existent transaction", func() {
		It("should return no error", func() {
			result, err := reconciler.Reconcile(ctx, req("does-not-exist"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("when a transaction is already in a terminal state", func() {
		It("should no-op for Committed", func() {
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "terminal-committed",
					Namespace:  testNamespace,
					Finalizers: []string{finalizerName, rollbackProtectionFinalizer},
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Manually set to Committed.
			txn.Status.Phase = backupv1alpha1.TransactionPhaseCommitted
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Reconcile strips both finalizers.
			result, err := reconciler.Reconcile(ctx, req("terminal-committed"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// Verify phase unchanged; lease-cleanup finalizer removed, rollback-protection remains (user-managed).
			Expect(k8sClient.Get(ctx, nn("terminal-committed"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))
			Expect(txn.Finalizers).NotTo(ContainElement(finalizerName))
			Expect(txn.Finalizers).To(ContainElement(rollbackProtectionFinalizer))
		})

		It("should no-op for RolledBack", func() {
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "terminal-rolledback",
					Namespace:  testNamespace,
					Finalizers: []string{finalizerName, rollbackProtectionFinalizer},
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			txn.Status.Phase = backupv1alpha1.TransactionPhaseRolledBack
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, req("terminal-rolledback"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("should no-op for Failed with no un-rolled-back commits", func() {
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "terminal-failed",
					Namespace:  testNamespace,
					Finalizers: []string{finalizerName, rollbackProtectionFinalizer},
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			txn.Status.Phase = backupv1alpha1.TransactionPhaseFailed
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, req("terminal-failed"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("when a transaction has multiple items", func() {
		It("should progress one item per reconcile and commit all", func() {
			cm1Content := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "multi-cm-1",
					"namespace": testNamespace,
				},
				"data": map[string]any{"k": "v1"},
			}
			cm2Content := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "multi-cm-2",
					"namespace": testNamespace,
				},
				"data": map[string]any{"k": "v2"},
			}
			raw1, _ := json.Marshal(cm1Content)
			raw2, _ := json.Marshal(cm2Content)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "multi-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc1 := createChange("multi-txn", "multi-cm-1-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "multi-cm-1",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw1},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc1)).To(Succeed())
			rc2 := createChange("multi-txn", "multi-cm-2-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "multi-cm-2",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw2},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc2)).To(Succeed())

			reconcileToPhase(reconciler, "multi-txn", backupv1alpha1.TransactionPhaseCommitted)

			// Verify both ConfigMaps were created.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("multi-cm-1"), cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("v1"))

			Expect(k8sClient.Get(ctx, nn("multi-cm-2"), cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("v2"))

			// Verify both items committed.
			Expect(k8sClient.Get(ctx, nn("multi-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items).To(HaveLen(2))
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(txn.Status.Items[1].Committed).To(BeTrue())

			// Verify metrics: 2 prepare successes, 2 commit successes, 2 lock acquires.
			Expect(testutil.ToFloat64(txmetrics.ItemOperations.WithLabelValues("prepare", "success"))).To(Equal(2.0))
			Expect(testutil.ToFloat64(txmetrics.ItemOperations.WithLabelValues("commit", "success"))).To(Equal(2.0))
			Expect(testutil.ToFloat64(txmetrics.LockOperations.WithLabelValues("acquire", "success"))).To(Equal(2.0))

			// Clean up.
			Expect(k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "multi-cm-1", Namespace: testNamespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "multi-cm-2", Namespace: testNamespace}})).To(Succeed())
		})
	})

	Context("when namespace is inherited from the transaction", func() {
		It("should use the transaction namespace when ResourceRef.Namespace is empty", func() {
			cmContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "ns-inherited-cm",
				},
				"data": map[string]any{"k": "v"},
			}
			raw, _ := json.Marshal(cmContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ns-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("ns-txn", "ns-inherited-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "ns-inherited-cm",
					// Namespace intentionally empty.
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			reconcileToPhase(reconciler, "ns-txn", backupv1alpha1.TransactionPhaseCommitted)

			// Verify the ConfigMap was created in the transaction's namespace.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("ns-inherited-cm"), cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("v"))

			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("finalizer lifecycle", func() {
		It("should add both finalizers on first reconciles", func() {
			txn := minimalTxn("fin-add")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createCMChange("fin-add", "fin-add-cm", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// First reconcile: rollback-protection.
			_, err := reconciler.Reconcile(ctx, req("fin-add"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("fin-add"), txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(rollbackProtectionFinalizer))

			// Second reconcile: lease-cleanup (after sealed check).
			_, err = reconciler.Reconcile(ctx, req("fin-add"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("fin-add"), txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(finalizerName))
		})

		It("should remove lease-cleanup finalizer at terminal states but keep rollback-protection", func() {
			txn := minimalTxn("fin-terminal")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createCMChange("fin-terminal", "fin-terminal-cm", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Add both finalizers.
			_, err := reconciler.Reconcile(ctx, req("fin-terminal"))
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, req("fin-terminal"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("fin-terminal"), txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(finalizerName))
			Expect(txn.Finalizers).To(ContainElement(rollbackProtectionFinalizer))

			// Manually set to Committed.
			txn.Status.Phase = backupv1alpha1.TransactionPhaseCommitted
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Reconcile: terminal → strip lease-cleanup, keep rollback-protection (user-managed).
			_, err = reconciler.Reconcile(ctx, req("fin-terminal"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("fin-terminal"), txn)).To(Succeed())
			Expect(txn.Finalizers).NotTo(ContainElement(finalizerName))
			Expect(txn.Finalizers).To(ContainElement(rollbackProtectionFinalizer))
		})

		It("should release leases on deletion during Preparing", func() {
			var releasedLeases []lock.LeaseRef
			reconciler.LockMgr = &fakeLockMgr{
				releaseAllFn: func(_ context.Context, leases []lock.LeaseRef, _ string) error {
					releasedLeases = leases
					return nil
				},
			}

			txn := minimalTxn("fin-del-prep")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createCMChange("fin-del-prep", "fin-del-prep-cm", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Add finalizer + Pending → Preparing.
			reconcileToPhase(reconciler, "fin-del-prep", backupv1alpha1.TransactionPhasePreparing)

			// One more reconcile to prepare items (acquire leases).
			_, err := reconciler.Reconcile(ctx, req("fin-del-prep"))
			Expect(err).NotTo(HaveOccurred())

			// Verify leases were acquired.
			Expect(k8sClient.Get(ctx, nn("fin-del-prep"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].LockLease).NotTo(BeEmpty())

			// Delete the transaction — finalizers prevent immediate removal.
			Expect(k8sClient.Delete(ctx, txn)).To(Succeed())

			// Reconcile: handleDeletion → release leases, remove lease-cleanup finalizer.
			_, err = reconciler.Reconcile(ctx, req("fin-del-prep"))
			Expect(err).NotTo(HaveOccurred())

			// Verify leases were released.
			Expect(releasedLeases).To(HaveLen(1))

			// rollback-protection remains — simulate user removing it.
			Expect(k8sClient.Get(ctx, nn("fin-del-prep"), txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(rollbackProtectionFinalizer))
			controllerutil.RemoveFinalizer(txn, rollbackProtectionFinalizer)
			Expect(k8sClient.Update(ctx, txn)).To(Succeed())

			// Object should now be gone.
			err = k8sClient.Get(ctx, nn("fin-del-prep"), txn)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("should handle deletion of Pending transaction gracefully", func() {
			reconciler.LockMgr = &fakeLockMgr{}

			txn := minimalTxn("fin-del-pending")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createCMChange("fin-del-pending", "fin-del-pending-cm", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Add rollback-protection finalizer.
			_, err := reconciler.Reconcile(ctx, req("fin-del-pending"))
			Expect(err).NotTo(HaveOccurred())

			// Delete before any leases are acquired.
			Expect(k8sClient.Get(ctx, nn("fin-del-pending"), txn)).To(Succeed())
			Expect(k8sClient.Delete(ctx, txn)).To(Succeed())

			// Reconcile: handleDeletion with no leases, no unrolled commits — removes lease-cleanup.
			_, err = reconciler.Reconcile(ctx, req("fin-del-pending"))
			Expect(err).NotTo(HaveOccurred())

			// rollback-protection remains — simulate user removing it.
			Expect(k8sClient.Get(ctx, nn("fin-del-pending"), txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(rollbackProtectionFinalizer))
			controllerutil.RemoveFinalizer(txn, rollbackProtectionFinalizer)
			Expect(k8sClient.Update(ctx, txn)).To(Succeed())

			// Object should now be gone.
			err = k8sClient.Get(ctx, nn("fin-del-pending"), txn)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("should add automatic-rollback annotation at seal time", func() {
			txn := minimalTxn("fin-auto-rb")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createCMChange("fin-auto-rb", "fin-auto-rb-cm", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Drive to Preparing (handlePending adds annotation).
			reconcileToPhase(reconciler, "fin-auto-rb", backupv1alpha1.TransactionPhasePreparing)

			Expect(k8sClient.Get(ctx, nn("fin-auto-rb"), txn)).To(Succeed())
			Expect(txn.Annotations).To(HaveKey(annotationAutoRollback))
		})
	})

	Context("safe deletion with rollback", func() {
		It("should trigger rollback when deleted during Committing with unrolled commits", func() {
			// Create target ConfigMap.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "del-commit-cm", Namespace: testNamespace},
				Data:       map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, _ := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})

			txn := minimalTxn("del-commit-txn")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("del-commit-txn", "del-commit-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1", Kind: "ConfigMap",
					Name: "del-commit-cm", Namespace: testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			blocker := blockerChange("del-commit-txn", "del-commit-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			// Commit first item, then trigger lock failure on second.
			reconcileToPhase(reconciler, "del-commit-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, req("del-commit-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("del-commit-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())

			// Now delete the transaction (automatic-rollback present from seal time).
			Expect(k8sClient.Delete(ctx, txn)).To(Succeed())

			// Keep reconciling — deletion triggers rollback.
			for range 20 {
				Expect(k8sClient.Get(ctx, nn("del-commit-txn"), txn)).To(Succeed())
				if txn.Status.Phase == backupv1alpha1.TransactionPhaseRolledBack {
					break
				}
				_, err = reconciler.Reconcile(ctx, req("del-commit-txn"))
				Expect(err).NotTo(HaveOccurred())
			}

			// Rollback complete — simulate user removing rollback-protection.
			Expect(k8sClient.Get(ctx, nn("del-commit-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRolledBack))
			controllerutil.RemoveFinalizer(txn, rollbackProtectionFinalizer)
			Expect(k8sClient.Update(ctx, txn)).To(Succeed())

			// Final reconcile removes lease-cleanup → GC.
			_, err = reconciler.Reconcile(ctx, req("del-commit-txn"))
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, nn("del-commit-txn"), txn)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			// Verify ConfigMap was restored.
			Expect(k8sClient.Get(ctx, nn("del-commit-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("original"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
			_ = k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: "del-commit-blocker", Namespace: testNamespace,
			}})
		})

		It("should enter protected state when automatic-rollback is removed", func() {
			reconciler.LockMgr = &fakeLockMgr{}

			// Create a transaction already in Failed state with unrolled commits.
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "del-protected-txn",
					Namespace:  testNamespace,
					Finalizers: []string{finalizerName, rollbackProtectionFinalizer},
					// No automatic-rollback annotation.
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			txn.Status.Phase = backupv1alpha1.TransactionPhaseFailed
			txn.Status.RollbackRef = "del-protected-txn-rollback"
			txn.Status.Items = []backupv1alpha1.ItemStatus{{
				Name: "del-protected-cm-change", Committed: true,
			}}
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Delete the transaction.
			Expect(k8sClient.Delete(ctx, txn)).To(Succeed())

			// Reconcile: handleDeletion → Failed, no auto-rollback → PROTECTED.
			_, err := reconciler.Reconcile(ctx, req("del-protected-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Object should still exist (rollback-protection prevents GC).
			Expect(k8sClient.Get(ctx, nn("del-protected-txn"), txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(rollbackProtectionFinalizer))
			Expect(txn.Finalizers).NotTo(ContainElement(finalizerName))
		})

		It("should recover from protected state when automatic-rollback is re-added", func() {
			// Create target ConfigMap.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "del-retry-cm", Namespace: testNamespace},
				Data:       map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, _ := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})

			txn := minimalTxn("del-retry-txn")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("del-retry-txn", "del-retry-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1", Kind: "ConfigMap",
					Name: "del-retry-cm", Namespace: testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			blocker := blockerChange("del-retry-txn", "del-retry-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			// Drive to Committing and commit item 0.
			reconcileToPhase(reconciler, "del-retry-txn", backupv1alpha1.TransactionPhaseCommitting)
			_, err := reconciler.Reconcile(ctx, req("del-retry-txn"))
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, nn("del-retry-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())

			// Remove automatic-rollback and delete.
			delete(txn.Annotations, annotationAutoRollback)
			Expect(k8sClient.Update(ctx, txn)).To(Succeed())
			Expect(k8sClient.Delete(ctx, txn)).To(Succeed())

			// Reconcile: no auto-rollback → PROTECTED state.
			_, err = reconciler.Reconcile(ctx, req("del-retry-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("del-retry-txn"), txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(rollbackProtectionFinalizer))
			Expect(txn.Finalizers).NotTo(ContainElement(finalizerName))

			// User re-adds automatic-rollback to trigger recovery.
			if txn.Annotations == nil {
				txn.Annotations = make(map[string]string)
			}
			txn.Annotations[annotationAutoRollback] = "true"
			Expect(k8sClient.Update(ctx, txn)).To(Succeed())

			// Reconcile until terminal, then simulate user removing rollback-protection.
			for range 20 {
				Expect(k8sClient.Get(ctx, nn("del-retry-txn"), txn)).To(Succeed())
				if txn.Status.Phase == backupv1alpha1.TransactionPhaseRolledBack {
					break
				}
				_, err = reconciler.Reconcile(ctx, req("del-retry-txn"))
				Expect(err).NotTo(HaveOccurred())
			}

			Expect(k8sClient.Get(ctx, nn("del-retry-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRolledBack))
			controllerutil.RemoveFinalizer(txn, rollbackProtectionFinalizer)
			Expect(k8sClient.Update(ctx, txn)).To(Succeed())

			// One more reconcile to remove lease-cleanup finalizer via handleDeletion.
			_, err = reconciler.Reconcile(ctx, req("del-retry-txn"))
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, nn("del-retry-txn"), txn)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			// Verify ConfigMap was restored.
			Expect(k8sClient.Get(ctx, nn("del-retry-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("original"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
			_ = k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: "del-retry-blocker", Namespace: testNamespace,
			}})
		})

		It("should retry-rollback in Failed phase during deletion", func() {
			reconciler.LockMgr = &fakeLockMgr{}

			// Create a transaction in Failed state with unrolled commits and rollback CM.
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "del-retry-failed-txn",
					Namespace:  testNamespace,
					Finalizers: []string{finalizerName, rollbackProtectionFinalizer},
					Annotations: map[string]string{
						annotationRetryRollback: "true",
					},
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			txn.Status.Phase = backupv1alpha1.TransactionPhaseFailed
			txn.Status.RollbackRef = "del-retry-failed-txn-rollback"
			txn.Status.Items = []backupv1alpha1.ItemStatus{{
				Name: "del-retry-failed-cm-change", Committed: true,
			}}
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Delete the transaction.
			Expect(k8sClient.Delete(ctx, txn)).To(Succeed())

			// Reconcile: handleDeletion sees retry-rollback → removes annotation, transitions to RollingBack.
			_, err := reconciler.Reconcile(ctx, req("del-retry-failed-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("del-retry-failed-txn"), txn)).To(Succeed())
			Expect(txn.Annotations).NotTo(HaveKey(annotationRetryRollback))
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))
		})

		It("should GC deleted transaction with no unrolled commits", func() {
			reconciler.LockMgr = &fakeLockMgr{}

			txn := minimalTxn("del-no-unrolled")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createCMChange("del-no-unrolled", "del-no-unrolled-cm", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Drive to Committed.
			reconcileToPhase(reconciler, "del-no-unrolled", backupv1alpha1.TransactionPhaseCommitted)

			// Terminal handler strips lease-cleanup; rollback-protection remains (user-managed).
			_, err := reconciler.Reconcile(ctx, req("del-no-unrolled"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("del-no-unrolled"), txn)).To(Succeed())
			Expect(txn.Finalizers).NotTo(ContainElement(finalizerName))
			Expect(txn.Finalizers).To(ContainElement(rollbackProtectionFinalizer))

			// Simulate user removing rollback-protection, then delete.
			controllerutil.RemoveFinalizer(txn, rollbackProtectionFinalizer)
			Expect(k8sClient.Update(ctx, txn)).To(Succeed())

			Expect(k8sClient.Delete(ctx, txn)).To(Succeed())
			err = k8sClient.Get(ctx, nn("del-no-unrolled"), txn)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			// Clean up created CM.
			_ = k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: "del-no-unrolled-cm", Namespace: testNamespace,
			}})
		})

		It("should force-delete when user removes rollback-protection finalizer", func() {
			reconciler.LockMgr = &fakeLockMgr{}

			// Create a transaction in PROTECTED state (only rollback-protection finalizer).
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "del-force-txn",
					Namespace:  testNamespace,
					Finalizers: []string{rollbackProtectionFinalizer},
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			txn.Status.Phase = backupv1alpha1.TransactionPhaseFailed
			txn.Status.Items = []backupv1alpha1.ItemStatus{{
				Name: "del-force-cm-change", Committed: true,
			}}
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Delete the transaction.
			Expect(k8sClient.Delete(ctx, txn)).To(Succeed())

			// Verify it's stuck (rollback-protection prevents GC).
			Expect(k8sClient.Get(ctx, nn("del-force-txn"), txn)).To(Succeed())
			Expect(txn.DeletionTimestamp).NotTo(BeNil())

			// User removes the finalizer manually.
			controllerutil.RemoveFinalizer(txn, rollbackProtectionFinalizer)
			Expect(k8sClient.Update(ctx, txn)).To(Succeed())

			// Now the object should be gone.
			err := k8sClient.Get(ctx, nn("del-force-txn"), txn)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("idempotent rollback of Delete (re-Create)", func() {
		It("should succeed when called twice (AlreadyExists on second attempt)", func() {
			// Create and commit a Delete transaction, then trigger rollback.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "idemp-rb-del-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "value"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			// Build a 2-item transaction: item 0 = Delete (will commit), item 1 = Create (lock fails).

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "idemp-rb-del-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("idemp-rb-del-txn", "idemp-rb-del-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "idemp-rb-del-cm",
					Namespace:  testNamespace,
				},
				Type: backupv1alpha1.ChangeTypeDelete,
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("idemp-rb-del-txn", "idemp-rb-del-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Progress to Committing.
			reconcileToPhase(reconciler, "idemp-rb-del-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0 (Delete).
			_, err := reconciler.Reconcile(ctx, req("idemp-rb-del-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Fail item 1's lock check → triggers rollback.
			reconciler.LockMgr = failingRenewLockMgr()
			_, err = reconciler.Reconcile(ctx, req("idemp-rb-del-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("idemp-rb-del-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Rollback re-Creates the deleted CM. Do one reconcile to rollback item 0.
			_, err = reconciler.Reconcile(ctx, req("idemp-rb-del-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Verify the CM was re-created.
			Expect(k8sClient.Get(ctx, nn("idemp-rb-del-cm"), cm)).To(Succeed())

			// Now simulate a crash: reset item 0's RolledBack to false (as if status update failed).
			Expect(k8sClient.Get(ctx, nn("idemp-rb-del-txn"), txn)).To(Succeed())
			txn.Status.Items[0].RolledBack = false
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Second rollback attempt for same item — should succeed (AlreadyExists → nil).
			_, err = reconciler.Reconcile(ctx, req("idemp-rb-del-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Should still complete to RolledBack.
			reconcileToPhase(reconciler, "idemp-rb-del-txn", backupv1alpha1.TransactionPhaseRolledBack)

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("Create with SSA (idempotent)", func() {
		It("should merge via SSA when resource already exists", func() {
			cmContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "idemp-create-cm", "namespace": testNamespace},
				"data":       map[string]any{"k": "v"},
			}
			raw, _ := json.Marshal(cmContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "idemp-create-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("idemp-create-txn", "idemp-create-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "idemp-create-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Pre-create the ConfigMap — simulates the resource existing before
			// the transaction. Prepare should capture prior state.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "idemp-create-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"existing": "data"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			// Progress to Committing (prepare captures existing CM state).
			reconcileToPhase(reconciler, "idemp-create-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Verify prepare captured a ResourceVersion (resource existed).
			Expect(k8sClient.Get(ctx, nn("idemp-create-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].ResourceVersion).NotTo(BeEmpty())

			// Commit via SSA — should merge, not fail.
			_, err := reconciler.Reconcile(ctx, req("idemp-create-txn"))
			Expect(err).NotTo(HaveOccurred())

			reconcileToPhase(reconciler, "idemp-create-txn", backupv1alpha1.TransactionPhaseCommitted)

			// Verify the CM has both old and new data.
			Expect(k8sClient.Get(ctx, nn("idemp-create-cm"), cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("v"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})

		It("should rollback Create-over-existing by restoring prior state via SSA", func() {
			// Pre-create a ConfigMap with original data.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-create-exist-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"original": "value"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			createContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "rb-create-exist-cm", "namespace": testNamespace},
				"data":       map[string]any{"original": "overwritten"},
			}
			createRaw, _ := json.Marshal(createContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-create-exist-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("rb-create-exist-txn", "rb-create-exist-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rb-create-exist-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: createRaw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("rb-create-exist-txn", "rb-create-exist-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			// Use real lock manager through prepare and first commit.
			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Reconcile to Committing (prepare captures existing CM).
			reconcileToPhase(reconciler, "rb-create-exist-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0 (Create via SSA — overwrites "original" → "overwritten").
			_, err := reconciler.Reconcile(ctx, req("rb-create-exist-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("rb-create-exist-cm"), cm)).To(Succeed())
			Expect(cm.Data["original"]).To(Equal("overwritten"))

			// Fail item 1's lock check to trigger rollback.
			reconciler.LockMgr = failingRenewLockMgr()

			// Committing → RollingBack.
			_, err = reconciler.Reconcile(ctx, req("rb-create-exist-txn"))
			Expect(err).NotTo(HaveOccurred())

			// RollingBack → RolledBack (restores prior state via reverse SSA).
			reconcileToPhase(reconciler, "rb-create-exist-txn", backupv1alpha1.TransactionPhaseRolledBack)

			// Verify the CM was restored to its original data.
			Expect(k8sClient.Get(ctx, nn("rb-create-exist-cm"), cm)).To(Succeed())
			Expect(cm.Data["original"]).To(Equal("value"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("handleRollingBack retries on failure", func() {
		It("should stay in RollingBack (not Failed) when applyRollback fails", func() {
			// Create a CM, commit a Patch, then trigger rollback.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "retry-rb-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, _ := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "retry-rb-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("retry-rb-txn", "retry-rb-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "retry-rb-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("retry-rb-txn", "retry-rb-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "retry-rb-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, req("retry-rb-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Trigger rollback by failing item 1 lock check.
			reconciler.LockMgr = failingRenewLockMgr()
			_, err = reconciler.Reconcile(ctx, req("retry-rb-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("retry-rb-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Corrupt the rollback CM data to force applyRollback to fail.
			rbCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: txn.Status.RollbackRef, Namespace: testNamespace,
			}, rbCM)).To(Succeed())
			// Replace the stored state with invalid JSON.
			for key := range rbCM.Data {
				rbCM.Data[key] = "not-json"
			}
			Expect(k8sClient.Update(ctx, rbCM)).To(Succeed())

			// Reconcile: rollback should fail but txn should stay in RollingBack.
			_, err = reconciler.Reconcile(ctx, req("retry-rb-txn"))
			Expect(err).To(HaveOccurred()) // error returned for backoff

			Expect(k8sClient.Get(ctx, nn("retry-rb-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))
			Expect(txn.Status.Items[0].Error).To(ContainSubstring("rollback failed"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("handleRollingBack with missing rollback ConfigMap", func() {
		It("should transition to Failed when the rollback CM is missing", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "missing-rbcm-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "val"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, _ := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "missing-rbcm-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("missing-rbcm-txn", "missing-rbcm-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "missing-rbcm-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("missing-rbcm-txn", "missing-rbcm-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "missing-rbcm-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, req("missing-rbcm-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Trigger rollback.
			reconciler.LockMgr = failingRenewLockMgr()
			_, err = reconciler.Reconcile(ctx, req("missing-rbcm-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("missing-rbcm-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Delete the rollback ConfigMap.
			rbCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: txn.Status.RollbackRef, Namespace: testNamespace,
			}, rbCM)).To(Succeed())
			Expect(k8sClient.Delete(ctx, rbCM)).To(Succeed())

			// Reconcile: missing CM → Failed.
			_, err = reconciler.Reconcile(ctx, req("missing-rbcm-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("missing-rbcm-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("recovery from Failed phase", func() {
		It("should transition to RollingBack when un-rolled-back commits exist with rollback CM", func() {
			// Create a target CM.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "recover-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, _ := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "recover-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("recover-txn", "recover-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "recover-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("recover-txn", "recover-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "recover-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, req("recover-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Manually set to Failed with un-rolled-back commit (simulates old behavior).
			Expect(k8sClient.Get(ctx, nn("recover-txn"), txn)).To(Succeed())
			txn.Status.Phase = backupv1alpha1.TransactionPhaseFailed
			now := metav1.Now()
			txn.Status.CompletedAt = &now
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Verify items: item 0 committed but not rolled back.
			Expect(k8sClient.Get(ctx, nn("recover-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(txn.Status.Items[0].RolledBack).To(BeFalse())

			// Reconcile: Failed → RollingBack (recovery).
			_, err = reconciler.Reconcile(ctx, req("recover-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("recover-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Continue reconciling to RolledBack.
			reconcileToPhase(reconciler, "recover-txn", backupv1alpha1.TransactionPhaseRolledBack)

			// Verify rollback restored the original data.
			Expect(k8sClient.Get(ctx, nn("recover-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("original"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})

		It("should stay Failed when un-rolled-back commits exist but rollback CM is missing", func() {
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "no-recover-txn",
					Namespace:  testNamespace,
					Finalizers: []string{finalizerName, rollbackProtectionFinalizer},
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Manually set to Failed with un-rolled-back commit and no rollback CM.
			txn.Status.Phase = backupv1alpha1.TransactionPhaseFailed
			txn.Status.RollbackRef = "no-recover-txn-rollback"
			txn.Status.Items = []backupv1alpha1.ItemStatus{{
				Name:       "no-recover-cm-change",
				Committed:  true,
				RolledBack: false,
			}}
			now := metav1.Now()
			txn.Status.CompletedAt = &now
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Reconcile: no CM → stays Failed, strips finalizer.
			result, err := reconciler.Reconcile(ctx, req("no-recover-txn"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			Expect(k8sClient.Get(ctx, nn("no-recover-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))
		})
	})

	Context("stale error cleared on successful retry", func() {
		It("should clear item.Error after successful rollback retry", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "stale-err-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, _ := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "stale-err-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("stale-err-txn", "stale-err-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "stale-err-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("stale-err-txn", "stale-err-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "stale-err-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, req("stale-err-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Trigger rollback.
			reconciler.LockMgr = failingRenewLockMgr()
			_, err = reconciler.Reconcile(ctx, req("stale-err-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Inject a stale error on item 0 (simulates a previously failed rollback attempt).
			Expect(k8sClient.Get(ctx, nn("stale-err-txn"), txn)).To(Succeed())
			txn.Status.Items[0].Error = "rollback failed: transient network error"
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Reconcile: rollback succeeds → error should be cleared.
			_, err = reconciler.Reconcile(ctx, req("stale-err-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("stale-err-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Error).To(BeEmpty())
			Expect(txn.Status.Items[0].RolledBack).To(BeTrue())

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("when Renew succeeds for all items during commit", func() {
		It("should commit all items without triggering rollback", func() {
			cm1Content := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "renew-cm-1",
					"namespace": testNamespace,
				},
				"data": map[string]any{"k": "v1"},
			}
			cm2Content := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "renew-cm-2",
					"namespace": testNamespace,
				},
				"data": map[string]any{"k": "v2"},
			}
			raw1, _ := json.Marshal(cm1Content)
			raw2, _ := json.Marshal(cm2Content)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "renew-multi-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc1 := createChange("renew-multi-txn", "renew-cm-1-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "renew-cm-1",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw1},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc1)).To(Succeed())
			rc2 := createChange("renew-multi-txn", "renew-cm-2-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "renew-cm-2",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw2},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc2)).To(Succeed())

			// Use real lock manager through prepare.
			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Progress to Committing.
			reconcileToPhase(reconciler, "renew-multi-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Switch to fake: track Renew calls.
			var renewCalls int
			reconciler.LockMgr = &fakeLockMgr{
				renewFn: func(_ context.Context, _ lock.LeaseRef, _ string, _ time.Duration) error {
					renewCalls++
					return nil
				},
			}

			// Reconcile to Committed.
			reconcileToPhase(reconciler, "renew-multi-txn", backupv1alpha1.TransactionPhaseCommitted)

			Expect(renewCalls).To(Equal(2))

			// Verify both ConfigMaps were created.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("renew-cm-1"), cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("v1"))
			Expect(k8sClient.Get(ctx, nn("renew-cm-2"), cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("v2"))

			Expect(k8sClient.Get(ctx, nn("renew-multi-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(txn.Status.Items[1].Committed).To(BeTrue())

			// Clean up.
			Expect(k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "renew-cm-1", Namespace: testNamespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "renew-cm-2", Namespace: testNamespace}})).To(Succeed())
		})
	})

	Context("ServiceAccount impersonation", func() {
		It("should fail with non-existent SA", func() {
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "missing-sa-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: "does-not-exist",
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("missing-sa-txn", "missing-sa-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "missing-sa-cm",
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"missing-sa-cm"}}`)},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// SA validation now happens in Preparing, so drive through
			// finalizer + Pending before hitting the missing-SA failure.
			reconcileToPhase(reconciler, "missing-sa-txn", backupv1alpha1.TransactionPhaseFailed)

			Expect(k8sClient.Get(ctx, nn("missing-sa-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))
			Expect(txn.Status.Conditions).NotTo(BeEmpty())
			Expect(txn.Status.Conditions[0].Message).To(ContainSubstring("does-not-exist"))
		})

		It("should fail when SA lacks permissions for the target resource", func() {
			cmContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "unpriv-cm", "namespace": testNamespace},
				"data":       map[string]any{"k": "v"},
			}
			raw, _ := json.Marshal(cmContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "unpriv-sa-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: unprivSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("unpriv-sa-txn", "unpriv-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "unpriv-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Reconcile to terminal state — the SA has no RBAC so the
			// prepare phase fails with 403 when trying to read the target
			// resource (Create now checks for existing state). Since nothing
			// was committed, the transaction goes directly to Failed.
			reconcileToPhase(reconciler, "unpriv-sa-txn", backupv1alpha1.TransactionPhaseFailed)

			Expect(k8sClient.Get(ctx, nn("unpriv-sa-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))
		})
	})

	Context("lease expiry and split-brain prevention", func() {
		It("should detect external modification on Update rollback and fail instead of clobbering", func() {
			// When TxnA fails and its leases expire, TxnB can acquire them and
			// modify the resource. TxnA's auto-recovery rollback must detect the
			// external modification and transition to Failed rather than overwriting.
			// This applies to Update change type; Patch uses reverse SSA which
			// handles conflict at the field level via ForceOwnership.

			// Create target resource.
			origCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "staledata-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, origCM)).To(Succeed())

			// Build full Update content (not a partial Patch).
			updateContent, err := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "staledata-cm", "namespace": testNamespace},
				"data":       map[string]any{"key": "txn-a-modified"},
			})
			Expect(err).NotTo(HaveOccurred())

			// Create transaction with 2 items.
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "staledata-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("staledata-txn", "staledata-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "staledata-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeUpdate,
				Content: runtime.RawExtension{Raw: updateContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("staledata-txn", "staledata-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			// Use real lock manager for full transaction execution.
			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Drive to commit item 0 (saves original state in rollback CM).
			reconcileToPhase(reconciler, "staledata-txn", backupv1alpha1.TransactionPhaseCommitting)
			_, err = reconciler.Reconcile(ctx, req("staledata-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("staledata-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			rbCMName := txn.Status.RollbackRef

			// Verify rollback ConfigMap saved the original state.
			rbCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: rbCMName, Namespace: testNamespace}, rbCM)).To(Succeed())
			Expect(rbCM.Data).NotTo(BeEmpty())

			// Manually set to Failed (item 1 failed).
			txn.Status.Phase = backupv1alpha1.TransactionPhaseFailed
			now := metav1.Now()
			txn.Status.CompletedAt = &now
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Simulate TxnB modifying the resource while leases were stale.
			Expect(k8sClient.Get(ctx, nn("staledata-cm"), origCM)).To(Succeed())
			origCM.Data["key"] = "txn-b-modified"
			Expect(k8sClient.Update(ctx, origCM)).To(Succeed())

			// TxnA auto-recovers and attempts rollback — detects RV mismatch.
			// Can't use reconcileToPhase here: it checks phase before reconciling,
			// and we're already in Failed. We need to drive through the recovery
			// path explicitly: Failed → RollingBack → conflict → Failed.

			// Reconcile 1: recovery detects un-rolled-back commit → RollingBack.
			_, err = reconciler.Reconcile(ctx, req("staledata-txn"))
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, nn("staledata-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Reconcile 2: conflict on item 0 (RV mismatch), records error, requeues.
			_, err = reconciler.Reconcile(ctx, req("staledata-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Reconcile 3: item 0 skipped (has conflict), all done → Failed.
			_, err = reconciler.Reconcile(ctx, req("staledata-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("staledata-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))
			Expect(txn.Status.Items[0].Error).To(ContainSubstring("rollback conflict"))

			// TxnB's modification is preserved — split-brain prevented.
			Expect(k8sClient.Get(ctx, nn("staledata-cm"), origCM)).To(Succeed())
			Expect(origCM.Data["key"]).To(Equal("txn-b-modified"))

			// Rollback ConfigMap preserved for manual recovery via janus recover.
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: rbCMName, Namespace: testNamespace}, rbCM)).To(Succeed())
		})
	})

	Context("rollback edge cases", func() {
		It("rollback with no conflicts reaches RolledBack", func() {
			// Happy path: 2-item txn, item 0 = Patch, item 1 = blocker.
			// No external modifications between commit and rollback.
			// Rollback should succeed → RolledBack (not Failed).
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-edge-noconflict-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, err := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-edge-noconflict-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("rb-edge-noconflict-txn", "rb-edge-noconflict-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rb-edge-noconflict-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("rb-edge-noconflict-txn", "rb-edge-noconflict-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "rb-edge-noconflict-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0 (Patch).
			_, err = reconciler.Reconcile(ctx, req("rb-edge-noconflict-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("rb-edge-noconflict-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())

			// Verify patch was applied.
			Expect(k8sClient.Get(ctx, nn("rb-edge-noconflict-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("patched"))

			// Trigger rollback: item 1 lock renewal fails.
			reconciler.LockMgr = failingRenewLockMgr()
			_, err = reconciler.Reconcile(ctx, req("rb-edge-noconflict-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("rb-edge-noconflict-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Roll back to completion.
			reconcileToPhase(reconciler, "rb-edge-noconflict-txn", backupv1alpha1.TransactionPhaseRolledBack)

			Expect(k8sClient.Get(ctx, nn("rb-edge-noconflict-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRolledBack))
			Expect(txn.Status.CompletedAt).NotTo(BeNil())
			Expect(txn.Status.Items[0].RolledBack).To(BeTrue())
			Expect(txn.Status.Items[0].Error).To(BeEmpty())

			// Verify the ConfigMap was restored to original state.
			Expect(k8sClient.Get(ctx, nn("rb-edge-noconflict-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("original"))

			// Rollback ConfigMap still exists (OwnerRef GC cleans up on Transaction deletion).
			rbCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{
				Name: txn.Status.RollbackRef, Namespace: testNamespace,
			}, rbCM)).To(Succeed())

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})

		It("Create rollback deletes the created resource", func() {
			// 2-item txn: item 0 = Create (commits), item 1 = blocker (fails lock).
			// Rollback of Create = Delete. Verify the created resource is removed.
			createContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "rb-edge-create-cm",
					"namespace": testNamespace,
				},
				"data": map[string]any{"k": "created"},
			}
			createRaw, err := json.Marshal(createContent)
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-edge-create-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("rb-edge-create-txn", "rb-edge-create-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rb-edge-create-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: createRaw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("rb-edge-create-txn", "rb-edge-create-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "rb-edge-create-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0 (Create).
			_, err = reconciler.Reconcile(ctx, req("rb-edge-create-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Verify the ConfigMap was created.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("rb-edge-create-cm"), cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("created"))

			// Trigger rollback: item 1 lock renewal fails.
			reconciler.LockMgr = failingRenewLockMgr()
			_, err = reconciler.Reconcile(ctx, req("rb-edge-create-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("rb-edge-create-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Roll back to completion — Create rollback = Delete.
			reconcileToPhase(reconciler, "rb-edge-create-txn", backupv1alpha1.TransactionPhaseRolledBack)

			Expect(k8sClient.Get(ctx, nn("rb-edge-create-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRolledBack))
			Expect(txn.Status.Items[0].RolledBack).To(BeTrue())

			// Verify the created ConfigMap was deleted during rollback.
			err = k8sClient.Get(ctx, nn("rb-edge-create-cm"), cm)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("Patch rollback when target deleted externally detects conflict", func() {
			// Patch committed, then target deleted externally before rollback.
			// The RV check detects the deletion as a conflict (resource gone
			// but stored RV exists), so the transaction ends as Failed.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-edge-patch-del-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, err := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-edge-patch-del-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("rb-edge-patch-del-txn", "rb-edge-patch-del-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rb-edge-patch-del-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("rb-edge-patch-del-txn", "rb-edge-patch-del-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "rb-edge-patch-del-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0 (Patch).
			_, err = reconciler.Reconcile(ctx, req("rb-edge-patch-del-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("rb-edge-patch-del-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())

			// Delete the target externally before rollback.
			Expect(k8sClient.Get(ctx, nn("rb-edge-patch-del-cm"), cm)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())

			// Trigger rollback: item 1 lock renewal fails.
			reconciler.LockMgr = failingRenewLockMgr()
			_, err = reconciler.Reconcile(ctx, req("rb-edge-patch-del-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("rb-edge-patch-del-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Rollback: RV check detects deletion as conflict → Failed.
			reconcileToPhase(reconciler, "rb-edge-patch-del-txn", backupv1alpha1.TransactionPhaseFailed)

			Expect(k8sClient.Get(ctx, nn("rb-edge-patch-del-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))
			Expect(txn.Status.Items[0].RolledBack).To(BeFalse())
			Expect(txn.Status.Items[0].Error).To(ContainSubstring("rollback conflict"))
		})

		It("Update rollback re-creates when target deleted externally", func() {
			// Update committed, then target deleted externally before rollback.
			// applyRollback for Update detects NotFound and falls back to Create
			// from the stored prior state.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-edge-upd-del-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			updateContent, err := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "rb-edge-upd-del-cm", "namespace": testNamespace},
				"data":       map[string]any{"key": "updated"},
			})
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-edge-upd-del-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("rb-edge-upd-del-txn", "rb-edge-upd-del-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rb-edge-upd-del-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeUpdate,
				Content: runtime.RawExtension{Raw: updateContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("rb-edge-upd-del-txn", "rb-edge-upd-del-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "rb-edge-upd-del-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0 (Update).
			_, err = reconciler.Reconcile(ctx, req("rb-edge-upd-del-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("rb-edge-upd-del-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())

			// Verify Update was applied.
			Expect(k8sClient.Get(ctx, nn("rb-edge-upd-del-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("updated"))

			// Delete the target externally before rollback.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())

			// Trigger rollback: item 1 lock renewal fails.
			reconciler.LockMgr = failingRenewLockMgr()
			_, err = reconciler.Reconcile(ctx, req("rb-edge-upd-del-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("rb-edge-upd-del-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Rollback: Update detects NotFound → falls back to Create from prior state.
			reconcileToPhase(reconciler, "rb-edge-upd-del-txn", backupv1alpha1.TransactionPhaseRolledBack)

			Expect(k8sClient.Get(ctx, nn("rb-edge-upd-del-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRolledBack))
			Expect(txn.Status.Items[0].RolledBack).To(BeTrue())

			// Verify the resource was re-created with the original data.
			Expect(k8sClient.Get(ctx, nn("rb-edge-upd-del-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("original"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("conflict detection", func() {
		It("should store resourceVersion in ItemStatus during prepare", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "conflict-rv-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "value"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

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
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("rv-capture-txn", "conflict-rv-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "conflict-rv-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: []byte(`{"data":{"key":"patched"}}`)},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			reconcileToPhase(reconciler, "rv-capture-txn", backupv1alpha1.TransactionPhasePrepared)

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "rv-capture-txn", Namespace: testNamespace,
			}, txn)).To(Succeed())
			Expect(txn.Status.Items[0].ResourceVersion).To(Equal(expectedRV))

			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})

		It("should commit Patch despite external modification (SSA is idempotent)", func() {
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
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("conflict-patch-txn", "conflict-patch-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "conflict-patch-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: []byte(`{"data":{"key":"patched"}}`)},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			reconcileToPhase(reconciler, "conflict-patch-txn", backupv1alpha1.TransactionPhasePrepared)

			// External actor modifies the resource.
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "conflict-patch-cm", Namespace: testNamespace,
			}, cm)).To(Succeed())
			cm.Data["key"] = "externally-modified" //nolint:goconst // test data
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			// Reconcile through Committing — SSA Patch skips RV conflict check
			// and succeeds via field ownership.
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
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))

			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})

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
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("conflict-delete-txn", "conflict-delete-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "conflict-delete-cm",
					Namespace:  testNamespace,
				},
				Type: backupv1alpha1.ChangeTypeDelete,
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			reconcileToPhase(reconciler, "conflict-delete-txn", backupv1alpha1.TransactionPhasePrepared)

			// External actor modifies the resource.
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "conflict-delete-cm", Namespace: testNamespace,
			}, cm)).To(Succeed())
			cm.Data["key"] = "externally-modified"
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			// Reconcile — should detect conflict and fail.
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

			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})

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
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("conflict-create-txn", "conflict-create-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "conflict-create-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			reconcileToPhase(reconciler, "conflict-create-txn", backupv1alpha1.TransactionPhaseCommitted)

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "conflict-create-txn", Namespace: testNamespace,
			}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))
			Expect(txn.Status.Items[0].ResourceVersion).To(BeEmpty())

			// Clean up.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "conflict-create-cm", Namespace: testNamespace,
			}, cm)).To(Succeed())
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})

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
	})

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

			// Reconcile once to transition Prepared → Committing.
			_, err = reconciler.Reconcile(ctx, req("selfwrite-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Reconcile again to actually commit item 0.
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

		It("should treat Delete as satisfied when resource is already gone on retry", func() {
			// Create a ConfigMap to delete.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "delete-retry-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "value"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			txn := minimalTxn("delete-retry-txn")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("delete-retry-txn", "delete-retry-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "delete-retry-cm",
					Namespace:  testNamespace,
				},
				Type: backupv1alpha1.ChangeTypeDelete,
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Reconcile through Prepared.
			reconcileToPhase(reconciler, "delete-retry-txn", backupv1alpha1.TransactionPhasePrepared)

			// Reconcile once to transition Prepared → Committing.
			_, err := reconciler.Reconcile(ctx, req("delete-retry-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Reconcile again to actually commit (delete the CM).
			_, err = reconciler.Reconcile(ctx, req("delete-retry-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Verify the item was committed and the CM is gone.
			Expect(k8sClient.Get(ctx, nn("delete-retry-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(apierrors.IsNotFound(
				k8sClient.Get(ctx, nn("delete-retry-cm"), cm),
			)).To(BeTrue())

			// Simulate crash: revert Committed flag while the
			// target resource remains deleted.
			txn.Status.Items[0].Committed = false
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Retry — should recognize the Delete is satisfied and
			// continue to Committed, not false-fail as conflict.
			for range 10 {
				Expect(k8sClient.Get(ctx, nn("delete-retry-txn"), txn)).To(Succeed())
				if txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted ||
					txn.Status.Phase == backupv1alpha1.TransactionPhaseFailed {
					break
				}
				_, err = reconciler.Reconcile(ctx, req("delete-retry-txn"))
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(k8sClient.Get(ctx, nn("delete-retry-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))
		})

		It("should re-apply Patch idempotently on crash-retry via SSA", func() {
			// Create a ConfigMap to patch.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "patch-retry-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original", "other": "kept"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "patch-retry-cm", "namespace": testNamespace},
				"data":       map[string]any{"key": "patched"},
			}
			raw, err := json.Marshal(patchContent)
			Expect(err).NotTo(HaveOccurred())

			txn := minimalTxn("patch-retry-txn")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("patch-retry-txn", "patch-retry-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "patch-retry-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: raw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Reconcile through Prepared.
			reconcileToPhase(reconciler, "patch-retry-txn", backupv1alpha1.TransactionPhasePrepared)

			// Reconcile once to transition Prepared → Committing.
			_, err = reconciler.Reconcile(ctx, req("patch-retry-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Reconcile again to actually commit (SSA patch the CM).
			_, err = reconciler.Reconcile(ctx, req("patch-retry-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Verify the item was committed and the patch applied.
			Expect(k8sClient.Get(ctx, nn("patch-retry-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(k8sClient.Get(ctx, nn("patch-retry-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("patched"))
			Expect(cm.Data["other"]).To(Equal("kept"))

			// Simulate crash: revert Committed flag while the
			// target resource already has the patched values.
			txn.Status.Items[0].Committed = false
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Retry — SSA re-apply is idempotent, should reach Committed.
			for range 10 {
				Expect(k8sClient.Get(ctx, nn("patch-retry-txn"), txn)).To(Succeed())
				if txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted ||
					txn.Status.Phase == backupv1alpha1.TransactionPhaseFailed {
					break
				}
				_, err = reconciler.Reconcile(ctx, req("patch-retry-txn"))
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(k8sClient.Get(ctx, nn("patch-retry-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))

			// Verify patched values survived the retry.
			Expect(k8sClient.Get(ctx, nn("patch-retry-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("patched"))
			Expect(cm.Data["other"]).To(Equal("kept"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("status conditions", func() {
		It("should set Prepared condition when all items are prepared", func() {
			txn := minimalTxn("cond-prepared")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createCMChange("cond-prepared", "cond-prepared-cm", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Drive through: finalizer, Pending→Preparing, prepare item, Preparing→Prepared→Committing
			reconcileToPhase(reconciler, "cond-prepared", backupv1alpha1.TransactionPhaseCommitting)

			Expect(k8sClient.Get(ctx, nn("cond-prepared"), txn)).To(Succeed())

			prepared := apimeta.FindStatusCondition(txn.Status.Conditions, "Prepared")
			Expect(prepared).NotTo(BeNil())
			Expect(prepared.Status).To(Equal(metav1.ConditionTrue))
			Expect(prepared.Reason).To(Equal("AllItemsPrepared"))

			// Clean up created ConfigMap.
			cm := &corev1.ConfigMap{}
			_ = k8sClient.Get(ctx, nn("cond-prepared-cm"), cm)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, cm))
		})

		It("should set Committed condition when transaction completes", func() {
			txn := minimalTxn("cond-committed")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createCMChange("cond-committed", "cond-committed-cm", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			reconcileToPhase(reconciler, "cond-committed", backupv1alpha1.TransactionPhaseCommitted)

			Expect(k8sClient.Get(ctx, nn("cond-committed"), txn)).To(Succeed())

			committed := apimeta.FindStatusCondition(txn.Status.Conditions, "Committed")
			Expect(committed).NotTo(BeNil())
			Expect(committed.Status).To(Equal(metav1.ConditionTrue))
			Expect(committed.Reason).To(Equal("AllItemsCommitted"))

			// Prepared condition should still be present (monotonic).
			prepared := apimeta.FindStatusCondition(txn.Status.Conditions, "Prepared")
			Expect(prepared).NotTo(BeNil())
			Expect(prepared.Status).To(Equal(metav1.ConditionTrue))

			// Clean up created ConfigMap.
			cm := &corev1.ConfigMap{}
			_ = k8sClient.Get(ctx, nn("cond-committed-cm"), cm)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, cm))
		})
	})

	Context("transaction timeout", func() {
		It("should fail a timed-out transaction with no commits", func() {
			txn := minimalTxn("timeout-no-commits")
			txn.Spec.Timeout = &metav1.Duration{Duration: 1 * time.Nanosecond}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createCMChange("timeout-no-commits", "timeout-no-commits-cm", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Drive through finalizers and into Preparing.
			reconcileToPhase(reconciler, "timeout-no-commits", backupv1alpha1.TransactionPhasePreparing)

			// At this point StartedAt is set and 1ns timeout has already expired.
			// Next reconcile should detect timeout. No items are committed.
			_, err := reconciler.Reconcile(ctx, req("timeout-no-commits"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("timeout-no-commits"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))

			failedCond := apimeta.FindStatusCondition(txn.Status.Conditions, "Failed")
			Expect(failedCond).NotTo(BeNil())
			Expect(failedCond.Message).To(ContainSubstring("timed out"))

			// Clean up created ConfigMap (rollback CM).
			cm := &corev1.ConfigMap{}
			_ = k8sClient.Get(ctx, nn("timeout-no-commits-cm"), cm)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, cm))
		})

		It("should roll back a timed-out transaction with committed items", func() {
			// Create a target ConfigMap that will be updated.
			targetCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "timeout-target-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"original": "data"},
			}
			Expect(k8sClient.Create(ctx, targetCM)).To(Succeed())

			updateContent, _ := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "timeout-target-cm", "namespace": testNamespace},
				"data":       map[string]any{"updated": "first"},
			})
			createContent, _ := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "timeout-new-cm", "namespace": testNamespace},
				"data":       map[string]any{"new": "item"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "timeout-with-commits",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Timeout:            &metav1.Duration{Duration: 10 * time.Minute},
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc1 := createChange("timeout-with-commits", "timeout-target-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "timeout-target-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeUpdate,
				Content: runtime.RawExtension{Raw: updateContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc1)).To(Succeed())
			rc2 := createChange("timeout-with-commits", "timeout-new-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "timeout-new-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: createContent},
				Order:   1,
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc2)).To(Succeed())

			// Drive through: finalizer, Pending->Preparing, prepare item 0, prepare item 1,
			// Prepared->Committing, commit item 0 (Update).
			// That's ~6 reconcile cycles. Use reconcileToPhase to get to Committing,
			// then one more to commit item 0.
			reconcileToPhase(reconciler, "timeout-with-commits", backupv1alpha1.TransactionPhaseCommitting)

			// Reconcile once more to commit item 0.
			_, err := reconciler.Reconcile(ctx, req("timeout-with-commits"))
			Expect(err).NotTo(HaveOccurred())

			// Verify item 0 is committed but item 1 is not.
			Expect(k8sClient.Get(ctx, nn("timeout-with-commits"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(txn.Status.Items[1].Committed).To(BeFalse())

			// Backdate startedAt to 20 minutes ago to trigger timeout.
			past := metav1.NewTime(time.Now().Add(-20 * time.Minute))
			txn.Status.StartedAt = &past
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Next reconcile should detect timeout and transition to RollingBack.
			_, err = reconciler.Reconcile(ctx, req("timeout-with-commits"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("timeout-with-commits"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Clean up created ConfigMaps.
			for _, name := range []string{"timeout-target-cm", "timeout-new-cm"} {
				cm := &corev1.ConfigMap{}
				_ = k8sClient.Get(ctx, nn(name), cm)
				_ = client.IgnoreNotFound(k8sClient.Delete(ctx, cm))
			}
		})

		It("should fail a timed-out rollback", func() {
			// Create a target ConfigMap that will be updated.
			targetCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-timeout-target-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"original": "data"},
			}
			Expect(k8sClient.Create(ctx, targetCM)).To(Succeed())

			updateContent, _ := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "rb-timeout-target-cm", "namespace": testNamespace},
				"data":       map[string]any{"updated": "first"},
			})
			createContent, _ := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "rb-timeout-new-cm", "namespace": testNamespace},
				"data":       map[string]any{"new": "item"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "timeout-rollback",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Timeout:            &metav1.Duration{Duration: 10 * time.Minute},
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc1 := createChange("timeout-rollback", "rb-timeout-target-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rb-timeout-target-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeUpdate,
				Content: runtime.RawExtension{Raw: updateContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc1)).To(Succeed())
			rc2 := createChange("timeout-rollback", "rb-timeout-new-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rb-timeout-new-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: createContent},
				Order:   1,
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc2)).To(Succeed())

			// Drive through to Committing, then commit item 0.
			reconcileToPhase(reconciler, "timeout-rollback", backupv1alpha1.TransactionPhaseCommitting)

			_, err := reconciler.Reconcile(ctx, req("timeout-rollback"))
			Expect(err).NotTo(HaveOccurred())

			// Verify item 0 is committed.
			Expect(k8sClient.Get(ctx, nn("timeout-rollback"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(txn.Status.Items[1].Committed).To(BeFalse())

			// Backdate startedAt to trigger timeout -> RollingBack.
			past := metav1.NewTime(time.Now().Add(-20 * time.Minute))
			txn.Status.StartedAt = &past
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, req("timeout-rollback"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("timeout-rollback"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Now we're in RollingBack with a backdated startedAt.
			// The timeout should fire again and transition to Failed.
			// Re-backdate in case the status update reset it.
			Expect(k8sClient.Get(ctx, nn("timeout-rollback"), txn)).To(Succeed())
			past = metav1.NewTime(time.Now().Add(-20 * time.Minute))
			txn.Status.StartedAt = &past
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, req("timeout-rollback"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("timeout-rollback"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))

			failedCond := apimeta.FindStatusCondition(txn.Status.Conditions, "Failed")
			Expect(failedCond).NotTo(BeNil())
			Expect(failedCond.Message).To(ContainSubstring("timed out"))

			// Clean up created ConfigMaps.
			for _, name := range []string{"rb-timeout-target-cm", "rb-timeout-new-cm"} {
				cm := &corev1.ConfigMap{}
				_ = k8sClient.Get(ctx, nn(name), cm)
				_ = client.IgnoreNotFound(k8sClient.Delete(ctx, cm))
			}
		})
	})

	Context("when rolling back an Update (full replace)", func() {
		It("should restore the original resource state", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-update-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"old-key": "old-val"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			updateContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "rb-update-cm",
					"namespace": testNamespace,
				},
				"data": map[string]any{
					"new-key": "new-val",
				},
			}
			updateRaw, _ := json.Marshal(updateContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-update-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("rb-update-txn", "rb-update-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rb-update-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeUpdate,
				Content: runtime.RawExtension{Raw: updateRaw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("rb-update-txn", "rb-update-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Progress to Committing and commit item 0 (Update).
			reconcileToPhase(reconciler, "rb-update-txn", backupv1alpha1.TransactionPhaseCommitting)

			_, err := reconciler.Reconcile(ctx, req("rb-update-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Verify item 0 committed — full replace applied.
			Expect(k8sClient.Get(ctx, nn("rb-update-cm"), cm)).To(Succeed())
			Expect(cm.Data).To(HaveKey("new-key"))
			Expect(cm.Data).NotTo(HaveKey("old-key"))

			// Fail item 1's lock check → triggers rollback.
			reconciler.LockMgr = failingRenewLockMgr()

			_, err = reconciler.Reconcile(ctx, req("rb-update-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Rollback should restore the original state.
			reconcileToPhase(reconciler, "rb-update-txn", backupv1alpha1.TransactionPhaseRolledBack)

			Expect(k8sClient.Get(ctx, nn("rb-update-cm"), cm)).To(Succeed())
			Expect(cm.Data["old-key"]).To(Equal("old-val"))
			Expect(cm.Data).NotTo(HaveKey("new-key"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("conflict detection for Update (optimistic concurrency)", func() {
		It("should fail when the resource is modified between prepare and commit", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "conflict-update-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			updateContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "conflict-update-cm",
					"namespace": testNamespace,
				},
				"data": map[string]any{"key": "updated"},
			}
			raw, _ := json.Marshal(updateContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "conflict-update-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("conflict-update-txn", "conflict-update-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "conflict-update-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeUpdate,
				Content: runtime.RawExtension{Raw: raw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			reconcileToPhase(reconciler, "conflict-update-txn", backupv1alpha1.TransactionPhasePrepared)

			// External actor modifies the resource after prepare.
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "conflict-update-cm", Namespace: testNamespace,
			}, cm)).To(Succeed())
			cm.Data["key"] = "externally-modified"
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			// Reconcile through Committing — should detect conflict and fail.
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

			// Verify the failure mentions conflict.
			failedCond := apimeta.FindStatusCondition(txn.Status.Conditions, "Failed")
			Expect(failedCond).NotTo(BeNil())
			Expect(failedCond.Message).To(ContainSubstring("conflict"))

			// Verify metrics.
			Expect(testutil.ToFloat64(txmetrics.ItemOperations.WithLabelValues("commit", "conflict"))).To(BeNumerically(">", 0))

			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("status conditions on rollback", func() {
		It("should set RolledBack condition when rollback completes", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cond-rb-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, _ := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cond-rb-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("cond-rb-txn", "cond-rb-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "cond-rb-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("cond-rb-txn", "cond-rb-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "cond-rb-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, req("cond-rb-txn"))
			Expect(err).NotTo(HaveOccurred())

			// Trigger rollback.
			reconciler.LockMgr = failingRenewLockMgr()

			_, err = reconciler.Reconcile(ctx, req("cond-rb-txn"))
			Expect(err).NotTo(HaveOccurred())

			reconcileToPhase(reconciler, "cond-rb-txn", backupv1alpha1.TransactionPhaseRolledBack)

			Expect(k8sClient.Get(ctx, nn("cond-rb-txn"), txn)).To(Succeed())

			rbCond := apimeta.FindStatusCondition(txn.Status.Conditions, "RolledBack")
			Expect(rbCond).NotTo(BeNil())
			Expect(rbCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(rbCond.Reason).To(Equal("RollbackComplete"))

			// CompletedAt should be set.
			Expect(txn.Status.CompletedAt).NotTo(BeNil())

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("custom lock timeout", func() {
		It("should pass the configured lockTimeout to the lock manager", func() {
			var acquiredTimeout time.Duration
			reconciler.LockMgr = &fakeLockMgr{
				acquireFn: func(_ context.Context, key lock.ResourceKey, _ string, timeout time.Duration) (string, error) {
					acquiredTimeout = timeout
					return lock.LeaseName(key), nil
				},
			}

			cmContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "lock-timeout-cm", "namespace": testNamespace},
				"data":       map[string]any{"k": "v"},
			}
			raw, _ := json.Marshal(cmContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "lock-timeout-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					LockTimeout:        &metav1.Duration{Duration: 42 * time.Second},
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("lock-timeout-txn", "lock-timeout-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "lock-timeout-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			// Drive through finalizer and Pending → Preparing.
			reconcileToPhase(reconciler, "lock-timeout-txn", backupv1alpha1.TransactionPhasePreparing)

			// Reconcile once more to prepare the item (triggers Acquire).
			_, err := reconciler.Reconcile(ctx, req("lock-timeout-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(acquiredTimeout).To(Equal(42 * time.Second))

			// Clean up created ConfigMap.
			cm := &corev1.ConfigMap{}
			_ = k8sClient.Get(ctx, nn("lock-timeout-cm"), cm)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, cm))
		})
	})

	Context("rollback ConfigMap _meta key", func() {
		It("should contain correct transaction metadata after reaching Prepared", func() {
			txn := minimalTxn("meta-txn")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createCMChange("meta-txn", "meta-txn-cm", txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())

			reconcileToPhase(reconciler, "meta-txn", backupv1alpha1.TransactionPhasePrepared)

			// Read the rollback ConfigMap.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("meta-txn"+rollbackCMSuffix), cm)).To(Succeed())

			// Verify _meta key exists.
			raw, ok := cm.Data[rollback.MetaKey]
			Expect(ok).To(BeTrue(), "_meta key should be present in rollback ConfigMap")

			// Unmarshal and verify contents.
			var meta rollback.Meta
			Expect(json.Unmarshal([]byte(raw), &meta)).To(Succeed())
			Expect(meta.Version).To(Equal(2))
			Expect(meta.TransactionName).To(Equal("meta-txn"))
			Expect(meta.TransactionNamespace).To(Equal(testNamespace))
			Expect(meta.Changes).To(HaveLen(1))

			ch := meta.Changes[0]
			Expect(ch.Name).To(Equal("meta-txn-cm-change"))
			Expect(ch.ChangeType).To(Equal(string(backupv1alpha1.ChangeTypeCreate)))
			Expect(ch.Target.APIVersion).To(Equal("v1"))
			Expect(ch.Target.Kind).To(Equal("ConfigMap"))
			Expect(ch.Target.Name).To(Equal("meta-txn-cm"))
			Expect(ch.Target.Namespace).To(Equal(testNamespace))
			Expect(ch.RollbackKey).To(Equal(rollback.Key("ConfigMap", testNamespace, "meta-txn-cm")))

			// Clean up created ConfigMap.
			created := &corev1.ConfigMap{}
			_ = k8sClient.Get(ctx, nn("meta-txn-cm"), created)
			_ = client.IgnoreNotFound(k8sClient.Delete(ctx, created))
		})
	})

	Context("rollback conflict detection", func() {
		It("should fail the transaction when an Update resource was modified externally during rollback", func() {
			// 1. Create target CM with original data.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rbconflict-cm",
					Namespace: testNamespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			// 2. Create a 2-item transaction: Update cm (item 0) + blocker Create (item 1).
			updateContent, err := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "rbconflict-cm", "namespace": testNamespace},
				"data":       map[string]any{"key": "updated"},
			})
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rbconflict-txn",
					Namespace: testNamespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("rbconflict-txn", "rbconflict-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "rbconflict-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeUpdate,
				Content: runtime.RawExtension{Raw: updateContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("rbconflict-txn", "rbconflict-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			// Use real lock manager through prepare and commit.
			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// 3. Drive through Pending → Preparing → Prepared → Committing.
			reconcileToPhase(reconciler, "rbconflict-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Reconcile to commit item 0 (Patch succeeds).
			_, err = reconciler.Reconcile(ctx, req("rbconflict-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("rbconflict-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(txn.Status.Items[1].Committed).To(BeFalse())

			// Swap to failing lock manager so item 1's lock renewal fails → RollingBack.
			reconciler.LockMgr = failingRenewLockMgr()

			_, err = reconciler.Reconcile(ctx, req("rbconflict-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("rbconflict-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// 4. Externally modify the CM before rollback runs.
			Expect(k8sClient.Get(ctx, nn("rbconflict-cm"), cm)).To(Succeed())
			cm.Data["key"] = "externally-modified"
			Expect(k8sClient.Update(ctx, cm)).To(Succeed())

			// 5. Reconcile rollback — detects conflict, continues past it, then Failed.
			reconcileToPhase(reconciler, "rbconflict-txn", backupv1alpha1.TransactionPhaseFailed)

			// 6. Assert: phase=Failed, item[0].Error contains "rollback conflict".
			Expect(k8sClient.Get(ctx, nn("rbconflict-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))
			Expect(txn.Status.Items[0].Error).To(ContainSubstring("rollback conflict"))

			// Verify the Failed condition is set.
			failedCond := apimeta.FindStatusCondition(txn.Status.Conditions, "Failed")
			Expect(failedCond).NotTo(BeNil())
			Expect(failedCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(failedCond.Message).To(ContainSubstring("conflicts"))

			// Verify the CM still has the external modification (not clobbered).
			Expect(k8sClient.Get(ctx, nn("rbconflict-cm"), cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("externally-modified"))

			// 7. Assert: rollback ConfigMap still exists (preserved for janus recover).
			rbCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{
				Name:      txn.Status.RollbackRef,
				Namespace: testNamespace,
			}, rbCM)).To(Succeed())

			// Verify conflict metric was recorded.
			Expect(testutil.ToFloat64(txmetrics.ItemOperations.WithLabelValues("rollback", "conflict"))).To(BeNumerically(">=", 1.0))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})

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

			// Use a lock manager that fails Renew on the blocker to trigger rollback.
			reconciler.LockMgr = &fakeLockMgr{
				renewFn: func(_ context.Context, lease lock.LeaseRef, _ string, _ time.Duration) error {
					if lease.Name == lock.LeaseName(lock.ResourceKey{
						Namespace: testNamespace,
						Kind:      "ConfigMap",
						Name:      "rb-patch-conflict-blocker",
					}) {
						return &lock.ErrLockExpired{LeaseName: lease.Name}
					}
					return nil
				},
			}

			// Reconcile: item 0 commits, item 1 triggers rollback.
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
		})

		It("should detect rollback conflict on Create (fresh) when resource modified after commit", func() {
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
	})

	Context("Patch rollback via reverse SSA patch", func() {
		It("should restore only patched fields, preserving unrelated changes", func() {
			// Create target with original data.
			target := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "ssa-rb-cm", Namespace: testNamespace},
				Data:       map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, target)).To(Succeed())

			// Build a 2-item txn: Patch (item 0) + blocker Create (item 1).
			// Item 1's lock renewal will fail, triggering rollback of item 0.
			raw, err := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "ssa-rb-cm", "namespace": testNamespace},
				"data":       map[string]any{"key": "patched"},
			})
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{Name: "ssa-rb-txn", Namespace: testNamespace},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc := createChange("ssa-rb-txn", "ssa-rb-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target:  backupv1alpha1.ResourceRef{APIVersion: "v1", Kind: "ConfigMap", Name: "ssa-rb-cm"},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: raw},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc)).To(Succeed())
			blocker := blockerChange("ssa-rb-txn", "ssa-rb-blocker", txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			// Use real lock manager through prepare and first commit.
			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Drive to Committing, then commit item 0 (Patch).
			reconcileToPhase(reconciler, "ssa-rb-txn", backupv1alpha1.TransactionPhaseCommitting)
			_, err = reconciler.Reconcile(ctx, req("ssa-rb-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("ssa-rb-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())

			// Verify the Patch was applied.
			Expect(k8sClient.Get(ctx, nn("ssa-rb-cm"), target)).To(Succeed())
			Expect(target.Data["key"]).To(Equal("patched"))

			// Swap to failing lock manager: item 1's lock renewal fails → RollingBack.
			reconciler.LockMgr = failingRenewLockMgr()
			_, err = reconciler.Reconcile(ctx, req("ssa-rb-txn"))
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, nn("ssa-rb-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Reconcile: RollingBack → reverse SSA patch rolls back item 0 → RolledBack.
			reconcileToPhase(reconciler, "ssa-rb-txn", backupv1alpha1.TransactionPhaseRolledBack)

			// Verify: key restored to original.
			Expect(k8sClient.Get(ctx, nn("ssa-rb-cm"), target)).To(Succeed())
			Expect(target.Data["key"]).To(Equal("original"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, target)).To(Succeed())
		})
	})

	Context("continue-past-conflicts rollback", func() {
		It("should skip conflicted Update items and still roll back Patch items", func() {
			// 3-item txn: Patch (order 0) + Update (order 1) + blocker Create (order 2).
			// Reverse iteration hits Update (1) first → conflict → must skip.
			// Then Patch (0) → reverse SSA → success. Final: Failed.
			// This ordering is critical: with stop-on-first, item 0 would never
			// get rolled back because the conflict on item 1 stops everything.
			//
			// The blocker must have order 2 (not 1) so it sorts after the Update.
			// With order+name sorting, same-order items sort alphabetically by name.

			for _, name := range []string{"cpc-patch-cm", "cpc-upd-cm"} {
				cm := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
					Data:       map[string]string{"key": "original"},
				}
				Expect(k8sClient.Create(ctx, cm)).To(Succeed())
			}

			patchContent, err := json.Marshal(map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "cpc-patch-cm", "namespace": testNamespace},
				"data":     map[string]any{"key": "patched"},
			})
			Expect(err).NotTo(HaveOccurred())
			updContent, err := json.Marshal(map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "cpc-upd-cm", "namespace": testNamespace},
				"data":     map[string]any{"key": "updated"},
			})
			Expect(err).NotTo(HaveOccurred())
			blockerContent, err := json.Marshal(map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "cpc-blocker", "namespace": testNamespace},
				"data":     map[string]any{"k": "v"},
			})
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{Name: "cpc-txn", Namespace: testNamespace},
				Spec: backupv1alpha1.TransactionSpec{
					ServiceAccountName: testSAName,
					Sealed:             true,
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			rc0 := createChange("cpc-txn", "cpc-patch-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target:  backupv1alpha1.ResourceRef{APIVersion: "v1", Kind: "ConfigMap", Name: "cpc-patch-cm"},
				Type:    backupv1alpha1.ChangeTypePatch,
				Content: runtime.RawExtension{Raw: patchContent},
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc0)).To(Succeed())
			rc1 := createChange("cpc-txn", "cpc-upd-cm-change", backupv1alpha1.ResourceChangeSpec{
				Target:  backupv1alpha1.ResourceRef{APIVersion: "v1", Kind: "ConfigMap", Name: "cpc-upd-cm"},
				Type:    backupv1alpha1.ChangeTypeUpdate,
				Content: runtime.RawExtension{Raw: updContent},
				Order:   1,
			}, txn.UID)
			Expect(k8sClient.Create(ctx, rc1)).To(Succeed())
			blocker := createChange("cpc-txn", "cpc-blocker-change", backupv1alpha1.ResourceChangeSpec{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       "cpc-blocker",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: blockerContent},
				Order:   2,
			}, txn.UID)
			Expect(k8sClient.Create(ctx, blocker)).To(Succeed())

			// Real lock manager for prepare and commits.
			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Drive to Committing.
			reconcileToPhase(reconciler, "cpc-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0 (Patch).
			_, err = reconciler.Reconcile(ctx, req("cpc-txn"))
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, nn("cpc-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())

			// Commit item 1 (Update).
			_, err = reconciler.Reconcile(ctx, req("cpc-txn"))
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, nn("cpc-txn"), txn)).To(Succeed())
			Expect(txn.Status.Items[1].Committed).To(BeTrue())

			// Externally modify item 1's target (bump RV past what envelope recorded).
			target1 := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("cpc-upd-cm"), target1)).To(Succeed())
			target1.Data["key"] = "externally-modified"
			Expect(k8sClient.Update(ctx, target1)).To(Succeed())

			// Failing lock manager → item 2 lock renewal fails → RollingBack.
			reconciler.LockMgr = failingRenewLockMgr()
			_, err = reconciler.Reconcile(ctx, req("cpc-txn"))
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, nn("cpc-txn"), txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Reconcile through rollback to completion (Failed due to conflict).
			reconcileToPhase(reconciler, "cpc-txn", backupv1alpha1.TransactionPhaseFailed)

			Expect(k8sClient.Get(ctx, nn("cpc-txn"), txn)).To(Succeed())

			// Item 0 (Patch): rolled back via reverse SSA despite item 1's conflict.
			Expect(txn.Status.Items[0].RolledBack).To(BeTrue())

			// Item 1 (Update): conflict, not rolled back.
			Expect(txn.Status.Items[1].RolledBack).To(BeFalse())
			Expect(txn.Status.Items[1].Error).To(ContainSubstring("rollback conflict"))

			// Rollback ConfigMap preserved for recovery CLI.
			rbCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{
				Name: txn.Status.RollbackRef, Namespace: testNamespace,
			}, rbCM)).To(Succeed())

			// Item 0's target restored.
			target0 := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("cpc-patch-cm"), target0)).To(Succeed())
			Expect(target0.Data["key"]).To(Equal("original"))

			// Item 1's target still has external modification.
			Expect(k8sClient.Get(ctx, nn("cpc-upd-cm"), target1)).To(Succeed())
			Expect(target1.Data["key"]).To(Equal("externally-modified"))

			// Clean up.
			for _, name := range []string{"cpc-patch-cm", "cpc-upd-cm"} {
				cm := &corev1.ConfigMap{}
				_ = k8sClient.Get(ctx, nn(name), cm)
				_ = k8sClient.Delete(ctx, cm)
			}
		})
	})
})

// reconcileToPhase drives the reconciler until the transaction reaches the target phase.
func reconcileToPhase(r *TransactionReconciler, name string, target backupv1alpha1.TransactionPhase) {
	txn := &backupv1alpha1.Transaction{}
	for range 30 {
		Expect(k8sClient.Get(ctx, nn(name), txn)).To(Succeed())
		if txn.Status.Phase == target {
			return
		}
		_, err := r.Reconcile(ctx, req(name))
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(k8sClient.Get(ctx, nn(name), txn)).To(Succeed())
	Expect(txn.Status.Phase).To(Equal(target))
}

// minimalTxn creates a sealed Transaction with no inline changes.
// Callers must create ResourceChange CRs separately after the Transaction exists (to get UID).
func minimalTxn(name string) *backupv1alpha1.Transaction {
	return &backupv1alpha1.Transaction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: backupv1alpha1.TransactionSpec{
			ServiceAccountName: testSAName,
			Sealed:             true,
		},
	}
}

// createChange creates a ResourceChange CR owned by the named Transaction.
// The txnUID must be provided after the Transaction is created.
func createChange(txnName, changeName string, spec backupv1alpha1.ResourceChangeSpec, txnUID types.UID) *backupv1alpha1.ResourceChange {
	return &backupv1alpha1.ResourceChange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      changeName,
			Namespace: testNamespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: backupv1alpha1.GroupVersion.String(),
				Kind:       "Transaction",
				Name:       txnName,
				UID:        txnUID,
			}},
		},
		Spec: spec,
	}
}

// createCMChange creates a ResourceChange for creating a ConfigMap.
func createCMChange(txnName, cmName string, txnUID types.UID) *backupv1alpha1.ResourceChange {
	raw, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": cmName, "namespace": testNamespace},
		"data":       map[string]any{"k": "v"},
	})
	return createChange(txnName, cmName+"-change", backupv1alpha1.ResourceChangeSpec{
		Target: backupv1alpha1.ResourceRef{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Name:       cmName,
			Namespace:  testNamespace,
		},
		Type:    backupv1alpha1.ChangeTypeCreate,
		Content: runtime.RawExtension{Raw: raw},
	}, txnUID)
}

// nn returns a NamespacedName in the test namespace.
func nn(name string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: testNamespace}
}

// req returns a reconcile Request for the given name in the test namespace.
func req(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: nn(name)}
}

// blockerChange creates a ResourceChange CR used as a second item to trigger rollback.
func blockerChange(txnName, name string, txnUID types.UID) *backupv1alpha1.ResourceChange {
	raw, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": testNamespace},
		"data":       map[string]any{"k": "v"},
	})
	return createChange(txnName, name+"-change", backupv1alpha1.ResourceChangeSpec{
		Target: backupv1alpha1.ResourceRef{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Name:       name,
			Namespace:  testNamespace,
		},
		Type:    backupv1alpha1.ChangeTypeCreate,
		Content: runtime.RawExtension{Raw: raw},
		Order:   1, // Higher order so it processes after the main item
	}, txnUID)
}

// failingRenewLockMgr returns a fake lock manager whose Renew always returns ErrLockExpired.
func failingRenewLockMgr() *fakeLockMgr {
	return &fakeLockMgr{
		renewFn: func(_ context.Context, lease lock.LeaseRef, _ string, _ time.Duration) error {
			return &lock.ErrLockExpired{LeaseName: lease.Name}
		},
	}
}

// --- cleanForRestore unit tests ---

var _ = Describe("cleanForRestore", func() {
	It("should strip cluster-assigned metadata", func() {
		obj := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":              "test",
					"namespace":         "old-ns",
					"resourceVersion":   "12345",
					"uid":               "abc-123",
					"creationTimestamp": "2025-01-01T00:00:00Z",
					"generation":        int64(5),
					"managedFields":     []any{map[string]any{"manager": "test"}},
					"ownerReferences":   []any{map[string]any{"name": "owner"}},
					"finalizers":        []any{"test-finalizer"},
				},
				"status": map[string]any{
					"ready": true,
				},
				"data": map[string]any{
					"key": "value",
				},
			},
		}

		rollback.CleanForRestore(obj, "new-ns")

		Expect(obj.GetResourceVersion()).To(Equal(""))
		Expect(string(obj.GetUID())).To(Equal(""))
		ts := obj.GetCreationTimestamp()
		Expect(ts.IsZero()).To(BeTrue())
		Expect(obj.GetGeneration()).To(Equal(int64(0)))
		Expect(obj.GetManagedFields()).To(BeNil())
		Expect(obj.GetOwnerReferences()).To(HaveLen(1))
		Expect(obj.GetOwnerReferences()[0].Name).To(Equal("owner"))
		Expect(obj.GetFinalizers()).To(Equal([]string{"test-finalizer"}))
		_, hasStatus := obj.Object["status"]
		Expect(hasStatus).To(BeFalse())
		Expect(obj.GetNamespace()).To(Equal("new-ns"))

		// Data preserved.
		data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
		Expect(data["key"]).To(Equal("value"))
	})

	It("should not set namespace when targetNS is empty", func() {
		obj := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "test",
					"namespace": "original",
				},
			},
		}

		rollback.CleanForRestore(obj, "")
		Expect(obj.GetNamespace()).To(Equal("original"))
	})
})

// --- computeReversePatch unit tests ---

var _ = Describe("computeReversePatch", func() {
	It("should replace leaf values with prior state values", func() {
		forwardPatch := map[string]any{
			"data": map[string]any{
				"version": "2.0",
			},
		}
		priorState := map[string]any{
			"data": map[string]any{
				"version": "1.0",
				"other":   "untouched",
			},
		}
		result := computeReversePatch(forwardPatch, priorState)
		Expect(result).To(HaveKey("data"))
		data := result["data"].(map[string]any)
		Expect(data["version"]).To(Equal("1.0"))
		Expect(data).NotTo(HaveKey("other"), "untouched fields should not appear")
	})

	It("should omit fields that did not exist in prior state (added by patch)", func() {
		forwardPatch := map[string]any{
			"data": map[string]any{
				"existing": "new-val",
				"added":    "brand-new",
			},
		}
		priorState := map[string]any{
			"data": map[string]any{
				"existing": "old-val",
			},
		}
		result := computeReversePatch(forwardPatch, priorState)
		data := result["data"].(map[string]any)
		Expect(data["existing"]).To(Equal("old-val"))
		Expect(data).NotTo(HaveKey("added"))
	})

	It("should handle nested maps recursively", func() {
		forwardPatch := map[string]any{
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{
							map[string]any{"name": "web", "image": "app:v2"},
						},
					},
				},
			},
		}
		priorState := map[string]any{
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{
							map[string]any{"name": "web", "image": "app:v1"},
						},
					},
				},
			},
		}
		result := computeReversePatch(forwardPatch, priorState)
		containers := result["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"]
		Expect(containers).To(Equal(priorState["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"]))
	})

	It("should return empty map when forward patch touches no prior fields", func() {
		forwardPatch := map[string]any{
			"data": map[string]any{
				"newKey": "newVal",
			},
		}
		priorState := map[string]any{
			"data": map[string]any{
				"existingKey": "existingVal",
			},
		}
		result := computeReversePatch(forwardPatch, priorState)
		Expect(result).To(BeEmpty())
	})
})
