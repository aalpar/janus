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
	"errors"
	"fmt"
	"sort"
	"strings"
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
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	backupv1alpha1 "github.com/aalpar/janus/api/v1alpha1"
	"github.com/aalpar/janus/internal/impersonate"
	"github.com/aalpar/janus/internal/lock"
	txmetrics "github.com/aalpar/janus/internal/metrics"
	"github.com/aalpar/janus/internal/rollback"
)

const (
	defaultTimeout              = 5 * time.Minute
	defaultTransactionTimeout   = 30 * time.Minute
	rollbackCMSuffix            = "-rollback"
	finalizerName               = "tx.janus.io/lease-cleanup"
	rollbackProtectionFinalizer = "tx.janus.io/rollback-protection"
	annotationAutoRollback      = backupv1alpha1.AnnotationAutoRollback
	annotationRetryRollback     = backupv1alpha1.AnnotationRetryRollback
	annotationRequestRollback   = backupv1alpha1.AnnotationRequestRollback
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

// +kubebuilder:rbac:groups=tx.janus.io,resources=transactions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tx.janus.io,resources=transactions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tx.janus.io,resources=transactions/finalizers,verbs=update
// +kubebuilder:rbac:groups=tx.janus.io,resources=resourcechanges,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;impersonate

func (r *TransactionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var txn backupv1alpha1.Transaction
	if err := r.Get(ctx, req.NamespacedName, &txn); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Ensure rollback-protection finalizer is present on non-terminal, non-deleting Transactions.
	// This finalizer is user-managed: the controller adds it but never removes it.
	// Once in a terminal phase or being deleted, don't re-add (the user may have removed it).
	if txn.DeletionTimestamp.IsZero() && !isTerminalPhase(txn.Status.Phase) {
		if controllerutil.AddFinalizer(&txn, rollbackProtectionFinalizer) {
			if err := r.Update(ctx, &txn); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Millisecond}, nil
		}
	}

	// Deletion in progress — phase-aware cleanup with rollback if needed.
	if !txn.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &txn)
	}

	// Terminal states — handle annotation-triggered rollback, auto-recovery, and finalizer cleanup.
	if isTerminalPhase(txn.Status.Phase) {
		return r.handleTerminal(ctx, &txn)
	}

	// Ignore unsealed transactions that haven't started processing.
	if !txn.Spec.Sealed && (txn.Status.Phase == "" || txn.Status.Phase == backupv1alpha1.TransactionPhasePending) {
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present before any work.
	if controllerutil.AddFinalizer(&txn, finalizerName) {
		if err := r.Update(ctx, &txn); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}

	// Check transaction-level timeout for non-terminal, in-progress phases.
	if timedOut, result, err := r.checkTimeout(ctx, &txn); timedOut {
		return result, err
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

// checkTimeout handles transaction-level timeout for non-terminal phases.
// Returns (true, result, err) if the timeout fired, (false, _, _) otherwise.
func (r *TransactionReconciler) checkTimeout(ctx context.Context, txn *backupv1alpha1.Transaction) (bool, ctrl.Result, error) {
	if txn.Status.StartedAt == nil || isTerminalPhase(txn.Status.Phase) {
		return false, ctrl.Result{}, nil
	}
	deadline := txn.Status.StartedAt.Add(r.transactionTimeout(txn))
	if !time.Now().After(deadline) {
		return false, ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)
	elapsed := time.Since(txn.Status.StartedAt.Time).Round(time.Second)
	if txn.Status.Phase == backupv1alpha1.TransactionPhaseRollingBack {
		log.Info("rollback timed out", "elapsed", elapsed)
		return true, ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("rollback timed out after %s", elapsed))
	}
	if r.hasUnrolledCommits(txn) {
		log.Info("transaction timed out with committed items, initiating rollback", "elapsed", elapsed)
		r.event(txn, corev1.EventTypeWarning, "Timeout", "transaction timed out after %s, rolling back", elapsed)
		now := metav1.Now()
		txn.Status.StartedAt = &now // Reset so rollback gets a fresh timeout window.
		result, err := r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRollingBack)
		return true, result, err
	}
	log.Info("transaction timed out with no commits", "elapsed", elapsed)
	return true, ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("transaction timed out after %s", elapsed))
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

