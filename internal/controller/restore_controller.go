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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
)

// RestoreReconciler reconciles a Restore object
type RestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=backup.janus.io,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backup.janus.io,resources=restores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.janus.io,resources=restores/finalizers,verbs=update
// +kubebuilder:rbac:groups=backup.janus.io,resources=backups,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=create
// +kubebuilder:rbac:groups="*",resources="*",verbs=get;list;watch;create;update;patch

func (r *RestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var restore backupv1alpha1.Restore
	if err := r.Get(ctx, req.NamespacedName, &restore); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Terminal states — nothing to do.
	switch restore.Status.Phase {
	case backupv1alpha1.RestorePhaseCompleted, backupv1alpha1.RestorePhaseFailed:
		return ctrl.Result{}, nil
	}

	// Fetch the referenced Backup.
	var backup backupv1alpha1.Backup
	if err := r.Get(ctx, client.ObjectKey{Name: restore.Spec.BackupRef, Namespace: restore.Namespace}, &backup); err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &restore, fmt.Sprintf("Backup %q not found: %v", restore.Spec.BackupRef, err))
	}
	if backup.Status.Phase != backupv1alpha1.BackupPhaseCompleted {
		return ctrl.Result{}, r.setFailed(ctx, &restore, fmt.Sprintf("Backup %q is not completed (phase: %s)", restore.Spec.BackupRef, backup.Status.Phase))
	}

	// Fetch the BackupContract.
	var contract backupv1alpha1.BackupContract
	if err := r.Get(ctx, client.ObjectKey{Name: backup.Spec.ContractRef}, &contract); err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &restore, fmt.Sprintf("BackupContract %q not found: %v", backup.Spec.ContractRef, err))
	}

	// Resolve the target namespace.
	targetNS := restore.Spec.TargetNamespace
	if targetNS == "" {
		targetNS = restore.Namespace
	}

	// Load the backup manifest.
	var manifestCM corev1.ConfigMap
	manifestName := backup.Name + "-manifest"
	if err := r.Get(ctx, client.ObjectKey{Name: manifestName, Namespace: backup.Namespace}, &manifestCM); err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &restore, fmt.Sprintf("manifest ConfigMap %q not found: %v", manifestName, err))
	}

	switch restore.Status.Phase {
	case "", backupv1alpha1.RestorePhasePending:
		log.Info("starting restore", "backup", restore.Spec.BackupRef)
		now := metav1.Now()
		restore.Status.StartedAt = &now
		restore.Status.Steps = nil
		return r.transitionToStep(ctx, &restore, backupv1alpha1.RestorePhaseRestoring, 0, "restoring resources")

	case backupv1alpha1.RestorePhaseRestoring:
		return r.handleRestoring(ctx, &restore, &backup, &contract, &manifestCM, targetNS)

	case backupv1alpha1.RestorePhaseVerifying:
		return r.handleVerifying(ctx, &restore, &backup, &contract, targetNS)
	}

	return ctrl.Result{}, nil
}

