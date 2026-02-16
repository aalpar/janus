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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
	"github.com/aalpar/janus/internal/impersonate"
	"github.com/aalpar/janus/internal/lock"
)

const (
	defaultTimeout   = 5 * time.Minute
	rollbackCMSuffix = "-rollback"
	finalizerName    = "backup.janus.io/lease-cleanup"
)

// cachedClient holds a lazily-initialized impersonating client.
// sync.Once ensures exactly one goroutine creates the client; others wait.
type cachedClient struct {
	once sync.Once
	cl   client.Client
	err  error
}

// TransactionReconciler reconciles a Transaction object.
type TransactionReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	BaseCfg  *rest.Config
	Mapper   apimeta.RESTMapper
	LockMgr  lock.Manager
	Recorder record.EventRecorder

	// impersonatedClients caches impersonating clients keyed by "namespace/saName".
	// Entries are evicted when SA validation fails (SA deleted/not found).
	impersonatedClients sync.Map // → *cachedClient
}

// +kubebuilder:rbac:groups=backup.janus.io,resources=transactions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=backup.janus.io,resources=transactions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=backup.janus.io,resources=transactions/finalizers,verbs=update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;impersonate

func (r *TransactionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var txn backupv1alpha1.Transaction
	if err := r.Get(ctx, req.NamespacedName, &txn); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Deletion in progress — release leases, remove finalizer.
	if !txn.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &txn)
	}

	// Terminal states — strip finalizer so subsequent deletes are instant.
	switch txn.Status.Phase {
	case backupv1alpha1.TransactionPhaseCommitted,
		backupv1alpha1.TransactionPhaseRolledBack:
		if controllerutil.RemoveFinalizer(&txn, finalizerName) {
			if err := r.Update(ctx, &txn); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	case backupv1alpha1.TransactionPhaseFailed:
		if r.hasUnrolledCommits(&txn) {
			rbCM := &corev1.ConfigMap{}
			if err := r.Get(ctx, client.ObjectKey{
				Name: txn.Status.RollbackRef, Namespace: txn.Namespace,
			}, rbCM); err == nil {
				log.Info("recovering failed transaction with un-rolled-back commits")
				r.event(&txn, corev1.EventTypeWarning, "RecoveryInitiated", "recovering failed transaction with un-rolled-back commits")
				return r.transition(ctx, &txn, backupv1alpha1.TransactionPhaseRollingBack)
			}
			log.Info("cannot recover: rollback ConfigMap missing")
			r.event(&txn, corev1.EventTypeWarning, "RecoveryBlocked", "cannot recover: rollback ConfigMap %q missing", txn.Status.RollbackRef)
		}
		if controllerutil.RemoveFinalizer(&txn, finalizerName) {
			if err := r.Update(ctx, &txn); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present before any work.
	if controllerutil.AddFinalizer(&txn, finalizerName) {
		if err := r.Update(ctx, &txn); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}

	// Phases that don't need an impersonating client.
	switch txn.Status.Phase {
	case "", backupv1alpha1.TransactionPhasePending:
		return r.handlePending(ctx, &txn)
	case backupv1alpha1.TransactionPhasePrepared:
		log.Info("all items prepared, transitioning to committing")
		return r.transition(ctx, &txn, backupv1alpha1.TransactionPhaseCommitting)
	}

	// Remaining phases operate on user resources — require an impersonating client.
	userClient, err := r.getImpersonatingClient(ctx, &txn)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch txn.Status.Phase {
	case backupv1alpha1.TransactionPhasePreparing:
		return r.handlePreparing(ctx, &txn, userClient)
	case backupv1alpha1.TransactionPhaseCommitting:
		return r.handleCommitting(ctx, &txn, userClient)
	case backupv1alpha1.TransactionPhaseRollingBack:
		return r.handleRollingBack(ctx, &txn, userClient)
	}

	return ctrl.Result{}, nil
}

// getImpersonatingClient validates the SA and returns a cached impersonating client.
func (r *TransactionReconciler) getImpersonatingClient(ctx context.Context, txn *backupv1alpha1.Transaction) (client.Client, error) {
	key := txn.Namespace + "/" + txn.Spec.ServiceAccountName

	// Validate that the named ServiceAccount still exists.
	// On failure, evict the cache entry so a recreated SA gets a fresh client.
	var sa corev1.ServiceAccount
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: txn.Namespace,
		Name:      txn.Spec.ServiceAccountName,
	}, &sa); err != nil {
		r.impersonatedClients.Delete(key)
		return nil, r.setFailed(ctx, txn,
			fmt.Sprintf("ServiceAccount %q not found in namespace %q: %v",
				txn.Spec.ServiceAccountName, txn.Namespace, err))
	}

	val, _ := r.impersonatedClients.LoadOrStore(key, &cachedClient{})
	entry := val.(*cachedClient)
	entry.once.Do(func() {
		entry.cl, entry.err = impersonate.NewClient(r.BaseCfg, r.Scheme, r.Mapper,
			txn.Namespace, txn.Spec.ServiceAccountName)
	})
	if entry.err != nil {
		r.impersonatedClients.Delete(key)
		return nil, r.setFailed(ctx, txn,
			fmt.Sprintf("building impersonating client: %v", entry.err))
	}
	return entry.cl, nil
}

