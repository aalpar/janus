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
	"fmt"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
)

// BackupContractReconciler reconciles a BackupContract object
type BackupContractReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=backup.janus.io,resources=backupcontracts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backup.janus.io,resources=backupcontracts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.janus.io,resources=backupcontracts/finalizers,verbs=update

func (r *BackupContractReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var contract backupv1alpha1.BackupContract
	if err := r.Get(ctx, req.NamespacedName, &contract); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Validate the contract and update the Valid condition.
	validationErrors := validateContract(&contract)

	condition := metav1.Condition{
		Type:               "Valid",
		ObservedGeneration: contract.Generation,
		LastTransitionTime: metav1.Now(),
	}

	if len(validationErrors) == 0 {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "Valid"
		condition.Message = "BackupContract is valid"
		log.Info("BackupContract validated successfully")
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "Invalid"
		condition.Message = strings.Join(validationErrors, "; ")
		log.Info("BackupContract validation failed", "errors", validationErrors)
	}

	apimeta.SetStatusCondition(&contract.Status.Conditions, condition)

	if err := r.Status().Update(ctx, &contract); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// validateContract checks the structural and semantic validity of a BackupContract.
func validateContract(contract *backupv1alpha1.BackupContract) []string {
	var errs []string

	// Quiesce requires resume.
	if contract.Spec.Quiesce != nil && contract.Spec.Resume == nil {
		errs = append(errs, "quiesce is defined but resume is not; both must be specified together")
	}

	// Validate CEL expressions compile.
	if contract.Spec.Quiesce != nil {
		if _, err := compileCELCondition(contract.Spec.Quiesce.Ready.Condition); err != nil {
			errs = append(errs, fmt.Sprintf("quiesce.ready.condition: %v", err))
		}
	}

	if contract.Spec.Resume != nil {
		if _, err := compileCELCondition(contract.Spec.Resume.Ready.Condition); err != nil {
			errs = append(errs, fmt.Sprintf("resume.ready.condition: %v", err))
		}
	}

	for i, step := range contract.Spec.Restore.Order {
		if step.Ready != nil {
			if _, err := compileCELCondition(step.Ready.Condition); err != nil {
				errs = append(errs, fmt.Sprintf("restore.order[%d].ready.condition: %v", i, err))
			}
		}
	}

	if _, err := compileCELCondition(contract.Spec.Restore.Verify.Condition); err != nil {
		errs = append(errs, fmt.Sprintf("restore.verify.condition: %v", err))
	}

	// Validate critical list is not empty.
	if len(contract.Spec.Critical) == 0 {
		errs = append(errs, "critical: at least one resource selector is required")
	}

	// Validate restore order is not empty.
	if len(contract.Spec.Restore.Order) == 0 {
		errs = append(errs, "restore.order: at least one restore step is required")
	}

	return errs
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupContractReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&backupv1alpha1.BackupContract{}).
		Named("backupcontract").
		Complete(r)
}
