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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
)

const requeueDelay = 5 * time.Second

// BackupReconciler reconciles a Backup object
type BackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=backup.janus.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backup.janus.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.janus.io,resources=backups/finalizers,verbs=update
// +kubebuilder:rbac:groups=backup.janus.io,resources=backupcontracts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="*",resources="*",verbs=get;list;watch;patch

func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var backup backupv1alpha1.Backup
	if err := r.Get(ctx, req.NamespacedName, &backup); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Terminal states — nothing to do.
	switch backup.Status.Phase {
	case backupv1alpha1.BackupPhaseCompleted, backupv1alpha1.BackupPhaseFailed:
		return ctrl.Result{}, nil
	}

	// Fetch the BackupContract.
	var contract backupv1alpha1.BackupContract
	if err := r.Get(ctx, client.ObjectKey{Name: backup.Spec.ContractRef}, &contract); err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &backup, fmt.Sprintf("BackupContract %q not found: %v", backup.Spec.ContractRef, err))
	}

	// Resolve the target namespace.
	targetNS := backup.Spec.TargetNamespace
	if targetNS == "" {
		targetNS = backup.Namespace
	}

	// Fetch the target resource.
	target, err := r.getTarget(ctx, &contract, backup.Spec.TargetName, targetNS)
	if err != nil {
		return ctrl.Result{}, r.setFailed(ctx, &backup, fmt.Sprintf("target resource not found: %v", err))
	}

	switch backup.Status.Phase {
	case "", backupv1alpha1.BackupPhasePending:
		log.Info("starting backup", "target", backup.Spec.TargetName)
		now := metav1.Now()
		backup.Status.StartedAt = &now

		if contract.Spec.Quiesce != nil {
			return r.transitionTo(ctx, &backup, backupv1alpha1.BackupPhaseQuiescing, "applying quiesce patch")
		}
		// No quiesce defined — go directly to snapshotting.
		return r.transitionTo(ctx, &backup, backupv1alpha1.BackupPhaseSnapshotting, "no quiesce defined, snapshotting directly")

	case backupv1alpha1.BackupPhaseQuiescing:
		return r.handleQuiescing(ctx, &backup, &contract, target, targetNS)

	case backupv1alpha1.BackupPhaseSnapshotting:
		return r.handleSnapshotting(ctx, &backup, &contract, target, targetNS)

	case backupv1alpha1.BackupPhaseResuming:
		return r.handleResuming(ctx, &backup, &contract, target)
	}

	return ctrl.Result{}, nil
}