// handleDeletion runs when a Transaction's DeletionTimestamp is set.
// It releases held leases (best-effort, Lease TTL as fallback) and removes
// the finalizer so the object can be garbage collected.
//
// This does NOT attempt rollback of partially-committed state — rollback is
// a business decision the user triggers explicitly via the RollingBack phase.
func (r *TransactionReconciler) handleDeletion(ctx context.Context, txn *backupv1alpha1.Transaction) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("handling deletion, releasing locks")

	leaseRefs := r.collectLeaseRefs(txn)
	if err := r.LockMgr.ReleaseAll(ctx, leaseRefs); err != nil {
		log.Error(err, "best-effort lease release failed during deletion")
	}

	controllerutil.RemoveFinalizer(txn, finalizerName)
	if err := r.Update(ctx, txn); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// handlePending initializes the transaction status and creates the rollback ConfigMap.
func (r *TransactionReconciler) handlePending(ctx context.Context, txn *backupv1alpha1.Transaction) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("initializing transaction", "changes", len(txn.Spec.Changes))
	r.event(txn, corev1.EventTypeNormal, "Initializing", "starting transaction with %d changes", len(txn.Spec.Changes))

	now := metav1.Now()
	txn.Status.StartedAt = &now
	txn.Status.Items = make([]backupv1alpha1.ItemStatus, len(txn.Spec.Changes))
	txn.Status.RollbackRef = txn.Name + rollbackCMSuffix

	// Create the rollback ConfigMap.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      txn.Status.RollbackRef,
			Namespace: txn.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "janus",
				"janus.io/transaction":         txn.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: backupv1alpha1.GroupVersion.String(),
				Kind:       "Transaction",
				Name:       txn.Name,
				UID:        txn.UID,
			}},
		},
		Data: map[string]string{},
	}
	if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, r.setFailed(ctx, txn, fmt.Sprintf("creating rollback ConfigMap: %v", err))
	}

	return r.transition(ctx, txn, backupv1alpha1.TransactionPhasePreparing)
}

// handlePreparing acquires locks and records prior state for each resource, one per reconcile.
func (r *TransactionReconciler) handlePreparing(ctx context.Context, txn *backupv1alpha1.Transaction, userClient client.Client) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	timeout := r.lockTimeout(txn)

	for i, change := range txn.Spec.Changes {
		if txn.Status.Items[i].Prepared {
			continue
		}

		ns := r.resolveNamespace(change.Target, txn.Namespace)
		key := lock.ResourceKey{Namespace: ns, Kind: change.Target.Kind, Name: change.Target.Name}

		// Acquire lock.
		leaseName, err := r.LockMgr.Acquire(ctx, key, txn.Name, timeout)
		if err != nil {
			log.Error(err, "lock acquisition failed", "item", i, "resource", key)
			r.event(txn, corev1.EventTypeWarning, "LockFailed", "item %d: lock acquisition failed for %s/%s: %v", i, change.Target.Kind, change.Target.Name, err)
			return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("item %d lock failed: %v", i, err))
		}
		txn.Status.Items[i].LockLease = leaseName
		txn.Status.Items[i].LeaseNamespace = ns

		// Read current state and store in rollback ConfigMap.
		if change.Type != backupv1alpha1.ChangeTypeCreate {
			obj, err := r.getResource(ctx, userClient, change.Target, ns)
			if err != nil {
				return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("item %d: reading current state: %v", i, err))
			}
			if err := r.saveRollbackState(ctx, txn, change, obj); err != nil {
				return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("item %d: saving rollback state: %v", i, err))
			}
		}

		txn.Status.Items[i].Prepared = true
		log.Info("item prepared", "item", i, "kind", change.Target.Kind, "name", change.Target.Name)

		// Persist progress and requeue — one item per reconcile cycle.
		return r.updateStatusAndRequeue(ctx, txn)
	}

	// All items prepared.
	return r.transition(ctx, txn, backupv1alpha1.TransactionPhasePrepared)
}