// cleanupAndReturn releases all locks, removes the lease-cleanup finalizer, and persists.
func (r *TransactionReconciler) cleanupAndReturn(ctx context.Context, txn *backupv1alpha1.Transaction) (ctrl.Result, error) {
	r.releaseAllLocks(ctx, txn)
	controllerutil.RemoveFinalizer(txn, finalizerName)
	if err := r.Update(ctx, txn); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// handleTerminal runs for transactions in a terminal phase (Committed, RolledBack, Failed).
// It handles annotation-triggered rollback for Committed transactions, auto-recovery for
// Failed transactions with un-rolled-back commits, and lease-cleanup finalizer removal.
func (r *TransactionReconciler) handleTerminal(ctx context.Context, txn *backupv1alpha1.Transaction) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	switch txn.Status.Phase {
	case backupv1alpha1.TransactionPhaseCommitted:
		// Check for user-requested rollback before settling into terminal state.
		if _, ok := txn.Annotations[annotationRequestRollback]; ok {
			log.Info("request-rollback annotation found on committed transaction")
			r.event(txn, corev1.EventTypeWarning, "RequestRollback",
				"user requested rollback of committed transaction")
			delete(txn.Annotations, annotationRequestRollback)
			if err := r.Update(ctx, txn); err != nil {
				return ctrl.Result{}, err
			}
			return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRollingBack)
		}

	case backupv1alpha1.TransactionPhaseFailed:
		if r.hasUnrolledCommits(txn) {
			rbCM := &corev1.ConfigMap{}
			if err := r.Get(ctx, client.ObjectKey{
				Name: txn.Status.RollbackRef, Namespace: txn.Namespace,
			}, rbCM); err == nil {
				log.Info("recovering failed transaction with un-rolled-back commits")
				r.event(txn, corev1.EventTypeWarning, "RecoveryInitiated", "recovering failed transaction with un-rolled-back commits")
				return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRollingBack)
			}
			log.Info("cannot recover: rollback ConfigMap missing")
			r.event(txn, corev1.EventTypeWarning, "RecoveryBlocked", "cannot recover: rollback ConfigMap %q missing", txn.Status.RollbackRef)
		}
	}

	// Strip lease-cleanup finalizer so subsequent deletes are instant.
	// rollbackProtectionFinalizer is user-managed; the controller never removes it.
	if controllerutil.RemoveFinalizer(txn, finalizerName) {
		if err := r.Update(ctx, txn); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// handleDeletion runs when a Transaction's DeletionTimestamp is set.
// It performs phase-aware cleanup: if committed changes haven't been rolled
// back, it initiates rollback (controlled by the automatic-rollback and
// retry-rollback annotations). The rollback-protection finalizer prevents
// GC until rollback succeeds or the user removes it manually.
func (r *TransactionReconciler) handleDeletion(ctx context.Context, txn *backupv1alpha1.Transaction) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Terminal phases — nothing left to protect or roll back.
	switch txn.Status.Phase {
	case backupv1alpha1.TransactionPhaseCommitted,
		backupv1alpha1.TransactionPhaseRolledBack:
		log.Info("deleting terminal transaction, releasing locks and removing finalizer")
		return r.cleanupAndReturn(ctx, txn)

	case backupv1alpha1.TransactionPhaseRollingBack:
		// Rollback in progress — let it complete (even if all items are already rolled back,
		// the handler must run to transition the phase to RolledBack).
		userClient, err := r.getImpersonatingClient(ctx, txn)
		if err != nil {
			return ctrl.Result{}, err
		}
		return r.handleRollingBack(ctx, txn, userClient)
	}

	if !r.hasUnrolledCommits(txn) {
		log.Info("no unrolled commits, releasing locks and removing finalizer")
		return r.cleanupAndReturn(ctx, txn)
	}

	// Has unrolled commits from here down.
	switch txn.Status.Phase {
	case backupv1alpha1.TransactionPhaseFailed:
		// Rollback was attempted and failed.
		_, hasRetry := txn.Annotations[annotationRetryRollback]
		if hasRetry {
			log.Info("retry-rollback annotation found, retrying rollback")
			r.event(txn, corev1.EventTypeNormal, "RetryRollback", "retry-rollback annotation found, transitioning to RollingBack")
			delete(txn.Annotations, annotationRetryRollback)
			if err := r.Update(ctx, txn); err != nil {
				return ctrl.Result{}, err
			}
			return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRollingBack)
		}
		_, hasAuto := txn.Annotations[annotationAutoRollback]
		if !hasAuto {
			// No auto-retry, no manual retry → PROTECTED state.
			log.Info("no rollback annotations present, entering protected state")
			r.event(txn, corev1.EventTypeWarning, "ProtectedState",
				"transaction has unrolled commits but no rollback annotation; remove %s finalizer to force-delete",
				rollbackProtectionFinalizer)
			return r.cleanupAndReturn(ctx, txn)
		}
		// automatic-rollback present on a Failed transaction during deletion → retry.
		log.Info("automatic-rollback present, retrying rollback")
		return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRollingBack)

	}

	// Any other phase with unrolled commits (Committing, Preparing, etc.)
	_, hasAuto := txn.Annotations[annotationAutoRollback]
	if hasAuto {
		log.Info("deletion with unrolled commits and automatic-rollback, initiating rollback",
			"phase", txn.Status.Phase)
		r.event(txn, corev1.EventTypeWarning, "DeletionRollback",
			"transaction deleted with unrolled commits in phase %s, initiating rollback", txn.Status.Phase)
		return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRollingBack)
	}

	// No auto-rollback → go straight to PROTECTED.
	log.Info("deletion with unrolled commits but no automatic-rollback, entering protected state",
		"phase", txn.Status.Phase)
	r.event(txn, corev1.EventTypeWarning, "ProtectedState",
		"transaction has unrolled commits but automatic-rollback absent; remove %s finalizer to force-delete",
		rollbackProtectionFinalizer)
	return r.cleanupAndReturn(ctx, txn)
}