func (r *BackupReconciler) handleQuiescing(ctx context.Context, backup *backupv1alpha1.Backup, contract *backupv1alpha1.BackupContract, target *unstructured.Unstructured, targetNS string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Apply the quiesce patch if not yet applied.
	if backup.Status.Message == "applying quiesce patch" {
		if err := r.applyPatch(ctx, target, contract.Spec.Quiesce.Patch); err != nil {
			return ctrl.Result{}, r.setFailedAndResume(ctx, backup, contract, target, fmt.Sprintf("quiesce patch failed: %v", err))
		}
		backup.Status.Message = "waiting for quiesce readiness"
		if err := r.Status().Update(ctx, backup); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check readiness.
	ready, err := r.checkReadiness(ctx, target, &contract.Spec.Quiesce.Ready)
	if err != nil {
		log.Error(err, "readiness check error during quiesce")
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	if !ready {
		// Check timeout.
		if r.isTimedOut(backup, contract.Spec.Quiesce.Ready.Timeout.Duration) {
			return ctrl.Result{}, r.setFailedAndResume(ctx, backup, contract, target, "quiesce timed out")
		}
		log.Info("quiesce not ready yet, requeuing")
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	log.Info("quiesce complete")
	return r.transitionTo(ctx, backup, backupv1alpha1.BackupPhaseSnapshotting, "quiesce complete, snapshotting")
}

func (r *BackupReconciler) handleSnapshotting(ctx context.Context, backup *backupv1alpha1.Backup, contract *backupv1alpha1.BackupContract, target *unstructured.Unstructured, targetNS string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	artifacts, err := r.snapshotResources(ctx, backup, contract, target, targetNS)
	if err != nil {
		return ctrl.Result{}, r.setFailedAndResume(ctx, backup, contract, target, fmt.Sprintf("snapshot failed: %v", err))
	}

	backup.Status.Artifacts = artifacts
	log.Info("snapshot complete", "artifacts", len(artifacts))

	if contract.Spec.Resume != nil {
		return r.transitionTo(ctx, backup, backupv1alpha1.BackupPhaseResuming, "applying resume patch")
	}

	// No resume defined — done.
	return r.setCompleted(ctx, backup)
}

func (r *BackupReconciler) handleResuming(ctx context.Context, backup *backupv1alpha1.Backup, contract *backupv1alpha1.BackupContract, target *unstructured.Unstructured) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if backup.Status.Message == "applying resume patch" {
		if err := r.applyPatch(ctx, target, contract.Spec.Resume.Patch); err != nil {
			return ctrl.Result{}, r.setFailed(ctx, backup, fmt.Sprintf("resume patch failed: %v", err))
		}
		backup.Status.Message = "waiting for resume readiness"
		if err := r.Status().Update(ctx, backup); err != nil {
			return ctrl.Result{}, err
		}
	}

	ready, err := r.checkReadiness(ctx, target, &contract.Spec.Resume.Ready)
	if err != nil {
		log.Error(err, "readiness check error during resume")
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}
	if !ready {
		if r.isTimedOut(backup, contract.Spec.Resume.Ready.Timeout.Duration) {
			return ctrl.Result{}, r.setFailed(ctx, backup, "resume timed out")
		}
		log.Info("resume not ready yet, requeuing")
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	log.Info("resume complete")
	return r.setCompleted(ctx, backup)
}

// snapshotResources captures all critical resources and stores them.
func (r *BackupReconciler) snapshotResources(ctx context.Context, backup *backupv1alpha1.Backup, contract *backupv1alpha1.BackupContract, target *unstructured.Unstructured, targetNS string) ([]backupv1alpha1.BackupArtifact, error) {
	var artifacts []backupv1alpha1.BackupArtifact
	manifestData := make(map[string]string)

	for _, sel := range contract.Spec.Critical {
		resolvedSelector := r.resolveSelector(sel, target.GetName())

		if sel.Kind == "PersistentVolumeClaim" {
			// For PVCs: store metadata in manifest AND create VolumeSnapshots.
			pvcArtifacts, pvcData, err := r.snapshotPVCs(ctx, resolvedSelector, targetNS, backup.Name)
			if err != nil {
				return nil, fmt.Errorf("snapshotting PVCs: %w", err)
			}
			artifacts = append(artifacts, pvcArtifacts...)
			for k, v := range pvcData {
				manifestData[k] = v
			}
			continue
		}

		// For other resources: serialize to JSON and store in manifest.
		resources, err := r.listResources(ctx, resolvedSelector, targetNS)
		if err != nil {
			return nil, fmt.Errorf("listing %s resources: %w", sel.Kind, err)
		}

		for _, res := range resources {
			key := fmt.Sprintf("%s/%s", res.GetKind(), res.GetName())
			data, err := json.Marshal(res.Object)
			if err != nil {
				return nil, fmt.Errorf("serializing %s: %w", key, err)
			}
			manifestData[key] = string(data)
			artifacts = append(artifacts, backupv1alpha1.BackupArtifact{
				APIGroup:  res.GetObjectKind().GroupVersionKind().Group,
				Kind:      res.GetKind(),
				Name:      res.GetName(),
				Namespace: res.GetNamespace(),
				DataRef:   backup.Name + "-manifest",
			})
		}
	}

	// Also snapshot the target resource itself.
	targetKey := fmt.Sprintf("%s/%s", target.GetKind(), target.GetName())
	targetData, err := json.Marshal(target.Object)
	if err != nil {
		return nil, fmt.Errorf("serializing target: %w", err)
	}
	manifestData[targetKey] = string(targetData)
	artifacts = append(artifacts, backupv1alpha1.BackupArtifact{
		APIGroup:  target.GetObjectKind().GroupVersionKind().Group,
		Kind:      target.GetKind(),
		Name:      target.GetName(),
		Namespace: target.GetNamespace(),
		DataRef:   backup.Name + "-manifest",
	})

	// Store all serialized resources in a ConfigMap.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backup.Name + "-manifest",
			Namespace: backup.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
				"backup.janus.io/backup":       backup.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: backup.APIVersion,
					Kind:       backup.Kind,
					Name:       backup.Name,
					UID:        backup.UID,
				},
			},
		},
		Data: manifestData,
	}

	if err := r.Create(ctx, cm); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := r.Update(ctx, cm); err != nil {
				return nil, fmt.Errorf("updating manifest ConfigMap: %w", err)
			}
		} else {
			return nil, fmt.Errorf("creating manifest ConfigMap: %w", err)
		}
	}

	return artifacts, nil
}