// handleCommitting applies each mutation, one per reconcile.
func (r *TransactionReconciler) handleCommitting(ctx context.Context, txn *backupv1alpha1.Transaction, userClient client.Client) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	for i, change := range txn.Spec.Changes {
		if txn.Status.Items[i].Committed {
			continue
		}

		// Renew the lock to prevent expiry during long commit phases.
		timeout := r.lockTimeout(txn)
		leaseRef := lock.LeaseRef{Name: txn.Status.Items[i].LockLease, Namespace: txn.Status.Items[i].LeaseNamespace}
		if err := r.LockMgr.Renew(ctx, leaseRef, txn.Name, timeout); err != nil {
			log.Error(err, "lock renewal failed, initiating rollback", "item", i)
			r.event(txn, corev1.EventTypeWarning, "LockRenewalFailed", "item %d: lock renewal failed, initiating rollback: %v", i, err)
			return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRollingBack)
		}

		ns := r.resolveNamespace(change.Target, txn.Namespace)
		if err := r.applyChange(ctx, userClient, change, ns, txn.Name); err != nil {
			txn.Status.Items[i].Error = err.Error()
			log.Error(err, "commit failed, initiating rollback", "item", i)
			r.event(txn, corev1.EventTypeWarning, "CommitFailed", "item %d: %s %s/%s failed: %v", i, change.Type, change.Target.Kind, change.Target.Name, err)
			return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRollingBack)
		}

		txn.Status.Items[i].Committed = true
		log.Info("item committed", "item", i, "type", change.Type, "kind", change.Target.Kind, "name", change.Target.Name)
		r.event(txn, corev1.EventTypeNormal, "ItemCommitted", "item %d: %s %s/%s committed", i, change.Type, change.Target.Kind, change.Target.Name)

		return r.updateStatusAndRequeue(ctx, txn)
	}

	// All committed — release locks and clean up.
	log.Info("all items committed, releasing locks")
	leaseRefs := r.collectLeaseRefs(txn)
	if err := r.LockMgr.ReleaseAll(ctx, leaseRefs); err != nil {
		log.Error(err, "best-effort lease release failed after commit")
	}

	// Delete rollback ConfigMap — no longer needed.
	rbCM := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: txn.Status.RollbackRef, Namespace: txn.Namespace}, rbCM); err == nil {
		if err := r.Delete(ctx, rbCM); err != nil {
			log.Error(err, "best-effort rollback ConfigMap cleanup failed", "configmap", txn.Status.RollbackRef)
		}
	}

	now := metav1.Now()
	txn.Status.CompletedAt = &now
	return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseCommitted)
}