// handlePending initializes the transaction status and creates the rollback ConfigMap.
func (r *TransactionReconciler) handlePending(ctx context.Context, txn *backupv1alpha1.Transaction) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	changes, err := r.listChanges(ctx, txn)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing ResourceChanges: %w", err)
	}
	if len(changes) == 0 {
		return ctrl.Result{}, r.setFailed(ctx, txn, "no ResourceChange CRs found for this transaction")
	}

	log.Info("initializing transaction", "changes", len(changes))
	r.event(txn, corev1.EventTypeNormal, "Initializing", "starting transaction with %d changes", len(changes))

	// Add automatic-rollback annotation at seal time so deletion triggers rollback by default.
	if txn.Annotations == nil {
		txn.Annotations = make(map[string]string)
	}
	if _, ok := txn.Annotations[annotationAutoRollback]; !ok {
		txn.Annotations[annotationAutoRollback] = "true"
		if err := r.Update(ctx, txn); err != nil {
			return ctrl.Result{}, err
		}
	}

	wasNew := txn.Status.StartedAt == nil
	now := metav1.Now()
	txn.Status.StartedAt = &now
	txn.Status.Items = make([]backupv1alpha1.ItemStatus, len(changes))
	for i, rc := range changes {
		txn.Status.Items[i] = backupv1alpha1.ItemStatus{Name: rc.Name}
	}
	txn.Status.RollbackRef = txn.Name + rollbackCMSuffix
	if wasNew {
		txmetrics.ItemCount.Observe(float64(len(changes)))
	}

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
				APIVersion:         backupv1alpha1.GroupVersion.String(),
				Kind:               "Transaction",
				Name:               txn.Name,
				UID:                txn.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Data: map[string]string{},
	}

	// Populate _meta so the CLI can operate without the Transaction CR.
	metaChanges := make([]rollback.MetaChange, len(changes))
	for i, rc := range changes {
		ns := r.resolveNamespace(rc.Spec.Target, txn.Namespace)
		metaChanges[i] = rollback.MetaChange{
			Name:        rc.Name,
			Target:      rollback.MetaTarget{APIVersion: rc.Spec.Target.APIVersion, Kind: rc.Spec.Target.Kind, Name: rc.Spec.Target.Name, Namespace: ns},
			ChangeType:  string(rc.Spec.Type),
			RollbackKey: rollback.Key(rc.Spec.Target.Kind, ns, rc.Spec.Target.Name),
		}
	}
	meta := rollback.Meta{
		Version:              2,
		TransactionName:      txn.Name,
		TransactionNamespace: txn.Namespace,
		Changes:              metaChanges,
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return ctrl.Result{}, r.setFailed(ctx, txn, fmt.Sprintf("serializing rollback metadata: %v", err))
	}
	cm.Data[rollback.MetaKey] = string(metaJSON)

	if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, r.setFailed(ctx, txn, fmt.Sprintf("creating rollback ConfigMap: %v", err))
	}

	return r.transition(ctx, txn, backupv1alpha1.TransactionPhasePreparing)
}

// handlePreparing acquires locks and records prior state for each resource, one per reconcile.
func (r *TransactionReconciler) handlePreparing(ctx context.Context, txn *backupv1alpha1.Transaction, userClient client.Client) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	timeout := r.lockTimeout(txn)

	changes, err := r.listChanges(ctx, txn)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing ResourceChanges: %w", err)
	}

	for _, rc := range changes {
		item := findItemStatus(txn.Status.Items, rc.Name)
		if item == nil {
			return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("no status entry for ResourceChange %q", rc.Name))
		}
		if item.Prepared {
			continue
		}

		change := rc.Spec
		ns := r.resolveNamespace(change.Target, txn.Namespace)
		key := lock.ResourceKey{Namespace: ns, Kind: change.Target.Kind, Name: change.Target.Name}

		// Acquire lock.
		leaseName, err := r.LockMgr.Acquire(ctx, key, txn.Name, timeout)
		if err != nil {
			txmetrics.LockOperations.WithLabelValues("acquire", "error").Inc()
			log.Error(err, "lock acquisition failed", "resourcechange", rc.Name, "resource", key)
			r.event(txn, corev1.EventTypeWarning, "LockFailed", "%s: lock acquisition failed for %s/%s: %v", rc.Name, change.Target.Kind, change.Target.Name, err)
			txmetrics.ItemOperations.WithLabelValues("prepare", "error").Inc()
			return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("%s lock failed: %v", rc.Name, err))
		}
		txmetrics.LockOperations.WithLabelValues("acquire", "success").Inc()
		item.LockLease = leaseName
		item.LeaseNamespace = ns

		// Read current state and store in rollback ConfigMap.
		if change.Type == backupv1alpha1.ChangeTypeCreate {
			// Create uses SSA — resource may or may not exist.
			obj, err := r.getResource(ctx, userClient, change.Target, ns)
			if err != nil && !apierrors.IsNotFound(err) {
				txmetrics.ItemOperations.WithLabelValues("prepare", "error").Inc()
				return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("%s: reading current state: %v", rc.Name, err))
			}
			if apierrors.IsNotFound(err) {
				// Resource doesn't exist — rollback will delete.
				if err := r.saveRollbackState(ctx, txn, change, nil); err != nil {
					txmetrics.ItemOperations.WithLabelValues("prepare", "error").Inc()
					return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("%s: saving rollback state: %v", rc.Name, err))
				}
			} else {
				// Resource exists — save prior state so rollback restores it.
				item.ResourceVersion = obj.GetResourceVersion()
				if err := r.saveRollbackState(ctx, txn, change, obj); err != nil {
					txmetrics.ItemOperations.WithLabelValues("prepare", "error").Inc()
					return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("%s: saving rollback state: %v", rc.Name, err))
				}
			}
		} else {
			obj, err := r.getResource(ctx, userClient, change.Target, ns)
			if err != nil {
				txmetrics.ItemOperations.WithLabelValues("prepare", "error").Inc()
				return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("%s: reading current state: %v", rc.Name, err))
			}
			item.ResourceVersion = obj.GetResourceVersion()
			if err := r.saveRollbackState(ctx, txn, change, obj); err != nil {
				txmetrics.ItemOperations.WithLabelValues("prepare", "error").Inc()
				return ctrl.Result{}, r.failAndReleaseLocks(ctx, txn, fmt.Sprintf("%s: saving rollback state: %v", rc.Name, err))
			}
		}

		txmetrics.ItemOperations.WithLabelValues("prepare", "success").Inc()
		item.Prepared = true
		log.Info("item prepared", "resourcechange", rc.Name, "kind", change.Target.Kind, "name", change.Target.Name)

		// Persist progress and requeue — one item per reconcile cycle.
		return r.updateStatusAndRequeue(ctx, txn)
	}

	// All items prepared.
	return r.transition(ctx, txn, backupv1alpha1.TransactionPhasePrepared)
}