// snapshotPVCs creates VolumeSnapshots for each PVC and returns artifacts + manifest data.
func (r *BackupReconciler) snapshotPVCs(ctx context.Context, sel backupv1alpha1.ResourceSelector, namespace, backupName string) ([]backupv1alpha1.BackupArtifact, map[string]string, error) {
	var artifacts []backupv1alpha1.BackupArtifact
	data := make(map[string]string)

	resources, err := r.listResources(ctx, sel, namespace)
	if err != nil {
		return nil, nil, err
	}

	for _, pvc := range resources {
		// Store PVC metadata in manifest.
		key := fmt.Sprintf("PersistentVolumeClaim/%s", pvc.GetName())
		pvcData, err := json.Marshal(pvc.Object)
		if err != nil {
			return nil, nil, fmt.Errorf("serializing PVC %s: %w", pvc.GetName(), err)
		}
		data[key] = string(pvcData)

		// Create a VolumeSnapshot for the PVC's data.
		snapshotName := fmt.Sprintf("%s-%s", backupName, pvc.GetName())
		vs := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "snapshot.storage.k8s.io/v1",
				"kind":       "VolumeSnapshot",
				"metadata": map[string]interface{}{
					"name":      snapshotName,
					"namespace": namespace,
					"labels": map[string]interface{}{
						"app.kubernetes.io/managed-by": "janus",
						"backup.janus.io/backup":       backupName,
					},
				},
				"spec": map[string]interface{}{
					"source": map[string]interface{}{
						"persistentVolumeClaimName": pvc.GetName(),
					},
				},
			},
		}

		if err := r.Create(ctx, vs); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, nil, fmt.Errorf("creating VolumeSnapshot for PVC %s: %w", pvc.GetName(), err)
		}

		artifacts = append(artifacts, backupv1alpha1.BackupArtifact{
			Kind:      "PersistentVolumeClaim",
			Name:      pvc.GetName(),
			Namespace: namespace,
			DataRef:   snapshotName,
		})
	}

	return artifacts, data, nil
}

// getTarget fetches the target resource identified by the BackupContract.
func (r *BackupReconciler) getTarget(ctx context.Context, contract *backupv1alpha1.BackupContract, name, namespace string) (*unstructured.Unstructured, error) {
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   contract.Spec.Target.APIGroup,
		Version: "v1", // TODO: make version configurable in TargetRef
		Kind:    contract.Spec.Target.Kind,
	})

	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, target); err != nil {
		// Try common version patterns if v1 fails.
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

// applyPatch applies a strategic merge patch to a resource.
func (r *BackupReconciler) applyPatch(ctx context.Context, obj *unstructured.Unstructured, patchAction *backupv1alpha1.PatchAction) error {
	if patchAction == nil {
		return nil
	}

	patchData, err := json.Marshal(patchAction.Merge.Raw)
	if err != nil {
		return fmt.Errorf("marshaling patch: %w", err)
	}

	// Use merge patch since we're working with unstructured resources.
	patch := client.RawPatch(types.MergePatchType, patchAction.Merge.Raw)
	_ = patchData // raw bytes used directly via RawPatch
	return r.Patch(ctx, obj, patch)
}

// checkReadiness evaluates the CEL readiness condition against a resource.
func (r *BackupReconciler) checkReadiness(ctx context.Context, obj *unstructured.Unstructured, check *backupv1alpha1.ReadinessCheck) (bool, error) {
	// Re-fetch the resource to get the latest status.
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(obj.GroupVersionKind())
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), latest); err != nil {
		return false, fmt.Errorf("fetching latest state: %w", err)
	}

	return evaluateCELCondition(check.Condition, latest.Object)
}