// handleRollingBack reverses committed changes in reverse order, one per reconcile.
func (r *TransactionReconciler) handleRollingBack(ctx context.Context, txn *backupv1alpha1.Transaction, userClient client.Client) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Load rollback ConfigMap.
	rbCM := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: txn.Status.RollbackRef, Namespace: txn.Namespace}, rbCM); err != nil {
		log.Error(err, "rollback ConfigMap not found, marking failed")
		r.event(txn, corev1.EventTypeWarning, "RollbackConfigMapMissing", "rollback ConfigMap %q not found", txn.Status.RollbackRef)
		return ctrl.Result{}, r.setFailed(ctx, txn, fmt.Sprintf("rollback ConfigMap missing: %v", err))
	}

	// Iterate in reverse — rollback committed, not-yet-rolled-back items.
	for i := len(txn.Spec.Changes) - 1; i >= 0; i-- {
		item := &txn.Status.Items[i]
		if !item.Committed || item.RolledBack {
			continue
		}

		change := txn.Spec.Changes[i]
		ns := r.resolveNamespace(change.Target, txn.Namespace)

		if err := r.applyRollback(ctx, userClient, change, ns, rbCM); err != nil {
			item.Error = fmt.Sprintf("rollback failed: %v", err)
			log.Error(err, "rollback failed for item, will retry", "item", i)
			r.event(txn, corev1.EventTypeWarning, "RollbackFailed", "item %d: rollback failed for %s/%s, will retry: %v", i, change.Target.Kind, change.Target.Name, err)
			if statusErr := r.Status().Update(ctx, txn); statusErr != nil {
				log.Error(statusErr, "failed to persist rollback error on item status", "item", i)
			}
			return ctrl.Result{}, err // controller-runtime backoff
		}

		item.Error = "" // clear stale error from a previous failed attempt
		item.RolledBack = true
		log.Info("item rolled back", "item", i, "kind", change.Target.Kind, "name", change.Target.Name)
		r.event(txn, corev1.EventTypeNormal, "ItemRolledBack", "item %d: %s/%s rolled back", i, change.Target.Kind, change.Target.Name)

		return r.updateStatusAndRequeue(ctx, txn)
	}

	// All rolled back — release locks, preserve rollback CM for forensics.
	log.Info("rollback complete, releasing locks")
	leaseRefs := r.collectLeaseRefs(txn)
	if err := r.LockMgr.ReleaseAll(ctx, leaseRefs); err != nil {
		log.Error(err, "best-effort lease release failed after rollback")
	}

	now := metav1.Now()
	txn.Status.CompletedAt = &now
	return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRolledBack)
}

// --- Resource operations ---

// getResource fetches a resource by its ResourceRef using the given client.
func (r *TransactionReconciler) getResource(ctx context.Context, cl client.Client, ref backupv1alpha1.ResourceRef, namespace string) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, &ResourceOpError{Op: "parsing apiVersion", Ref: ref.APIVersion, Err: err}
	}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   gv.Group,
		Version: gv.Version,
		Kind:    ref.Kind,
	})

	if err := cl.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: namespace}, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// applyChange dispatches the appropriate mutation for a ResourceChange.
// txnName is used as the field manager identity for server-side apply patches.
func (r *TransactionReconciler) applyChange(ctx context.Context, cl client.Client, change backupv1alpha1.ResourceChange, namespace, txnName string) error {
	switch change.Type {
	case backupv1alpha1.ChangeTypeCreate:
		obj, err := r.unmarshalContent(change.Content, change.Target)
		if err != nil {
			return err
		}
		obj.SetNamespace(namespace)
		if err := cl.Create(ctx, obj); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil // Already created by a previous attempt.
			}
			return err
		}
		return nil

	case backupv1alpha1.ChangeTypeUpdate:
		obj, err := r.unmarshalContent(change.Content, change.Target)
		if err != nil {
			return err
		}
		obj.SetNamespace(namespace)
		// Fetch current resourceVersion for the update.
		existing, err := r.getResource(ctx, cl, change.Target, namespace)
		if err != nil {
			return &ResourceOpError{Op: "fetching for update", Err: err}
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		return cl.Update(ctx, obj)

	case backupv1alpha1.ChangeTypePatch:
		// Server-side apply: Janus owns only the fields specified in the patch.
		// Other controllers (HPA, etc.) retain ownership of their fields.
		obj, err := r.unmarshalContent(change.Content, change.Target)
		if err != nil {
			return err
		}
		obj.SetName(change.Target.Name)
		obj.SetNamespace(namespace)
		gv, err := schema.ParseGroupVersion(change.Target.APIVersion)
		if err != nil {
			return &ResourceOpError{Op: "parsing apiVersion for patch", Ref: change.Target.APIVersion, Err: err}
		}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group: gv.Group, Version: gv.Version, Kind: change.Target.Kind,
		})
		ac := client.ApplyConfigurationFromUnstructured(obj)
		return cl.Apply(ctx, ac, client.FieldOwner("janus-"+txnName), client.ForceOwnership)

	case backupv1alpha1.ChangeTypeDelete:
		existing, err := r.getResource(ctx, cl, change.Target, namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil // Already gone.
			}
			return &ResourceOpError{Op: "fetching for delete", Err: err}
		}
		return cl.Delete(ctx, existing)

	default:
		return fmt.Errorf("%w: %s", errUnknownChangeType, change.Type)
	}
}