func (r *RestoreReconciler) handleRestoring(
	ctx context.Context,
	restore *backupv1alpha1.Restore,
	backup *backupv1alpha1.Backup,
	contract *backupv1alpha1.BackupContract,
	manifestCM *corev1.ConfigMap,
	targetNS string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	stepIdx := restore.Status.CurrentStep
	if stepIdx >= len(contract.Spec.Restore.Order) {
		// All steps complete — transition to verification.
		log.Info("all restore steps complete, verifying")
		return r.transitionToStep(ctx, restore, backupv1alpha1.RestorePhaseVerifying, stepIdx, "verifying restore")
	}

	step := contract.Spec.Restore.Order[stepIdx]
	resolvedSel := r.resolveSelector(step.Selector, backup.Spec.TargetName)

	// Sub-phase: create resources.
	if restore.Status.Message == "restoring resources" {
		count, err := r.restoreStepResources(ctx, resolvedSel, backup, manifestCM, targetNS)
		if err != nil {
			return ctrl.Result{}, r.setFailed(ctx, restore, fmt.Sprintf("step %d (%s) failed: %v", stepIdx, step.Selector.Kind, err))
		}

		stepStatus := backupv1alpha1.RestoreStepStatus{
			Selector:          step.Selector,
			ResourcesRestored: count,
			Message:           fmt.Sprintf("restored %d %s resource(s)", count, step.Selector.Kind),
		}

		if step.Ready == nil {
			// No readiness check — step complete.
			stepStatus.Ready = true
			restore.Status.Steps = appendOrUpdateStep(restore.Status.Steps, stepIdx, stepStatus)
			log.Info("restore step complete (no readiness check)", "step", stepIdx, "kind", step.Selector.Kind)
			return r.transitionToStep(ctx, restore, backupv1alpha1.RestorePhaseRestoring, stepIdx+1, "restoring resources")
		}

		// Readiness check required — switch to waiting sub-phase.
		restore.Status.Steps = appendOrUpdateStep(restore.Status.Steps, stepIdx, stepStatus)
		restore.Status.Message = "waiting for readiness"
		if err := r.Status().Update(ctx, restore); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	// Sub-phase: wait for readiness.
	if restore.Status.Message == "waiting for readiness" {
		ready, err := r.checkStepReadiness(ctx, resolvedSel, targetNS, step.Ready)
		if err != nil {
			log.Error(err, "readiness check error", "step", stepIdx)
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}
		if !ready {
			if r.isTimedOut(restore, step.Ready.Timeout.Duration) {
				return ctrl.Result{}, r.setFailed(ctx, restore, fmt.Sprintf("step %d (%s) readiness timed out", stepIdx, step.Selector.Kind))
			}
			log.Info("step not ready, requeuing", "step", stepIdx, "kind", step.Selector.Kind)
			return ctrl.Result{RequeueAfter: requeueDelay}, nil
		}

		// Step ready — record and advance.
		if stepIdx < len(restore.Status.Steps) {
			restore.Status.Steps[stepIdx].Ready = true
			restore.Status.Steps[stepIdx].Message = "ready"
		}
		log.Info("restore step complete", "step", stepIdx, "kind", step.Selector.Kind)
		return r.transitionToStep(ctx, restore, backupv1alpha1.RestorePhaseRestoring, stepIdx+1, "restoring resources")
	}

	return ctrl.Result{}, nil
}

func (r *RestoreReconciler) handleVerifying(
	ctx context.Context,
	restore *backupv1alpha1.Restore,
	backup *backupv1alpha1.Backup,
	contract *backupv1alpha1.BackupContract,
	targetNS string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	target, err := r.getTarget(ctx, contract, backup.Spec.TargetName, targetNS)
	if err != nil {
		log.Error(err, "target not found for verification, requeuing")
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	ready, err := evaluateCELCondition(contract.Spec.Restore.Verify.Condition, target.Object)
	if err != nil {
		log.Error(err, "verification condition error")
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	if !ready {
		if r.isTimedOut(restore, contract.Spec.Restore.Verify.Timeout.Duration) {
			return ctrl.Result{}, r.setFailed(ctx, restore, "restore verification timed out")
		}
		log.Info("verification not ready, requeuing")
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	log.Info("restore verified and complete")
	return r.setCompleted(ctx, restore)
}

// restoreStepResources creates resources for a single restore step.
func (r *RestoreReconciler) restoreStepResources(
	ctx context.Context,
	sel backupv1alpha1.ResourceSelector,
	backup *backupv1alpha1.Backup,
	manifestCM *corev1.ConfigMap,
	targetNS string,
) (int, error) {
	count := 0
	for _, artifact := range backup.Status.Artifacts {
		if !artifactMatchesSelector(artifact, sel) {
			continue
		}

		if artifact.Kind == "PersistentVolumeClaim" {
			if err := r.restorePVCFromSnapshot(ctx, artifact, manifestCM, targetNS); err != nil {
				return count, fmt.Errorf("restoring PVC %s: %w", artifact.Name, err)
			}
		} else {
			if err := r.restoreFromManifest(ctx, artifact, manifestCM, targetNS); err != nil {
				return count, fmt.Errorf("restoring %s/%s: %w", artifact.Kind, artifact.Name, err)
			}
		}
		count++
	}
	return count, nil
}

// restoreFromManifest deserializes a resource from the manifest ConfigMap and creates it.
func (r *RestoreReconciler) restoreFromManifest(
	ctx context.Context,
	artifact backupv1alpha1.BackupArtifact,
	manifestCM *corev1.ConfigMap,
	targetNS string,
) error {
	key := fmt.Sprintf("%s/%s", artifact.Kind, artifact.Name)
	data, ok := manifestCM.Data[key]
	if !ok {
		return fmt.Errorf("key %q not found in manifest", key)
	}

	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(data), &obj.Object); err != nil {
		return fmt.Errorf("deserializing: %w", err)
	}

	cleanForRestore(obj, targetNS)

	if err := r.Create(ctx, obj); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating resource: %w", err)
		}
		// Already exists — update with the backup data.
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(obj.GroupVersionKind())
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
			return fmt.Errorf("fetching existing resource: %w", err)
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		if err := r.Update(ctx, obj); err != nil {
			return fmt.Errorf("updating existing resource: %w", err)
		}
	}
	return nil
}

// restorePVCFromSnapshot creates a PVC with a dataSource pointing to the backup VolumeSnapshot.
func (r *RestoreReconciler) restorePVCFromSnapshot(
	ctx context.Context,
	artifact backupv1alpha1.BackupArtifact,
	manifestCM *corev1.ConfigMap,
	targetNS string,
) error {
	log := logf.FromContext(ctx)

	key := fmt.Sprintf("PersistentVolumeClaim/%s", artifact.Name)
	data, ok := manifestCM.Data[key]
	if !ok {
		return fmt.Errorf("PVC %q not found in manifest", artifact.Name)
	}

	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(data), &obj.Object); err != nil {
		return fmt.Errorf("deserializing PVC: %w", err)
	}

	cleanForRestore(obj, targetNS)

	// Modify the PVC spec to restore from the VolumeSnapshot.
	spec, ok := obj.Object["spec"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("PVC %s has no spec", artifact.Name)
	}
	delete(spec, "volumeName") // remove existing PV binding
	spec["dataSource"] = map[string]interface{}{
		"name":     artifact.DataRef,
		"kind":     "VolumeSnapshot",
		"apiGroup": "snapshot.storage.k8s.io",
	}

	if err := r.Create(ctx, obj); err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.Info("PVC already exists, skipping", "name", artifact.Name)
			return nil
		}
		return fmt.Errorf("creating PVC from snapshot: %w", err)
	}
	return nil
}