// listResources lists resources matching a selector in a namespace.
func (r *BackupReconciler) listResources(ctx context.Context, sel backupv1alpha1.ResourceSelector, namespace string) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}

	gvk := schema.GroupVersionKind{
		Group:   sel.APIGroup,
		Version: "v1",
		Kind:    sel.Kind + "List",
	}
	// For core resources, use v1 directly.
	if sel.APIGroup != "" {
		gvk.Version = "v1" // TODO: discover version from API
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

// resolveSelector substitutes empty selector values with the target resource name.
func (r *BackupReconciler) resolveSelector(sel backupv1alpha1.ResourceSelector, targetName string) backupv1alpha1.ResourceSelector {
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

// isTimedOut checks if the backup has exceeded the given timeout since it started.
func (r *BackupReconciler) isTimedOut(backup *backupv1alpha1.Backup, timeout time.Duration) bool {
	if backup.Status.StartedAt == nil {
		return false
	}
	return time.Since(backup.Status.StartedAt.Time) > timeout
}

// transitionTo updates the backup phase and message, then requeues.
func (r *BackupReconciler) transitionTo(ctx context.Context, backup *backupv1alpha1.Backup, phase backupv1alpha1.BackupPhase, message string) (ctrl.Result, error) {
	backup.Status.Phase = phase
	backup.Status.Message = message
	if backup.Status.StartedAt == nil {
		now := metav1.Now()
		backup.Status.StartedAt = &now
	}
	if err := r.Status().Update(ctx, backup); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueDelay}, nil
}

// setCompleted marks the backup as completed.
func (r *BackupReconciler) setCompleted(ctx context.Context, backup *backupv1alpha1.Backup) (ctrl.Result, error) {
	now := metav1.Now()
	backup.Status.Phase = backupv1alpha1.BackupPhaseCompleted
	backup.Status.CompletedAt = &now
	backup.Status.Message = fmt.Sprintf("backup completed with %d artifacts", len(backup.Status.Artifacts))
	if err := r.Status().Update(ctx, backup); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// setFailed marks the backup as failed.
func (r *BackupReconciler) setFailed(ctx context.Context, backup *backupv1alpha1.Backup, message string) error {
	log := logf.FromContext(ctx)
	log.Error(fmt.Errorf("backup failed"), message)
	now := metav1.Now()
	backup.Status.Phase = backupv1alpha1.BackupPhaseFailed
	backup.Status.CompletedAt = &now
	backup.Status.Message = message
	return r.Status().Update(ctx, backup)
}

// setFailedAndResume attempts to resume the target before marking the backup as failed.
// This is the compensation action — if we quiesced the target and something went wrong,
// we should try to un-quiesce it before giving up.
func (r *BackupReconciler) setFailedAndResume(ctx context.Context, backup *backupv1alpha1.Backup, contract *backupv1alpha1.BackupContract, target *unstructured.Unstructured, message string) error {
	log := logf.FromContext(ctx)

	if contract.Spec.Resume != nil && contract.Spec.Resume.Patch != nil {
		log.Info("attempting compensating resume after failure")
		if err := r.applyPatch(ctx, target, contract.Spec.Resume.Patch); err != nil {
			log.Error(err, "compensating resume also failed")
			message = fmt.Sprintf("%s (compensating resume also failed: %v)", message, err)
		}
	}

	return r.setFailed(ctx, backup, message)
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&backupv1alpha1.Backup{}).
		Named("backup").
		Complete(r)
}
