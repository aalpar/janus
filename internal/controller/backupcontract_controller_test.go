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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
)

var _ = Describe("BackupContract Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		backupcontract := &backupv1alpha1.BackupContract{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind BackupContract")
			err := k8sClient.Get(ctx, typeNamespacedName, backupcontract)
			if err != nil && errors.IsNotFound(err) {
				resource := &backupv1alpha1.BackupContract{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: backupv1alpha1.BackupContractSpec{
						Target: backupv1alpha1.TargetRef{
							APIGroup: "example.io",
							Kind:     "Example",
						},
						Critical: []backupv1alpha1.ResourceSelector{
							{Kind: "Secret"},
						},
						Restore: backupv1alpha1.RestorePolicy{
							Order: []backupv1alpha1.RestoreStep{
								{Selector: backupv1alpha1.ResourceSelector{Kind: "Secret"}},
							},
							Verify: backupv1alpha1.ReadinessCheck{
								Condition: "true",
								Timeout:   metav1.Duration{Duration: 5 * time.Minute},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &backupv1alpha1.BackupContract{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance BackupContract")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &BackupContractReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
