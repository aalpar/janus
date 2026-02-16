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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
	"github.com/aalpar/janus/internal/lock"
)

const testNamespace = "default"

// fakeLockMgr is a controllable mock for lock.Manager used to trigger rollback paths.
type fakeLockMgr struct {
	acquireFn    func(ctx context.Context, key lock.ResourceKey, txnName string, timeout time.Duration) (string, error)
	releaseFn    func(ctx context.Context, leaseName string) error
	releaseAllFn func(ctx context.Context, txnName string, leaseNames []string) error
	isHeldByFn   func(ctx context.Context, leaseName string, txnName string) (bool, error)
}

func (f *fakeLockMgr) Acquire(ctx context.Context, key lock.ResourceKey, txnName string, timeout time.Duration) (string, error) {
	if f.acquireFn != nil {
		return f.acquireFn(ctx, key, txnName, timeout)
	}
	return lock.LeaseName(key), nil
}

func (f *fakeLockMgr) Release(ctx context.Context, leaseName string) error {
	if f.releaseFn != nil {
		return f.releaseFn(ctx, leaseName)
	}
	return nil
}

func (f *fakeLockMgr) ReleaseAll(ctx context.Context, txnName string, leaseNames []string) error {
	if f.releaseAllFn != nil {
		return f.releaseAllFn(ctx, txnName, leaseNames)
	}
	return nil
}

func (f *fakeLockMgr) IsHeldBy(ctx context.Context, leaseName string, txnName string) (bool, error) {
	if f.isHeldByFn != nil {
		return f.isHeldByFn(ctx, leaseName, txnName)
	}
	return true, nil
}