// handleCommitting applies each mutation, one per reconcile.
func (r *TransactionReconciler) handleCommitting(ctx context.Context, txn *backupv1alpha1.Transaction, userClient client.Client) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	changes, err := r.listChanges(ctx, txn)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing ResourceChanges: %w", err)
	}

	for _, rc := range changes {
		item := findItemStatus(txn.Status.Items, rc.Name)
		if item == nil {
			return ctrl.Result{}, r.setFailed(ctx, txn, fmt.Sprintf("no status entry for ResourceChange %q", rc.Name))
		}
		if item.Committed {
			continue
		}

		change := rc.Spec

		// Renew the lock to prevent expiry during long commit phases.
		timeout := r.lockTimeout(txn)
		leaseRef := lock.LeaseRef{Name: item.LockLease, Namespace: item.LeaseNamespace}
		if err := r.LockMgr.Renew(ctx, leaseRef, txn.Name, timeout); err != nil {
			txmetrics.LockOperations.WithLabelValues("renew", "error").Inc()
			log.Error(err, "lock renewal failed, initiating rollback", "resourcechange", rc.Name)
			r.event(txn, corev1.EventTypeWarning, "LockRenewalFailed", "%s: lock renewal failed, initiating rollback: %v", rc.Name, err)
			return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRollingBack)
		}
		txmetrics.LockOperations.WithLabelValues("renew", "success").Inc()

		ns := r.resolveNamespace(change.Target, txn.Namespace)

		// For Create: if the resource didn't exist at prepare time,
		// verify it still doesn't before applying.
		if change.Type == backupv1alpha1.ChangeTypeCreate && item.ResourceVersion == "" {
			_, err := r.getResource(ctx, userClient, change.Target, ns)
			if err == nil {
				// Resource was created externally between prepare and commit.
				txmetrics.ItemOperations.WithLabelValues("commit", "conflict").Inc()
				log.Error(nil, "conflict detected: resource created externally", "resourcechange", rc.Name)
				r.event(txn, corev1.EventTypeWarning, "ConflictDetected",
					"%s: %s/%s was created externally since prepare", rc.Name, change.Target.Kind, change.Target.Name)
				return ctrl.Result{}, r.setFailed(ctx, txn,
					(&ErrConflictDetected{Ref: change.Target}).Error())
			}
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}

		// Conflict detection: only needed for Update/Delete (RV-based).
		// SSA Create/Patch are idempotent — field ownership is the conflict mechanism.
		selfWrite := false
		if change.Type == backupv1alpha1.ChangeTypeUpdate || change.Type == backupv1alpha1.ChangeTypeDelete {
			committedRV := r.readCommittedRV(ctx, txn, change, ns)
			if err := r.checkConflict(ctx, userClient, change, ns, item.ResourceVersion, committedRV); err != nil {
				if errors.Is(err, errSelfWrite) {
					// Crash-retry: Janus already committed this item.
					// Skip the apply — the resource already reflects our write.
					selfWrite = true
					log.Info("self-write detected (crash-retry), skipping apply", "resourcechange", rc.Name)
				} else {
					txmetrics.ItemOperations.WithLabelValues("commit", "conflict").Inc()
					log.Error(err, "conflict detected, failing transaction", "resourcechange", rc.Name)
					r.event(txn, corev1.EventTypeWarning, "ConflictDetected",
						"%s: %s/%s was modified externally since prepare", rc.Name, change.Target.Kind, change.Target.Name)
					return ctrl.Result{}, r.setFailed(ctx, txn, err.Error())
				}
			}
		}

		if !selfWrite {
			if err := r.applyChange(ctx, userClient, change, ns, txn.Name, item.ResourceVersion); err != nil {
				var conflictErr *ErrConflictDetected
				if errors.As(err, &conflictErr) {
					txmetrics.ItemOperations.WithLabelValues("commit", "conflict").Inc()
					log.Error(err, "conflict detected during apply, failing transaction", "resourcechange", rc.Name)
					r.event(txn, corev1.EventTypeWarning, "ConflictDetected",
						"%s: %s/%s was modified externally", rc.Name, change.Target.Kind, change.Target.Name)
					return ctrl.Result{}, r.setFailed(ctx, txn, err.Error())
				}
				txmetrics.ItemOperations.WithLabelValues("commit", "error").Inc()
				item.Error = err.Error()
				log.Error(err, "commit failed, initiating rollback", "resourcechange", rc.Name)
				r.event(txn, corev1.EventTypeWarning, "CommitFailed", "%s: %s %s/%s failed: %v", rc.Name, change.Type, change.Target.Kind, change.Target.Name, err)
				return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRollingBack)
			}

			// Update rollback envelope with post-commit RV so the rollback conflict
			// check compares against the RV Janus wrote, not the pre-commit RV.
			if change.Type != backupv1alpha1.ChangeTypeDelete {
				if err := r.updateRollbackRV(ctx, txn, change, ns, userClient); err != nil {
					log.Error(err, "failed to update rollback RV after commit", "resourcechange", rc.Name)
					// Non-fatal: worst case rollback will detect a false conflict.
				}
			}
		}

		txmetrics.ItemOperations.WithLabelValues("commit", "success").Inc()
		item.Committed = true

		log.Info("item committed", "resourcechange", rc.Name, "type", change.Type, "kind", change.Target.Kind, "name", change.Target.Name)
		r.event(txn, corev1.EventTypeNormal, "ItemCommitted", "%s: %s %s/%s committed", rc.Name, change.Type, change.Target.Kind, change.Target.Name)

		return r.updateStatusAndRequeue(ctx, txn)
	}

	// All committed — release locks.
	// Rollback ConfigMap is preserved for request-rollback; OwnerRef GC handles cleanup.
	log.Info("all items committed, releasing locks")
	r.releaseAllLocks(ctx, txn)

	now := metav1.Now()
	txn.Status.CompletedAt = &now
	if txn.Status.StartedAt != nil {
		txmetrics.Duration.WithLabelValues("Committed").Observe(time.Since(txn.Status.StartedAt.Time).Seconds())
	}
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

	changes, err := r.listChanges(ctx, txn)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing ResourceChanges: %w", err)
	}

	// Iterate in reverse — rollback committed, not-yet-rolled-back items.
	for i := len(changes) - 1; i >= 0; i-- {
		rc := changes[i]
		item := findItemStatus(txn.Status.Items, rc.Name)
		if item == nil {
			continue
		}
		if !item.Committed || item.RolledBack {
			continue
		}
		// Skip items already marked as conflict from a previous reconcile cycle.
		if strings.Contains(item.Error, "rollback conflict") {
			continue
		}

		change := rc.Spec
		ns := r.resolveNamespace(change.Target, txn.Namespace)

		if err := r.applyRollback(ctx, userClient, change, ns, rbCM, txn.Name); err != nil {
			var conflictErr *ErrRollbackConflict
			if errors.As(err, &conflictErr) {
				// Conflict: record on item, skip, continue to next item.
				txmetrics.ItemOperations.WithLabelValues("rollback", "conflict").Inc()
				item.Error = err.Error()
				log.Info("rollback conflict detected, skipping item", "resourcechange", rc.Name,
					"kind", change.Target.Kind, "name", change.Target.Name)
				r.event(txn, corev1.EventTypeWarning, "RollbackConflict",
					"%s: %s — manual intervention required via janus recover", rc.Name, err)
				// Requeue to continue with remaining items.
				return r.updateStatusAndRequeue(ctx, txn)
			}
			// Non-conflict error: transient, retry.
			txmetrics.ItemOperations.WithLabelValues("rollback", "error").Inc()
			item.Error = fmt.Sprintf("rollback failed: %v", err)
			log.Error(err, "rollback failed for item, will retry", "resourcechange", rc.Name)
			r.event(txn, corev1.EventTypeWarning, "RollbackFailed",
				"%s: rollback failed for %s/%s, will retry: %v",
				rc.Name, change.Target.Kind, change.Target.Name, err)
			if statusErr := r.Status().Update(ctx, txn); statusErr != nil {
				log.Error(statusErr, "failed to persist rollback error on item status", "resourcechange", rc.Name)
			}
			return ctrl.Result{}, err
		}

		txmetrics.ItemOperations.WithLabelValues("rollback", "success").Inc()
		item.Error = "" // clear stale error from a previous failed attempt
		item.RolledBack = true
		log.Info("item rolled back", "resourcechange", rc.Name, "kind", change.Target.Kind, "name", change.Target.Name)
		r.event(txn, corev1.EventTypeNormal, "ItemRolledBack", "%s: %s/%s rolled back", rc.Name, change.Target.Kind, change.Target.Name)

		return r.updateStatusAndRequeue(ctx, txn)
	}

	// All items processed. Check for unresolved conflicts.
	hasConflicts := false
	for i := range txn.Status.Items {
		if txn.Status.Items[i].Committed && !txn.Status.Items[i].RolledBack {
			hasConflicts = true
			break
		}
	}

	// Release locks regardless of conflict outcome.
	log.Info("rollback loop complete, releasing locks")
	r.releaseAllLocks(ctx, txn)

	// rollbackProtectionFinalizer is user-managed; the controller never removes it.
	// Only strip the lease-cleanup finalizer here.
	if !hasConflicts {
		if controllerutil.RemoveFinalizer(txn, finalizerName) {
			if err := r.Update(ctx, txn); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	now := metav1.Now()
	txn.Status.CompletedAt = &now

	if hasConflicts {
		log.Info("rollback completed with conflicts, marking failed")
		r.event(txn, corev1.EventTypeWarning, "RollbackPartial",
			"rollback completed with conflicts; manual intervention required via janus recover")
		if txn.Status.StartedAt != nil {
			txmetrics.Duration.WithLabelValues("Failed").Observe(time.Since(txn.Status.StartedAt.Time).Seconds())
		}
		return ctrl.Result{}, r.setFailed(ctx, txn,
			"rollback completed with conflicts; use janus recover to resolve remaining items")
	}

	log.Info("rollback complete")
	if txn.Status.StartedAt != nil {
		txmetrics.Duration.WithLabelValues("RolledBack").Observe(time.Since(txn.Status.StartedAt.Time).Seconds())
	}

	return r.transition(ctx, txn, backupv1alpha1.TransactionPhaseRolledBack)
}

// checkConflict verifies that a target resource has not been modified since prepare.
// committedRV, if non-empty, is the post-commit RV from a previous (crashed) attempt;
// a match indicates a self-write rather than an external modification.
func (r *TransactionReconciler) checkConflict(ctx context.Context, cl client.Client,
	change backupv1alpha1.ResourceChangeSpec, ns string, expectedRV, committedRV string) error {

	if expectedRV == "" {
		return nil
	}

	obj, err := r.getResource(ctx, cl, change.Target, ns)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Delete intent is satisfied by absence — no conflict.
			if change.Type == backupv1alpha1.ChangeTypeDelete {
				return errSelfWrite
			}
			return &ErrConflictDetected{Ref: change.Target, Expected: expectedRV}
		}
		return err
	}
	currentRV := obj.GetResourceVersion()
	if currentRV != expectedRV {
		// Check if this is our own previous write (crash-retry).
		if committedRV != "" && currentRV == committedRV {
			return errSelfWrite
		}
		return &ErrConflictDetected{
			Ref:      change.Target,
			Expected: expectedRV,
			Actual:   currentRV,
		}
	}
	return nil
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