// applyRollback reverses a committed change using the stored prior state.
func (r *TransactionReconciler) applyRollback(ctx context.Context, cl client.Client, change backupv1alpha1.ResourceChange, namespace string, rbCM *corev1.ConfigMap) error {
	rbKey := rollbackKey(change.Target, namespace)

	switch change.Type {
	case backupv1alpha1.ChangeTypeCreate:
		// Reverse of Create = Delete.
		existing, err := r.getResource(ctx, cl, change.Target, namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		return cl.Delete(ctx, existing)

	case backupv1alpha1.ChangeTypeDelete:
		// Reverse of Delete = re-Create from rollback state.
		data, ok := rbCM.Data[rbKey]
		if !ok {
			return &RollbackDataError{Key: rbKey}
		}
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal([]byte(data), &obj.Object); err != nil {
			return &RollbackDataError{Key: rbKey, Err: err}
		}
		cleanForRestore(obj, namespace)
		if err := cl.Create(ctx, obj); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil // Already restored by a previous attempt.
			}
			return err
		}
		return nil

	case backupv1alpha1.ChangeTypeUpdate, backupv1alpha1.ChangeTypePatch:
		// Reverse = restore the previous state.
		data, ok := rbCM.Data[rbKey]
		if !ok {
			return &RollbackDataError{Key: rbKey}
		}
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal([]byte(data), &obj.Object); err != nil {
			return &RollbackDataError{Key: rbKey, Err: err}
		}
		cleanForRestore(obj, namespace)
		// Fetch current resourceVersion for the update.
		existing, err := r.getResource(ctx, cl, change.Target, namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Resource was deleted externally — re-create.
				if createErr := cl.Create(ctx, obj); createErr != nil {
					if apierrors.IsAlreadyExists(createErr) {
						return nil
					}
					return createErr
				}
				return nil
			}
			return err
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		return cl.Update(ctx, obj)

	default:
		return fmt.Errorf("%w for rollback: %s", errUnknownChangeType, change.Type)
	}
}

// --- Helpers ---

func (r *TransactionReconciler) unmarshalContent(raw runtime.RawExtension, ref backupv1alpha1.ResourceRef) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw.Raw, &obj.Object); err != nil {
		return nil, &ResourceOpError{Op: "unmarshaling content", Ref: ref.Kind + "/" + ref.Name, Err: err}
	}
	return obj, nil
}

func (r *TransactionReconciler) saveRollbackState(ctx context.Context, txn *backupv1alpha1.Transaction, change backupv1alpha1.ResourceChange, obj *unstructured.Unstructured) error {
	ns := r.resolveNamespace(change.Target, txn.Namespace)
	key := rollbackKey(change.Target, ns)

	data, err := json.Marshal(obj.Object)
	if err != nil {
		return &ResourceOpError{Op: "serializing rollback state", Err: err}
	}

	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: txn.Status.RollbackRef, Namespace: txn.Namespace}, cm); err != nil {
		return &ResourceOpError{Op: "fetching rollback ConfigMap", Err: err}
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[key] = string(data)
	return r.Update(ctx, cm)
}

func (r *TransactionReconciler) resolveNamespace(ref backupv1alpha1.ResourceRef, txnNamespace string) string {
	if ref.Namespace != "" {
		return ref.Namespace
	}
	return txnNamespace
}

func (r *TransactionReconciler) lockTimeout(txn *backupv1alpha1.Transaction) time.Duration {
	if txn.Spec.LockTimeout != nil {
		return txn.Spec.LockTimeout.Duration
	}
	return defaultTimeout
}

