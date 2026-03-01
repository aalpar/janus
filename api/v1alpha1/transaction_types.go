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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ChangeType defines the type of mutation to apply.
// +kubebuilder:validation:Enum=Create;Update;Patch;Delete
type ChangeType string

const (
	ChangeTypeCreate ChangeType = "Create"
	ChangeTypeUpdate ChangeType = "Update"
	ChangeTypePatch  ChangeType = "Patch"
	ChangeTypeDelete ChangeType = "Delete"
)

// TransactionPhase represents the current state of a Transaction.
type TransactionPhase string

const (
	TransactionPhasePending     TransactionPhase = "Pending"
	TransactionPhasePreparing   TransactionPhase = "Preparing"
	TransactionPhasePrepared    TransactionPhase = "Prepared"
	TransactionPhaseCommitting  TransactionPhase = "Committing"
	TransactionPhaseCommitted   TransactionPhase = "Committed"
	TransactionPhaseRollingBack TransactionPhase = "RollingBack"
	TransactionPhaseRolledBack  TransactionPhase = "RolledBack"
	TransactionPhaseFailed      TransactionPhase = "Failed"
)

// ResourceRef identifies a specific Kubernetes resource.
type ResourceRef struct {
	// APIVersion is the group/version of the target resource (e.g. "v1", "apps/v1").
	APIVersion string `json:"apiVersion"`
	// Kind is the resource kind (e.g. "ConfigMap", "Deployment").
	Kind string `json:"kind"`
	// Name of the target resource.
	Name string `json:"name"`
	// Namespace of the target resource. Defaults to the Transaction's namespace if empty.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ResourceChangeSpec describes a single mutation within a transaction.
type ResourceChangeSpec struct {
	// Target identifies the resource to mutate.
	Target ResourceRef `json:"target"`
	// Type is the kind of mutation: Create, Update, Patch, or Delete.
	Type ChangeType `json:"type"`
	// Content holds the resource manifest or patch data.
	// Required for Create, Update, and Patch. Ignored for Delete.
	// +optional
	Content runtime.RawExtension `json:"content,omitempty"`
	// Order controls execution sequence. Changes with the same order value
	// are independent and may execute in parallel in future versions.
	// Changes with lower order values execute first.
	// +kubebuilder:default=0
	// +optional
	Order int32 `json:"order,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.kind`
// +kubebuilder:printcolumn:name="Order",type=integer,JSONPath=`.spec.order`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ResourceChange represents a single resource mutation belonging to a Transaction.
type ResourceChange struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ResourceChangeSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ResourceChangeList contains a list of ResourceChanges.
type ResourceChangeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceChange `json:"items"`
}

// Annotation keys for Transaction behavior.
const (
	// AnnotationAutoRollback controls automatic rollback retry on deletion.
	// Present by default (added at seal time). Remove to stop retrying.
	AnnotationAutoRollback = "tx.janus.io/automatic-rollback"

	// AnnotationRetryRollback is a one-shot trigger for manual rollback retry.
	// User adds it; controller removes it after the attempt.
	AnnotationRetryRollback = "tx.janus.io/retry-rollback"

	// AnnotationRequestRollback is a one-shot trigger to roll back a Committed
	// transaction. User adds it; controller removes it and transitions to
	// RollingBack. Ignored on non-Committed transactions.
	AnnotationRequestRollback = "tx.janus.io/request-rollback"
)

// TransactionSpec defines the desired state of a Transaction.
type TransactionSpec struct {
	// ServiceAccountName is the SA whose identity is used for resource operations.
	// Must exist in the Transaction's namespace.
	// +kubebuilder:validation:MinLength=1
	ServiceAccountName string `json:"serviceAccountName"`
	// Sealed indicates the transaction is ready for processing.
	// The controller ignores unsealed transactions.
	// Once sealed, this field is immutable.
	// +optional
	Sealed bool `json:"sealed,omitempty"`
	// LockTimeout is how long acquired locks are held before expiring.
	// Defaults to 5 minutes.
	// +optional
	LockTimeout *metav1.Duration `json:"lockTimeout,omitempty"`
	// Timeout bounds the overall transaction duration.
	// When elapsed, the transaction transitions to RollingBack (if any commits
	// exist) or Failed (if none do).
	// Defaults to 30 minutes.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// ItemStatus tracks the state of a single resource change within the transaction.
type ItemStatus struct {
	// Name is the name of the ResourceChange CR this status tracks.
	Name string `json:"name"`
	// LockLease is the name of the Lease object used to lock this resource.
	// +optional
	LockLease string `json:"lockLease,omitempty"`
	// LeaseNamespace is the namespace of the Lease object.
	// +optional
	LeaseNamespace string `json:"leaseNamespace,omitempty"`
	// ResourceVersion is the target resource's version at prepare time.
	// Used to detect external modifications before commit.
	// +optional
	ResourceVersion string `json:"resourceVersion,omitempty"`
	// Prepared indicates whether the resource's prior state has been captured.
	Prepared bool `json:"prepared"`
	// Committed indicates whether the mutation has been applied.
	Committed bool `json:"committed"`
	// RolledBack indicates whether a committed change has been reverted.
	RolledBack bool `json:"rolledBack"`
	// Error records any error encountered during processing of this item.
	// +optional
	Error string `json:"error,omitempty"`
}

// TransactionStatus defines the observed state of a Transaction.
type TransactionStatus struct {
	// Phase is the current phase of the transaction state machine.
	// +optional
	Phase TransactionPhase `json:"phase,omitempty"`
	// Version is incremented on each status update to detect stale writes.
	Version int64 `json:"version"`
	// Items tracks the per-resource state for each ResourceChange.
	// +optional
	// +listType=map
	// +listMapKey=name
	Items []ItemStatus `json:"items,omitempty"`
	// RollbackRef is the name of the ConfigMap storing rollback data.
	// +optional
	RollbackRef string `json:"rollbackRef,omitempty"`
	// StartedAt is when the transaction began processing.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the transaction reached a terminal state.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Sealed",type=boolean,JSONPath=`.spec.sealed`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Transaction represents an atomic set of Kubernetes resource mutations
// executed with 2-phase commit semantics and advisory Lease-based locking.
type Transaction struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TransactionSpec   `json:"spec,omitempty"`
	Status TransactionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TransactionList contains a list of Transactions.
type TransactionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Transaction `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Transaction{}, &TransactionList{})
	SchemeBuilder.Register(&ResourceChange{}, &ResourceChangeList{})
}