// applyChange dispatches the appropriate mutation for a ResourceChangeSpec.
// txnName is used as the field manager identity for server-side apply patches.
// storedRV is the resourceVersion captured during prepare; it enables native
// Kubernetes conflict detection for Update (optimistic concurrency) and Delete
// (precondition) operations.
func (r *TransactionReconciler) applyChange(ctx context.Context, cl client.Client, change backupv1alpha1.ResourceChangeSpec, namespace, txnName, storedRV string) error {
	switch change.Type {
	case backupv1alpha1.ChangeTypeCreate, backupv1alpha1.ChangeTypePatch:
		// Server-side apply: creates or merges owned fields (idempotent).
		obj, err := r.unmarshalContent(change.Content, change.Target)
		if err != nil {
			return err
		}
		return r.ssaApply(ctx, cl, obj, change.Target, namespace, txnName)

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

	case backupv1alpha1.ChangeTypeDelete:
		existing, err := r.getResource(ctx, cl, change.Target, namespace)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil // Already gone.
			}
			return &ResourceOpError{Op: "fetching for delete", Err: err}
		}
		deleteOpts := &client.DeleteOptions{}
		if storedRV != "" {
			deleteOpts.Preconditions = &metav1.Preconditions{
				ResourceVersion: &storedRV,
			}
		}
		if err := cl.Delete(ctx, existing, deleteOpts); err != nil {
			if apierrors.IsConflict(err) {
				return &ErrConflictDetected{Ref: change.Target, Expected: storedRV}
			}
			return err
		}
		return nil

	default:
		return fmt.Errorf("%w: %s", errUnknownChangeType, change.Type)
	}
}