var _ = Describe("Transaction Controller", func() {
	const (
		txnName   = "test-txn"
		namespace = "default"
		timeout   = 10 * time.Second
		interval  = 250 * time.Millisecond
	)

	var (
		reconciler *TransactionReconciler
	)

	BeforeEach(func() {
		reconciler = &TransactionReconciler{
			Client:  k8sClient,
			Scheme:  k8sClient.Scheme(),
			LockMgr: &lock.LeaseManager{Client: k8sClient},
		}
	})

	AfterEach(func() {
		// Clean up transactions — strip finalizers first so envtest GC can proceed.
		txnList := &backupv1alpha1.TransactionList{}
		Expect(k8sClient.List(ctx, txnList, client.InNamespace(namespace))).To(Succeed())
		for i := range txnList.Items {
			t := &txnList.Items[i]
			if controllerutil.RemoveFinalizer(t, finalizerName) {
				Expect(k8sClient.Update(ctx, t)).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, t))).To(Succeed())
		}

		// Clean up ConfigMaps created by the controller.
		cmList := &corev1.ConfigMapList{}
		Expect(k8sClient.List(ctx, cmList, client.InNamespace(namespace),
			client.MatchingLabels{"app.kubernetes.io/managed-by": "janus"})).To(Succeed())
		for i := range cmList.Items {
			Expect(k8sClient.Delete(ctx, &cmList.Items[i])).To(Succeed())
		}
	})

	Context("when creating a Transaction to create a ConfigMap", func() {
		It("should progress through phases to Committed", func() {
			cmContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "txn-created-cm",
					"namespace": namespace,
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
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "txn-created-cm",
							Namespace:  namespace,
						},
						Type:    backupv1alpha1.ChangeTypeCreate,
						Content: runtime.RawExtension{Raw: raw},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Reconcile: adds finalizer.
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: txnName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: txnName, Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(finalizerName))

			// Reconcile: Pending → Preparing
			result, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: txnName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Re-fetch to see updated status.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: txnName, Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhasePreparing))

			// Reconcile: Preparing → Prepared (single item, no prior state for Create)
			result, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: txnName, Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: txnName, Namespace: namespace}, txn)).To(Succeed())
			// Should be Prepared or already Committing after the lock+prepare step.
			Expect(txn.Status.Items[0].Prepared).To(BeTrue())

			// Keep reconciling until committed.
			for range 10 {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: txnName, Namespace: namespace}, txn)).To(Succeed())
				if txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted {
					break
				}
				_, err = reconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: txnName, Namespace: namespace},
				})
				Expect(err).NotTo(HaveOccurred())
			}

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: txnName, Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))
			Expect(txn.Status.Items[0].Committed).To(BeTrue())

			// Verify the ConfigMap was actually created.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "txn-created-cm", Namespace: namespace}, cm)).To(Succeed())
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
					Namespace: namespace,
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
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "existing-cm",
							Namespace:  namespace,
						},
						Type:    backupv1alpha1.ChangeTypePatch,
						Content: runtime.RawExtension{Raw: patchContent},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Reconcile through all phases.
			for range 15 {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "update-txn", Namespace: namespace}, txn)).To(Succeed())
				if txn.Status.Phase == backupv1alpha1.TransactionPhaseCommitted {
					break
				}
				_, err = reconciler.Reconcile(ctx, ctrl.Request{
					NamespacedName: types.NamespacedName{Name: "update-txn", Namespace: namespace},
				})
				Expect(err).NotTo(HaveOccurred())
			}

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "update-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))

			// Verify the ConfigMap was patched.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "existing-cm", Namespace: namespace}, cm)).To(Succeed())
			Expect(cm.Data["original"]).To(Equal("modified"))
			Expect(cm.Data["added"]).To(Equal("new-value"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("when a transaction has an empty changes list validation", func() {
		It("should create the Transaction object with changes", func() {
			cmContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "validation-cm",
					"namespace": namespace,
				},
				"data": map[string]any{
					"test": "data",
				},
			}
			raw, err := json.Marshal(cmContent)
			Expect(err).NotTo(HaveOccurred())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "valid-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "validation-cm",
						},
						Type:    backupv1alpha1.ChangeTypeCreate,
						Content: runtime.RawExtension{Raw: raw},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())
			Expect(txn.Spec.Changes).To(HaveLen(1))
		})
	})

	Context("when deleting an existing ConfigMap", func() {
		It("should delete the resource and reach Committed", func() {
			// Create the target ConfigMap.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "delete-target-cm",
					Namespace: namespace,
				},
				Data: map[string]string{"keep": "me"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "delete-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "delete-target-cm",
							Namespace:  namespace,
						},
						Type: backupv1alpha1.ChangeTypeDelete,
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			reconcileToPhase(reconciler, "delete-txn", backupv1alpha1.TransactionPhaseCommitted)

			// Verify the ConfigMap is gone.
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "delete-target-cm", Namespace: namespace}, &corev1.ConfigMap{})
			Expect(err).To(HaveOccurred())
			Expect(client.IgnoreNotFound(err)).To(Succeed())
		})
	})

	Context("when updating an existing ConfigMap (full replace)", func() {
		It("should fully replace the resource data", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "update-replace-cm",
					Namespace: namespace,
				},
				Data: map[string]string{"old-key": "old-val"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			updateContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "update-replace-cm",
					"namespace": namespace,
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
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "update-replace-cm",
							Namespace:  namespace,
						},
						Type:    backupv1alpha1.ChangeTypeUpdate,
						Content: runtime.RawExtension{Raw: raw},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			reconcileToPhase(reconciler, "update-replace-txn", backupv1alpha1.TransactionPhaseCommitted)

			// Verify full replacement.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "update-replace-cm", Namespace: namespace}, cm)).To(Succeed())
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
					Namespace: namespace,
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

			// Item 1: Create a new ConfigMap (lock check will fail before this commits).
			createContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "rb-new-cm",
					"namespace": namespace,
				},
				"data": map[string]any{"k": "v"},
			}
			createRaw, _ := json.Marshal(createContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-multi-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "rb-patch-cm",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypePatch,
							Content: runtime.RawExtension{Raw: patchContent},
						},
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "rb-new-cm",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypeCreate,
							Content: runtime.RawExtension{Raw: createRaw},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Use real lock manager through prepare and first commit.
			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Reconcile through Pending → Preparing → Prepared → Committing.
			reconcileToPhase(reconciler, "rb-multi-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Reconcile once more: commits item 0 (patch).
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "rb-multi-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify item 0 is committed.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-multi-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(txn.Status.Items[1].Committed).To(BeFalse())

			// Verify the ConfigMap was actually patched.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-patch-cm", Namespace: namespace}, cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("patched"))

			// Now swap to fake lock manager: item 1's lock check fails.
			reconciler.LockMgr = &fakeLockMgr{
				isHeldByFn: func(_ context.Context, _ string, _ string) (bool, error) {
					return false, nil
				},
			}

			// Reconcile: Committing → RollingBack (item 1 lock check fails).
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "rb-multi-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-multi-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Reconcile: RollingBack → rolls back item 0's patch → RolledBack.
			reconcileToPhase(reconciler, "rb-multi-txn", backupv1alpha1.TransactionPhaseRolledBack)

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-multi-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRolledBack))
			Expect(txn.Status.CompletedAt).NotTo(BeNil())
			Expect(txn.Status.Items[0].RolledBack).To(BeTrue())

			// Verify the ConfigMap was restored to its original state.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-patch-cm", Namespace: namespace}, cm)).To(Succeed())
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
				"metadata":   map[string]any{"name": "lock-fail-cm", "namespace": namespace},
				"data":       map[string]any{"k": "v"},
			}
			raw, _ := json.Marshal(cmContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "lock-fail-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "lock-fail-cm",
							Namespace:  namespace,
						},
						Type:    backupv1alpha1.ChangeTypeCreate,
						Content: runtime.RawExtension{Raw: raw},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Add finalizer.
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "lock-fail-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Pending → Preparing.
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "lock-fail-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Preparing → Failed (lock acquisition fails, triggers failAndReleaseLocks).
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "lock-fail-txn", Namespace: namespace},
			})
			// setFailed returns the status update error, which should be nil on success.
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "lock-fail-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))
			Expect(txn.Status.CompletedAt).NotTo(BeNil())
			Expect(txn.Status.Conditions).NotTo(BeEmpty())
			Expect(txn.Status.Conditions[0].Type).To(Equal("Failed"))
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
					"namespace": namespace,
				},
				"data": map[string]any{"k": "created"},
			}
			createRaw, _ := json.Marshal(createContent)

			// Item 1: Create another ConfigMap (lock check will fail before commit).
			create2Content := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "rb-created-cm-2",
					"namespace": namespace,
				},
				"data": map[string]any{"k": "v2"},
			}
			create2Raw, _ := json.Marshal(create2Content)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-create-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "rb-created-cm",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypeCreate,
							Content: runtime.RawExtension{Raw: createRaw},
						},
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "rb-created-cm-2",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypeCreate,
							Content: runtime.RawExtension{Raw: create2Raw},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Use real lock manager through prepare and first commit.
			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Reconcile to Committing.
			reconcileToPhase(reconciler, "rb-create-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "rb-create-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify item 0 created the ConfigMap.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-created-cm", Namespace: namespace}, cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("created"))

			// Fail item 1's lock check.
			reconciler.LockMgr = &fakeLockMgr{
				isHeldByFn: func(_ context.Context, _ string, _ string) (bool, error) {
					return false, nil
				},
			}

			// Committing → RollingBack.
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "rb-create-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// RollingBack → RolledBack (reverses item 0's Create by Deleting).
			reconcileToPhase(reconciler, "rb-create-txn", backupv1alpha1.TransactionPhaseRolledBack)

			// Verify the created ConfigMap was deleted during rollback.
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "rb-created-cm", Namespace: namespace}, cm)
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
					Namespace: namespace,
				},
				Data: map[string]string{"preserved": "data"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			// Item 0: Delete the ConfigMap.
			// Item 1: Create another ConfigMap (will fail lock check).
			create2Content := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "rb-delete-blocker",
					"namespace": namespace,
				},
				"data": map[string]any{"k": "v"},
			}
			create2Raw, _ := json.Marshal(create2Content)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "rb-delete-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "rb-deleted-cm",
								Namespace:  namespace,
							},
							Type: backupv1alpha1.ChangeTypeDelete,
						},
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "rb-delete-blocker",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypeCreate,
							Content: runtime.RawExtension{Raw: create2Raw},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Reconcile to Committing.
			reconcileToPhase(reconciler, "rb-delete-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0 (Delete).
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "rb-delete-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify ConfigMap was deleted.
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "rb-deleted-cm", Namespace: namespace}, cm)
			Expect(err).To(HaveOccurred())
			Expect(client.IgnoreNotFound(err)).To(Succeed())

			// Fail item 1's lock check.
			reconciler.LockMgr = &fakeLockMgr{
				isHeldByFn: func(_ context.Context, _ string, _ string) (bool, error) {
					return false, nil
				},
			}

			// Committing → RollingBack.
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "rb-delete-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// RollingBack → RolledBack (reverses item 0's Delete by re-Creating).
			reconcileToPhase(reconciler, "rb-delete-txn", backupv1alpha1.TransactionPhaseRolledBack)

			// Verify the ConfigMap was re-created from rollback state.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "rb-deleted-cm", Namespace: namespace}, cm)).To(Succeed())
			Expect(cm.Data["preserved"]).To(Equal("data"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("when reconciling a non-existent transaction", func() {
		It("should return no error", func() {
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("when a transaction is already in a terminal state", func() {
		It("should no-op for Committed", func() {
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "terminal-committed",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "dummy",
						},
						Type:    backupv1alpha1.ChangeTypeCreate,
						Content: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"dummy"}}`)},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Manually set to Committed.
			txn.Status.Phase = backupv1alpha1.TransactionPhaseCommitted
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "terminal-committed", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// Verify phase unchanged.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "terminal-committed", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseCommitted))
		})

		It("should no-op for RolledBack", func() {
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "terminal-rolledback",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "dummy2",
						},
						Type:    backupv1alpha1.ChangeTypeCreate,
						Content: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"dummy2"}}`)},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			txn.Status.Phase = backupv1alpha1.TransactionPhaseRolledBack
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "terminal-rolledback", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("should no-op for Failed with no un-rolled-back commits", func() {
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "terminal-failed",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "dummy3",
						},
						Type:    backupv1alpha1.ChangeTypeCreate,
						Content: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"dummy3"}}`)},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			txn.Status.Phase = backupv1alpha1.TransactionPhaseFailed
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "terminal-failed", Namespace: namespace},
			})
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
					"namespace": namespace,
				},
				"data": map[string]any{"k": "v1"},
			}
			cm2Content := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "multi-cm-2",
					"namespace": namespace,
				},
				"data": map[string]any{"k": "v2"},
			}
			raw1, _ := json.Marshal(cm1Content)
			raw2, _ := json.Marshal(cm2Content)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "multi-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "multi-cm-1",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypeCreate,
							Content: runtime.RawExtension{Raw: raw1},
						},
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "multi-cm-2",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypeCreate,
							Content: runtime.RawExtension{Raw: raw2},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			reconcileToPhase(reconciler, "multi-txn", backupv1alpha1.TransactionPhaseCommitted)

			// Verify both ConfigMaps were created.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "multi-cm-1", Namespace: namespace}, cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("v1"))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "multi-cm-2", Namespace: namespace}, cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("v2"))

			// Verify both items committed.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "multi-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Items).To(HaveLen(2))
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(txn.Status.Items[1].Committed).To(BeTrue())

			// Clean up.
			Expect(k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "multi-cm-1", Namespace: namespace}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "multi-cm-2", Namespace: namespace}})).To(Succeed())
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
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "ns-inherited-cm",
							// Namespace intentionally empty.
						},
						Type:    backupv1alpha1.ChangeTypeCreate,
						Content: runtime.RawExtension{Raw: raw},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			reconcileToPhase(reconciler, "ns-txn", backupv1alpha1.TransactionPhaseCommitted)

			// Verify the ConfigMap was created in the transaction's namespace.
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "ns-inherited-cm", Namespace: namespace}, cm)).To(Succeed())
			Expect(cm.Data["k"]).To(Equal("v"))

			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("finalizer lifecycle", func() {
		It("should add the finalizer on first reconcile", func() {
			txn := minimalTxn("fin-add")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "fin-add", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fin-add", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(finalizerName))
		})

		It("should remove the finalizer at terminal states", func() {
			txn := minimalTxn("fin-terminal")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Add finalizer.
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "fin-terminal", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fin-terminal", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Finalizers).To(ContainElement(finalizerName))

			// Manually set to Committed.
			txn.Status.Phase = backupv1alpha1.TransactionPhaseCommitted
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Reconcile: terminal → strip finalizer.
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "fin-terminal", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fin-terminal", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Finalizers).NotTo(ContainElement(finalizerName))
		})

		It("should release leases on deletion during Preparing", func() {
			var releasedLeases []string
			reconciler.LockMgr = &fakeLockMgr{
				releaseAllFn: func(_ context.Context, _ string, leases []string) error {
					releasedLeases = leases
					return nil
				},
			}

			txn := minimalTxn("fin-del-prep")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Add finalizer + Pending → Preparing.
			reconcileToPhase(reconciler, "fin-del-prep", backupv1alpha1.TransactionPhasePreparing)

			// One more reconcile to prepare items (acquire leases).
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "fin-del-prep", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify leases were acquired.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fin-del-prep", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Items[0].LockLease).NotTo(BeEmpty())

			// Delete the transaction — finalizer prevents immediate removal.
			Expect(k8sClient.Delete(ctx, txn)).To(Succeed())

			// Reconcile: handleDeletion → release leases, remove finalizer.
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "fin-del-prep", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify leases were released.
			Expect(releasedLeases).To(HaveLen(1))

			// Object should be gone (finalizer removed → GC).
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "fin-del-prep", Namespace: namespace}, txn)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("should handle deletion of Pending transaction gracefully", func() {
			reconciler.LockMgr = &fakeLockMgr{}

			txn := minimalTxn("fin-del-pending")
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Add finalizer.
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "fin-del-pending", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Delete before any leases are acquired.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fin-del-pending", Namespace: namespace}, txn)).To(Succeed())
			Expect(k8sClient.Delete(ctx, txn)).To(Succeed())

			// Reconcile: handleDeletion with no leases.
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "fin-del-pending", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Object should be gone.
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "fin-del-pending", Namespace: namespace}, txn)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("idempotent rollback of Delete (re-Create)", func() {
		It("should succeed when called twice (AlreadyExists on second attempt)", func() {
			// Create and commit a Delete transaction, then trigger rollback.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "idemp-rb-del-cm",
					Namespace: namespace,
				},
				Data: map[string]string{"key": "value"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			// Build a 2-item transaction: item 0 = Delete (will commit), item 1 = Create (lock fails).
			create2Raw, _ := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "idemp-rb-del-blocker", "namespace": namespace},
				"data":       map[string]any{"k": "v"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "idemp-rb-del-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "idemp-rb-del-cm",
								Namespace:  namespace,
							},
							Type: backupv1alpha1.ChangeTypeDelete,
						},
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "idemp-rb-del-blocker",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypeCreate,
							Content: runtime.RawExtension{Raw: create2Raw},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			// Progress to Committing.
			reconcileToPhase(reconciler, "idemp-rb-del-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0 (Delete).
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "idemp-rb-del-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Fail item 1's lock check → triggers rollback.
			reconciler.LockMgr = &fakeLockMgr{
				isHeldByFn: func(_ context.Context, _ string, _ string) (bool, error) {
					return false, nil
				},
			}
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "idemp-rb-del-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "idemp-rb-del-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Rollback re-Creates the deleted CM. Do one reconcile to rollback item 0.
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "idemp-rb-del-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify the CM was re-created.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "idemp-rb-del-cm", Namespace: namespace}, cm)).To(Succeed())

			// Now simulate a crash: reset item 0's RolledBack to false (as if status update failed).
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "idemp-rb-del-txn", Namespace: namespace}, txn)).To(Succeed())
			txn.Status.Items[0].RolledBack = false
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Second rollback attempt for same item — should succeed (AlreadyExists → nil).
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "idemp-rb-del-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Should still complete to RolledBack.
			reconcileToPhase(reconciler, "idemp-rb-del-txn", backupv1alpha1.TransactionPhaseRolledBack)

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})

	Context("idempotent commit Create", func() {
		It("should succeed when resource already exists from a previous attempt", func() {
			cmContent := map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "idemp-create-cm", "namespace": namespace},
				"data":       map[string]any{"k": "v"},
			}
			raw, _ := json.Marshal(cmContent)

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "idemp-create-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "idemp-create-cm",
							Namespace:  namespace,
						},
						Type:    backupv1alpha1.ChangeTypeCreate,
						Content: runtime.RawExtension{Raw: raw},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Progress to Committing.
			reconcileToPhase(reconciler, "idemp-create-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Pre-create the ConfigMap to simulate crash-after-create-before-status-update.
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "idemp-create-cm",
					Namespace: namespace,
				},
				Data: map[string]string{"k": "v"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			// Reconcile: should not fail despite AlreadyExists.
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "idemp-create-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Should progress to Committed.
			reconcileToPhase(reconciler, "idemp-create-txn", backupv1alpha1.TransactionPhaseCommitted)

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
					Namespace: namespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, _ := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})
			create2Raw, _ := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "retry-rb-blocker", "namespace": namespace},
				"data":       map[string]any{"k": "v"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "retry-rb-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "retry-rb-cm",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypePatch,
							Content: runtime.RawExtension{Raw: patchContent},
						},
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "retry-rb-blocker",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypeCreate,
							Content: runtime.RawExtension{Raw: create2Raw},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "retry-rb-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "retry-rb-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Trigger rollback by failing item 1 lock check.
			reconciler.LockMgr = &fakeLockMgr{
				isHeldByFn: func(_ context.Context, _ string, _ string) (bool, error) {
					return false, nil
				},
			}
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "retry-rb-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "retry-rb-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Corrupt the rollback CM data to force applyRollback to fail.
			rbCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: txn.Status.RollbackRef, Namespace: namespace,
			}, rbCM)).To(Succeed())
			// Replace the stored state with invalid JSON.
			for key := range rbCM.Data {
				rbCM.Data[key] = "not-json"
			}
			Expect(k8sClient.Update(ctx, rbCM)).To(Succeed())

			// Reconcile: rollback should fail but txn should stay in RollingBack.
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "retry-rb-txn", Namespace: namespace},
			})
			Expect(err).To(HaveOccurred()) // error returned for backoff

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "retry-rb-txn", Namespace: namespace}, txn)).To(Succeed())
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
					Namespace: namespace,
				},
				Data: map[string]string{"key": "val"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, _ := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})
			create2Raw, _ := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "missing-rbcm-blocker", "namespace": namespace},
				"data":       map[string]any{"k": "v"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "missing-rbcm-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "missing-rbcm-cm",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypePatch,
							Content: runtime.RawExtension{Raw: patchContent},
						},
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "missing-rbcm-blocker",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypeCreate,
							Content: runtime.RawExtension{Raw: create2Raw},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "missing-rbcm-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "missing-rbcm-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Trigger rollback.
			reconciler.LockMgr = &fakeLockMgr{
				isHeldByFn: func(_ context.Context, _ string, _ string) (bool, error) {
					return false, nil
				},
			}
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "missing-rbcm-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "missing-rbcm-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Delete the rollback ConfigMap.
			rbCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: txn.Status.RollbackRef, Namespace: namespace,
			}, rbCM)).To(Succeed())
			Expect(k8sClient.Delete(ctx, rbCM)).To(Succeed())

			// Reconcile: missing CM → Failed.
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "missing-rbcm-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "missing-rbcm-txn", Namespace: namespace}, txn)).To(Succeed())
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
					Namespace: namespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, _ := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})
			create2Raw, _ := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "recover-blocker", "namespace": namespace},
				"data":       map[string]any{"k": "v"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "recover-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "recover-cm",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypePatch,
							Content: runtime.RawExtension{Raw: patchContent},
						},
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "recover-blocker",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypeCreate,
							Content: runtime.RawExtension{Raw: create2Raw},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "recover-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "recover-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Manually set to Failed with un-rolled-back commit (simulates old behavior).
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "recover-txn", Namespace: namespace}, txn)).To(Succeed())
			txn.Status.Phase = backupv1alpha1.TransactionPhaseFailed
			now := metav1.Now()
			txn.Status.CompletedAt = &now
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Verify items: item 0 committed but not rolled back.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "recover-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Items[0].Committed).To(BeTrue())
			Expect(txn.Status.Items[0].RolledBack).To(BeFalse())

			// Reconcile: Failed → RollingBack (recovery).
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "recover-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "recover-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseRollingBack))

			// Continue reconciling to RolledBack.
			reconcileToPhase(reconciler, "recover-txn", backupv1alpha1.TransactionPhaseRolledBack)

			// Verify rollback restored the original data.
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "recover-cm", Namespace: namespace}, cm)).To(Succeed())
			Expect(cm.Data["key"]).To(Equal("original"))

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})

		It("should stay Failed when un-rolled-back commits exist but rollback CM is missing", func() {
			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "no-recover-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{{
						Target: backupv1alpha1.ResourceRef{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       "no-recover-cm",
						},
						Type:    backupv1alpha1.ChangeTypeCreate,
						Content: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"no-recover-cm"}}`)},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			// Manually set to Failed with un-rolled-back commit and no rollback CM.
			txn.Status.Phase = backupv1alpha1.TransactionPhaseFailed
			txn.Status.RollbackRef = "no-recover-txn-rollback"
			txn.Status.Items = []backupv1alpha1.ItemStatus{{
				Committed:  true,
				RolledBack: false,
			}}
			now := metav1.Now()
			txn.Status.CompletedAt = &now
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Reconcile: no CM → stays Failed, strips finalizer.
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "no-recover-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "no-recover-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Phase).To(Equal(backupv1alpha1.TransactionPhaseFailed))
		})
	})

	Context("stale error cleared on successful retry", func() {
		It("should clear item.Error after successful rollback retry", func() {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "stale-err-cm",
					Namespace: namespace,
				},
				Data: map[string]string{"key": "original"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())

			patchContent, _ := json.Marshal(map[string]any{
				"data": map[string]any{"key": "patched"},
			})
			create2Raw, _ := json.Marshal(map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "stale-err-blocker", "namespace": namespace},
				"data":       map[string]any{"k": "v"},
			})

			txn := &backupv1alpha1.Transaction{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "stale-err-txn",
					Namespace: namespace,
				},
				Spec: backupv1alpha1.TransactionSpec{
					Changes: []backupv1alpha1.ResourceChange{
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "stale-err-cm",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypePatch,
							Content: runtime.RawExtension{Raw: patchContent},
						},
						{
							Target: backupv1alpha1.ResourceRef{
								APIVersion: "v1",
								Kind:       "ConfigMap",
								Name:       "stale-err-blocker",
								Namespace:  namespace,
							},
							Type:    backupv1alpha1.ChangeTypeCreate,
							Content: runtime.RawExtension{Raw: create2Raw},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, txn)).To(Succeed())

			realLockMgr := &lock.LeaseManager{Client: k8sClient}
			reconciler.LockMgr = realLockMgr

			reconcileToPhase(reconciler, "stale-err-txn", backupv1alpha1.TransactionPhaseCommitting)

			// Commit item 0.
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "stale-err-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Trigger rollback.
			reconciler.LockMgr = &fakeLockMgr{
				isHeldByFn: func(_ context.Context, _ string, _ string) (bool, error) {
					return false, nil
				},
			}
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "stale-err-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			// Inject a stale error on item 0 (simulates a previously failed rollback attempt).
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "stale-err-txn", Namespace: namespace}, txn)).To(Succeed())
			txn.Status.Items[0].Error = "rollback failed: transient network error"
			Expect(k8sClient.Status().Update(ctx, txn)).To(Succeed())

			// Reconcile: rollback succeeds → error should be cleared.
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "stale-err-txn", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "stale-err-txn", Namespace: namespace}, txn)).To(Succeed())
			Expect(txn.Status.Items[0].Error).To(BeEmpty())
			Expect(txn.Status.Items[0].RolledBack).To(BeTrue())

			// Clean up.
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		})
	})
})

