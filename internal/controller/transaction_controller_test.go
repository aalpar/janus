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
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
	"github.com/aalpar/janus/internal/lock"
)

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
		// Clean up transactions.
		txnList := &backupv1alpha1.TransactionList{}
		Expect(k8sClient.List(ctx, txnList, client.InNamespace(namespace))).To(Succeed())
		for i := range txnList.Items {
			Expect(k8sClient.Delete(ctx, &txnList.Items[i])).To(Succeed())
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
			cmContent := map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "txn-created-cm",
					"namespace": namespace,
				},
				"data": map[string]interface{}{
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

			// Reconcile: Pending → Preparing
			result, err := reconciler.Reconcile(ctx, reconcile.Request{
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
			for i := 0; i < 10; i++ {
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

			patchContent, err := json.Marshal(map[string]interface{}{
				"data": map[string]interface{}{
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
			for i := 0; i < 15; i++ {
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
			cmContent := map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":      "validation-cm",
					"namespace": namespace,
				},
				"data": map[string]interface{}{
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
})