// applyRollback reverses a committed change using the stored prior state.
func (r *TransactionReconciler) applyRollback(ctx context.Context, cl client.Client, change backupv1alpha1.ResourceChangeSpec, namespace string, rbCM *corev1.ConfigMap, txnName string) error {
	rbKey := rollback.Key(change.Target.Kind, namespace, change.Target.Name)

	switch change.Type {
	case backupv1alpha1.ChangeTypeCreate:
		// If the resource didn't exist before, reverse = delete.
		// If it existed, reverse = SSA-apply prior values (like Patch rollback).
		obj, storedRV, err := loadRollbackState(rbCM, rbKey, namespace)
		if err != nil {
			return err
		}
		if obj == nil {
			// Fresh create — delete to reverse.
			existing, err := r.getResource(ctx, cl, change.Target, namespace)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}
			// Check for external modifications since commit.
			if storedRV != "" && existing.GetResourceVersion() != storedRV {
				return &ErrRollbackConflict{
					Ref:       change.Target,
					StoredRV:  storedRV,
					CurrentRV: existing.GetResourceVersion(),
				}
			}
			return cl.Delete(ctx, existing)
		}
		// Resource existed before — restore prior field values via SSA.
		if err := r.checkRollbackRV(ctx, cl, change.Target, namespace, storedRV); err != nil {
			return err
		}
		var contentMap map[string]any
		if err := json.Unmarshal(change.Content.Raw, &contentMap); err != nil {
			return &ResourceOpError{Op: "unmarshaling create content for reverse", Err: err}
		}
		reversePatch := computeReversePatch(contentMap, obj.Object)
		patchObj := &unstructured.Unstructured{Object: reversePatch}
		return r.ssaApply(ctx, cl, patchObj, change.Target, namespace, txnName)

	case backupv1alpha1.ChangeTypeDelete:
		// Reverse of Delete = re-Create from rollback state.
		obj, _, err := loadRollbackState(rbCM, rbKey, namespace)
		if err != nil {
			return err
		}
		if err := cl.Create(ctx, obj); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil // Already restored by a previous attempt.
			}
			return err
		}
		return nil

	case backupv1alpha1.ChangeTypePatch:
		// Reverse of Patch = SSA-apply prior values for the patched fields.
		obj, storedRV, err := loadRollbackState(rbCM, rbKey, namespace)
		if err != nil {
			return err
		}

		// Check for external modifications since commit.
		if err := r.checkRollbackRV(ctx, cl, change.Target, namespace, storedRV); err != nil {
			return err
		}

		// Parse the forward patch content to know which fields were touched.
		var contentMap map[string]any
		if err := json.Unmarshal(change.Content.Raw, &contentMap); err != nil {
			return &ResourceOpError{Op: "unmarshaling patch content for reverse", Err: err}
		}

		// Compute reverse: same field paths, prior values.
		reversePatch := computeReversePatch(contentMap, obj.Object)
		patchObj := &unstructured.Unstructured{Object: reversePatch}
		return r.ssaApply(ctx, cl, patchObj, change.Target, namespace, txnName)

	case backupv1alpha1.ChangeTypeUpdate:
		// Reverse of Update = restore the full previous state.
		obj, storedRV, err := loadRollbackState(rbCM, rbKey, namespace)
		if err != nil {
			return err
		}
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
		// Check for external modifications since prepare.
		if storedRV != "" && existing.GetResourceVersion() != storedRV {
			return &ErrRollbackConflict{
				Ref:       change.Target,
				StoredRV:  storedRV,
				CurrentRV: existing.GetResourceVersion(),
			}
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		return cl.Update(ctx, obj)

	default:
		return fmt.Errorf("%w for rollback: %s", errUnknownChangeType, change.Type)
	}
}

// ssaApply performs a server-side apply for the given unstructured object,
// setting the required identity fields (GVK, name, namespace) and using
// "janus-<txnName>" as the field manager.
func (r *TransactionReconciler) ssaApply(ctx context.Context, cl client.Client,
	obj *unstructured.Unstructured, ref backupv1alpha1.ResourceRef,
	namespace, txnName string) error {

	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return &ResourceOpError{Op: "parsing apiVersion for apply", Ref: ref.APIVersion, Err: err}
	}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: gv.Group, Version: gv.Version, Kind: ref.Kind,
	})
	obj.SetName(ref.Name)
	obj.SetNamespace(namespace)
	ac := client.ApplyConfigurationFromUnstructured(obj)
	return cl.Apply(ctx, ac, client.FieldOwner("janus-"+txnName), client.ForceOwnership)
}