func (r *TransactionReconciler) collectLeaseRefs(txn *backupv1alpha1.Transaction) []lock.LeaseRef {
	refs := make([]lock.LeaseRef, 0, len(txn.Status.Items))
	for _, item := range txn.Status.Items {
		if item.LockLease != "" {
			refs = append(refs, lock.LeaseRef{Name: item.LockLease, Namespace: item.LeaseNamespace})
		}
	}
	return refs
}

func (r *TransactionReconciler) transition(ctx context.Context, txn *backupv1alpha1.Transaction, phase backupv1alpha1.TransactionPhase) (ctrl.Result, error) {
	eventType := corev1.EventTypeNormal
	if phase == backupv1alpha1.TransactionPhaseRollingBack || phase == backupv1alpha1.TransactionPhaseFailed {
		eventType = corev1.EventTypeWarning
	}
	r.event(txn, eventType, "PhaseTransition", "transitioning to %s", phase)
	txn.Status.Phase = phase
	txn.Status.Version++
	if err := r.Status().Update(ctx, txn); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func (r *TransactionReconciler) updateStatusAndRequeue(ctx context.Context, txn *backupv1alpha1.Transaction) (ctrl.Result, error) {
	txn.Status.Version++
	if err := r.Status().Update(ctx, txn); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func (r *TransactionReconciler) setFailed(ctx context.Context, txn *backupv1alpha1.Transaction, message string) error {
	log := logf.FromContext(ctx)
	log.Error(fmt.Errorf("transaction failed"), message)
	now := metav1.Now()
	txn.Status.Phase = backupv1alpha1.TransactionPhaseFailed
	txn.Status.CompletedAt = &now
	txn.Status.Version++
	apimeta.SetStatusCondition(&txn.Status.Conditions, metav1.Condition{
		Type:               "Failed",
		Status:             metav1.ConditionTrue,
		Reason:             "TransactionFailed",
		Message:            message,
		LastTransitionTime: now,
	})
	return r.Status().Update(ctx, txn)
}

func (r *TransactionReconciler) failAndReleaseLocks(ctx context.Context, txn *backupv1alpha1.Transaction, message string) error {
	log := logf.FromContext(ctx)
	leaseRefs := r.collectLeaseRefs(txn)
	if err := r.LockMgr.ReleaseAll(ctx, leaseRefs); err != nil {
		log.Error(err, "best-effort lease release failed during failure handling")
	}
	return r.setFailed(ctx, txn, message)
}

// event emits a Kubernetes Event if the recorder is configured.
func (r *TransactionReconciler) event(txn *backupv1alpha1.Transaction, eventType, reason, messageFmt string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(txn, eventType, reason, messageFmt, args...)
	}
}

// hasUnrolledCommits reports whether any items were committed but not yet rolled back.
func (r *TransactionReconciler) hasUnrolledCommits(txn *backupv1alpha1.Transaction) bool {
	for _, item := range txn.Status.Items {
		if item.Committed && !item.RolledBack {
			return true
		}
	}
	return false
}

// rollbackKey produces the ConfigMap key for a resource's rollback state.
// Uses underscores as separators — K8s resource names (DNS-1123) and Kind values
// never contain underscores, so this is collision-free.
func rollbackKey(ref backupv1alpha1.ResourceRef, namespace string) string {
	return fmt.Sprintf("%s_%s_%s", ref.Kind, namespace, ref.Name)
}

// cleanForRestore strips cluster-assigned metadata from a resource so it can
// be re-created or updated in the cluster. OwnerReferences and finalizers are
// preserved — they were part of the original resource state and are needed to
// maintain GC chains and external controller contracts.
func cleanForRestore(obj *unstructured.Unstructured, targetNS string) {
	obj.SetResourceVersion("")
	obj.SetUID("")
	obj.SetCreationTimestamp(metav1.Time{})
	obj.SetGeneration(0)
	obj.SetManagedFields(nil)
	delete(obj.Object, "status")
	if targetNS != "" {
		obj.SetNamespace(targetNS)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *TransactionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&backupv1alpha1.Transaction{}).
		Named("transaction").
		Complete(r)
}