// checkStepReadiness evaluates the readiness condition against all resources matching the selector.
func (r *RestoreReconciler) checkStepReadiness(
	ctx context.Context,
	sel backupv1alpha1.ResourceSelector,
	namespace string,
	check *backupv1alpha1.ReadinessCheck,
) (bool, error) {
	resources, err := r.listResources(ctx, sel, namespace)
	if err != nil {
		return false, err
	}
	if len(resources) == 0 {
		return false, nil
	}
	for _, res := range resources {
		ready, err := evaluateCELCondition(check.Condition, res.Object)
		if err != nil {
			return false, fmt.Errorf("evaluating readiness for %s/%s: %w", res.GetKind(), res.GetName(), err)
		}
		if !ready {
			return false, nil
		}
	}
	return true, nil
}

// --- Utility methods ---

func (r *RestoreReconciler) getTarget(ctx context.Context, contract *backupv1alpha1.BackupContract, name, namespace string) (*unstructured.Unstructured, error) {
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   contract.Spec.Target.APIGroup,
		Version: "v1",
		Kind:    contract.Spec.Target.Kind,
	})

	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, target); err != nil {
		for _, version := range []string{"v1alpha1", "v1beta1", "v1beta2"} {
			target.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   contract.Spec.Target.APIGroup,
				Version: version,
				Kind:    contract.Spec.Target.Kind,
			})
			if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, target); err == nil {
				return target, nil
			}
		}
		return nil, err
	}
	return target, nil
}