// checkRollbackRV fetches the target resource and compares its current
// resourceVersion against storedRV. Returns ErrRollbackConflict if the
// resource is gone or the RV differs; returns nil if storedRV is empty
// (nothing to check) or the RV matches.
func (r *TransactionReconciler) checkRollbackRV(ctx context.Context, cl client.Client,
	ref backupv1alpha1.ResourceRef, namespace, storedRV string) error {

	if storedRV == "" {
		return nil
	}
	existing, err := r.getResource(ctx, cl, ref, namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &ErrRollbackConflict{Ref: ref, StoredRV: storedRV}
		}
		return err
	}
	if existing.GetResourceVersion() != storedRV {
		return &ErrRollbackConflict{
			Ref:       ref,
			StoredRV:  storedRV,
			CurrentRV: existing.GetResourceVersion(),
		}
	}
	return nil
}

// updateRollbackRV reads the current resourceVersion of a committed resource
// and updates the rollback envelope so the RV reflects the post-commit state.
// This lets the rollback conflict check distinguish Janus's own writes from
// external modifications.
func (r *TransactionReconciler) updateRollbackRV(ctx context.Context, txn *backupv1alpha1.Transaction, change backupv1alpha1.ResourceChangeSpec, namespace string, cl client.Client) error {
	obj, err := r.getResource(ctx, cl, change.Target, namespace)
	if err != nil {
		return err
	}

	rbKey := rollback.Key(change.Target.Kind, namespace, change.Target.Name)
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: txn.Status.RollbackRef, Namespace: txn.Namespace}, cm); err != nil {
		return err
	}

	raw, ok := cm.Data[rbKey]
	if !ok {
		return nil // No envelope to update.
	}

	var env rollback.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return err
	}
	env.ResourceVersion = obj.GetResourceVersion()

	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	cm.Data[rbKey] = string(data)
	return r.Update(ctx, cm)
}

// readCommittedRV reads the post-commit resourceVersion from the rollback
// ConfigMap for the given resource. Returns "" if no RV is stored or if the
// ConfigMap cannot be read (best-effort for self-write detection).
func (r *TransactionReconciler) readCommittedRV(ctx context.Context, txn *backupv1alpha1.Transaction, change backupv1alpha1.ResourceChangeSpec, ns string) string {
	rbKey := rollback.Key(change.Target.Kind, ns, change.Target.Name)
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Name: txn.Status.RollbackRef, Namespace: txn.Namespace}, cm); err != nil {
		return ""
	}
	raw, ok := cm.Data[rbKey]
	if !ok {
		return ""
	}
	var env rollback.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return ""
	}
	return env.ResourceVersion
}

// --- Helpers ---

func (r *TransactionReconciler) unmarshalContent(raw runtime.RawExtension, ref backupv1alpha1.ResourceRef) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw.Raw, &obj.Object); err != nil {
		return nil, &ResourceOpError{Op: "unmarshaling content", Ref: ref.Kind + "/" + ref.Name, Err: err}
	}
	return obj, nil
}

