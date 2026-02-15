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

// ResourceChange describes a single mutation within a transaction.
type ResourceChange struct {
	// Target identifies the resource to mutate.
	Target ResourceRef `json:"target"`
	// Type is the kind of mutation: Create, Update, Patch, or Delete.
	Type ChangeType `json:"type"`
	// Content holds the resource manifest or patch data.
	// Required for Create, Update, and Patch. Ignored for Delete.
	// +optional
	Content runtime.RawExtension `json:"content,omitempty"`
}

// TransactionSpec defines the desired state of a Transaction.
type TransactionSpec struct {
	// Changes is the ordered list of resource mutations to apply atomically.
	// +kubebuilder:validation:MinItems=1
	Changes []ResourceChange `json:"changes"`
	// LockTimeout is how long acquired locks are held before expiring.
	// Defaults to 5 minutes.
	// +optional
	LockTimeout *metav1.Duration `json:"lockTimeout,omitempty"`
}

// ItemStatus tracks the state of a single resource change within the transaction.
type ItemStatus struct {
	// LockLease is the name of the Lease object used to lock this resource.
	// +optional
	LockLease string `json:"lockLease,omitempty"`
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
	// Items tracks the per-resource state for each change in spec.changes.
	// +optional
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
}