// reconcileToPhase drives the reconciler until the transaction reaches the target phase.
func reconcileToPhase(r *TransactionReconciler, name string, target backupv1alpha1.TransactionPhase) {
	txn := &backupv1alpha1.Transaction{}
	for range 30 {
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, txn)).To(Succeed())
		if txn.Status.Phase == target {
			return
		}
		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
		})
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, txn)).To(Succeed())
	Expect(txn.Status.Phase).To(Equal(target))
}

// minimalTxn creates a Transaction with a single Create-ConfigMap change.
func minimalTxn(name string) *backupv1alpha1.Transaction {
	raw, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name + "-cm", "namespace": testNamespace},
		"data":       map[string]any{"k": "v"},
	})
	return &backupv1alpha1.Transaction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: backupv1alpha1.TransactionSpec{
			Changes: []backupv1alpha1.ResourceChange{{
				Target: backupv1alpha1.ResourceRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       name + "-cm",
					Namespace:  testNamespace,
				},
				Type:    backupv1alpha1.ChangeTypeCreate,
				Content: runtime.RawExtension{Raw: raw},
			}},
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

		cleanForRestore(obj, "new-ns")

		Expect(obj.GetResourceVersion()).To(Equal(""))
		Expect(string(obj.GetUID())).To(Equal(""))
		ts := obj.GetCreationTimestamp()
		Expect(ts.IsZero()).To(BeTrue())
		Expect(obj.GetGeneration()).To(Equal(int64(0)))
		Expect(obj.GetManagedFields()).To(BeNil())
		Expect(obj.GetOwnerReferences()).To(BeNil())
		Expect(obj.GetFinalizers()).To(BeNil())
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

		cleanForRestore(obj, "")
		Expect(obj.GetNamespace()).To(Equal("original"))
	})
})