func (r *TransactionReconciler) saveRollbackState(ctx context.Context, txn *backupv1alpha1.Transaction, change backupv1alpha1.ResourceChangeSpec, obj *unstructured.Unstructured) error {
	ns := r.resolveNamespace(change.Target, txn.Namespace)
	key := rollback.Key(change.Target.Kind, ns, change.Target.Name)

	var priorState json.RawMessage
	var rv string
	if obj != nil {
		var err error
		priorState, err = json.Marshal(obj.Object)
		if err != nil {
			return &ResourceOpError{Op: "serializing rollback state", Err: err}
		}
		rv = obj.GetResourceVersion()
	}

	env := rollback.Envelope{
		ResourceVersion: rv,
		ChangeType:      string(change.Type),
		PriorState:      priorState,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return &ResourceOpError{Op: "serializing rollback envelope", Err: err}
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

func (r *TransactionReconciler) transactionTimeout(txn *backupv1alpha1.Transaction) time.Duration {
	if txn.Spec.Timeout != nil {
		return txn.Spec.Timeout.Duration
	}
	return defaultTransactionTimeout
}

// releaseAllLocks releases all leases for a transaction (best-effort) and records metrics.
func (r *TransactionReconciler) releaseAllLocks(ctx context.Context, txn *backupv1alpha1.Transaction) {
	log := logf.FromContext(ctx)
	refs := make([]lock.LeaseRef, 0, len(txn.Status.Items))
	for _, item := range txn.Status.Items {
		if item.LockLease != "" {
			refs = append(refs, lock.LeaseRef{Name: item.LockLease, Namespace: item.LeaseNamespace})
		}
	}
	if err := r.LockMgr.ReleaseAll(ctx, refs, txn.Name); err != nil {
		txmetrics.LockOperations.WithLabelValues("release", "error").Inc()
		log.Error(err, "best-effort lease release failed")
	} else {
		txmetrics.LockOperations.WithLabelValues("release", "success").Inc()
	}
}

func (r *TransactionReconciler) transition(ctx context.Context, txn *backupv1alpha1.Transaction, phase backupv1alpha1.TransactionPhase) (ctrl.Result, error) {
	eventType := corev1.EventTypeNormal
	if phase == backupv1alpha1.TransactionPhaseRollingBack || phase == backupv1alpha1.TransactionPhaseFailed {
		eventType = corev1.EventTypeWarning
	}
	r.event(txn, eventType, "PhaseTransition", "transitioning to %s", phase)

	oldPhase := txn.Status.Phase
	txn.Status.Phase = phase
	if c := conditionForPhase(phase); c != nil {
		apimeta.SetStatusCondition(&txn.Status.Conditions, *c)
	}
	txn.Status.Version++
	if err := r.Status().Update(ctx, txn); err != nil {
		return ctrl.Result{}, err
	}

	recordPhaseChange(oldPhase, phase)
	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

// conditionForPhase returns a status Condition for significant phase milestones, nil otherwise.
// Failed is intentionally excluded — setFailed() handles it with a custom message.
func conditionForPhase(phase backupv1alpha1.TransactionPhase) *metav1.Condition {
	type condDef struct{ Type, Reason, Message string }
	conditions := map[backupv1alpha1.TransactionPhase]condDef{
		backupv1alpha1.TransactionPhasePrepared:   {"Prepared", "AllItemsPrepared", "All items have been prepared"},
		backupv1alpha1.TransactionPhaseCommitted:  {"Committed", "AllItemsCommitted", "All items have been committed"},
		backupv1alpha1.TransactionPhaseRolledBack: {"RolledBack", "RollbackComplete", "All committed items have been rolled back"},
	}
	entry, ok := conditions[phase]
	if !ok {
		return nil
	}
	return &metav1.Condition{
		Type:               entry.Type,
		Status:             metav1.ConditionTrue,
		Reason:             entry.Reason,
		Message:            entry.Message,
		LastTransitionTime: metav1.Now(),
	}
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
	log.Info("transaction failed", "reason", message)

	oldPhase := txn.Status.Phase
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
	if err := r.Status().Update(ctx, txn); err != nil {
		return err
	}

	recordPhaseChange(oldPhase, backupv1alpha1.TransactionPhaseFailed)
	if txn.Status.StartedAt != nil {
		txmetrics.Duration.WithLabelValues("Failed").Observe(time.Since(txn.Status.StartedAt.Time).Seconds())
	}
	return nil
}

func (r *TransactionReconciler) failAndReleaseLocks(ctx context.Context, txn *backupv1alpha1.Transaction, message string) error {
	r.releaseAllLocks(ctx, txn)
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

// listChanges returns the ResourceChanges owned by the Transaction, sorted by
// (order, name) for deterministic processing.
func (r *TransactionReconciler) listChanges(ctx context.Context, txn *backupv1alpha1.Transaction) ([]backupv1alpha1.ResourceChange, error) {
	var list backupv1alpha1.ResourceChangeList
	if err := r.List(ctx, &list, client.InNamespace(txn.Namespace)); err != nil {
		return nil, err
	}

	// Filter to only those owned by this Transaction.
	var owned []backupv1alpha1.ResourceChange
	for _, rc := range list.Items {
		for _, ref := range rc.OwnerReferences {
			if ref.UID == txn.UID {
				owned = append(owned, rc)
				break
			}
		}
	}

	// Sort by (order, name).
	sort.Slice(owned, func(i, j int) bool {
		if owned[i].Spec.Order != owned[j].Spec.Order {
			return owned[i].Spec.Order < owned[j].Spec.Order
		}
		return owned[i].Name < owned[j].Name
	})
	return owned, nil
}

// findItemStatus returns the ItemStatus for the given ResourceChange name, or nil.
func findItemStatus(items []backupv1alpha1.ItemStatus, name string) *backupv1alpha1.ItemStatus {
	for i := range items {
		if items[i].Name == name {
			return &items[i]
		}
	}
	return nil
}

// loadRollbackState retrieves and deserializes a resource's prior state from the rollback ConfigMap.
// It returns the prior-state object (nil for Create envelopes), the stored resourceVersion, and any error.
func loadRollbackState(rbCM *corev1.ConfigMap, rbKey, namespace string) (*unstructured.Unstructured, string, error) {
	data, ok := rbCM.Data[rbKey]
	if !ok {
		return nil, "", &RollbackDataError{Key: rbKey}
	}

	var env rollback.Envelope
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return nil, "", &RollbackDataError{Key: rbKey, Err: err}
	}

	if env.PriorState == nil {
		return nil, env.ResourceVersion, nil
	}

	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(env.PriorState, &obj.Object); err != nil {
		return nil, "", &RollbackDataError{Key: rbKey, Err: err}
	}
	rollback.CleanForRestore(obj, namespace)
	return obj, env.ResourceVersion, nil
}

// computeReversePatch takes a forward patch and the full prior state, and
// returns a partial object that restores patched fields to their prior values.
// Fields present in the forward patch but absent from the prior state are
// omitted (they were added by the patch and should be removed on rollback).
// Non-map leaf values (including slices) are treated as atomic replacements.
func computeReversePatch(forwardPatch, priorState map[string]any) map[string]any {
	result := make(map[string]any)
	for key, newVal := range forwardPatch {
		priorVal, exists := priorState[key]
		if !exists {
			continue // Field was added by patch; omit from reverse.
		}
		newMap, newIsMap := newVal.(map[string]any)
		priorMap, priorIsMap := priorVal.(map[string]any)
		if newIsMap && priorIsMap {
			sub := computeReversePatch(newMap, priorMap)
			if len(sub) > 0 {
				result[key] = sub
			}
		} else {
			result[key] = priorVal
		}
	}
	return result
}

// recordPhaseChange updates phase-transition counter and active-transactions gauge.
func recordPhaseChange(from, to backupv1alpha1.TransactionPhase) {
	oldPhase := string(from)
	if oldPhase == "" {
		oldPhase = "Pending"
	}
	txmetrics.PhaseTransitions.WithLabelValues(oldPhase, string(to)).Inc()
	if !isTerminalPhase(from) && from != "" && from != backupv1alpha1.TransactionPhasePending {
		txmetrics.ActiveTransactions.WithLabelValues(oldPhase).Dec()
	}
	if !isTerminalPhase(to) {
		txmetrics.ActiveTransactions.WithLabelValues(string(to)).Inc()
	}
}

func isTerminalPhase(p backupv1alpha1.TransactionPhase) bool {
	switch p {
	case backupv1alpha1.TransactionPhaseCommitted,
		backupv1alpha1.TransactionPhaseRolledBack,
		backupv1alpha1.TransactionPhaseFailed:
		return true
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *TransactionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&backupv1alpha1.Transaction{}).
		Owns(&backupv1alpha1.ResourceChange{}).
		Named("transaction").
		Complete(r)
}