func (r *RestoreReconciler) listResources(ctx context.Context, sel backupv1alpha1.ResourceSelector, namespace string) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	gvk := schema.GroupVersionKind{
		Group:   sel.APIGroup,
		Version: "v1",
		Kind:    sel.Kind + "List",
	}
	list.SetGroupVersionKind(gvk)

	matchLabels := make(client.MatchingLabels)
	for k, v := range sel.Selector {
		matchLabels[k] = v
	}

	if err := r.List(ctx, list, client.InNamespace(namespace), matchLabels); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *RestoreReconciler) resolveSelector(sel backupv1alpha1.ResourceSelector, targetName string) backupv1alpha1.ResourceSelector {
	resolved := sel
	if len(sel.Selector) > 0 {
		resolved.Selector = make(map[string]string)
		for k, v := range sel.Selector {
			if v == "" {
				resolved.Selector[k] = targetName
			} else {
				resolved.Selector[k] = v
			}
		}
	}
	return resolved
}

func (r *RestoreReconciler) isTimedOut(restore *backupv1alpha1.Restore, timeout time.Duration) bool {
	if restore.Status.StartedAt == nil {
		return false
	}
	return time.Since(restore.Status.StartedAt.Time) > timeout
}

func (r *RestoreReconciler) transitionToStep(ctx context.Context, restore *backupv1alpha1.Restore, phase backupv1alpha1.RestorePhase, step int, message string) (ctrl.Result, error) {
	restore.Status.Phase = phase
	restore.Status.CurrentStep = step
	restore.Status.Message = message
	if restore.Status.StartedAt == nil {
		now := metav1.Now()
		restore.Status.StartedAt = &now
	}
	if err := r.Status().Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueDelay}, nil
}

func (r *RestoreReconciler) setCompleted(ctx context.Context, restore *backupv1alpha1.Restore) (ctrl.Result, error) {
	now := metav1.Now()
	restore.Status.Phase = backupv1alpha1.RestorePhaseCompleted
	restore.Status.CompletedAt = &now
	restore.Status.Message = fmt.Sprintf("restore completed (%d steps)", len(restore.Status.Steps))
	if err := r.Status().Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *RestoreReconciler) setFailed(ctx context.Context, restore *backupv1alpha1.Restore, message string) error {
	log := logf.FromContext(ctx)
	log.Error(fmt.Errorf("restore failed"), message)
	now := metav1.Now()
	restore.Status.Phase = backupv1alpha1.RestorePhaseFailed
	restore.Status.CompletedAt = &now
	restore.Status.Message = message
	return r.Status().Update(ctx, restore)
}

// SetupWithManager sets up the controller with the Manager.
func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&backupv1alpha1.Restore{}).
		Named("restore").
		Complete(r)
}

// --- Package-level helpers ---

// cleanForRestore strips cluster-assigned metadata from a resource
// so it can be re-created in the cluster.
func cleanForRestore(obj *unstructured.Unstructured, targetNS string) {
	obj.SetResourceVersion("")
	obj.SetUID("")
	obj.SetCreationTimestamp(metav1.Time{})
	obj.SetGeneration(0)
	obj.SetManagedFields(nil)
	obj.SetOwnerReferences(nil)
	obj.SetFinalizers(nil)
	delete(obj.Object, "status")
	if targetNS != "" {
		obj.SetNamespace(targetNS)
	}
}

// artifactMatchesSelector checks if a backup artifact matches a resource selector by kind and API group.
func artifactMatchesSelector(artifact backupv1alpha1.BackupArtifact, sel backupv1alpha1.ResourceSelector) bool {
	return artifact.Kind == sel.Kind && artifact.APIGroup == sel.APIGroup
}

// appendOrUpdateStep appends or overwrites a step status at the given index.
func appendOrUpdateStep(steps []backupv1alpha1.RestoreStepStatus, idx int, step backupv1alpha1.RestoreStepStatus) []backupv1alpha1.RestoreStepStatus {
	if idx < len(steps) {
		steps[idx] = step
		return steps
	}
	return append(steps, step)
}
