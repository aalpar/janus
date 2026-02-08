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

// TargetRef identifies the primary custom resource that the operator manages.
type TargetRef struct {
	// apiGroup is the API group of the target resource (e.g. "postgresql.cnpg.io").
	// +required
	APIGroup string `json:"apiGroup"`

	// kind is the kind of the target resource (e.g. "Cluster").
	// +required
	Kind string `json:"kind"`
}

// PatchAction describes a strategic merge patch to apply to the target resource.
type PatchAction struct {
	// merge is the strategic merge patch to apply.
	// The value is an arbitrary JSON object that will be merged into the target resource.
	// +required
	// +kubebuilder:pruning:PreserveUnknownFields
	Merge runtime.RawExtension `json:"merge"`
}

// ReadinessCheck defines how to determine that an operation has completed.
type ReadinessCheck struct {
	// condition is a CEL expression evaluated against the target resource.
	// Must evaluate to a boolean. The resource object is available as "object".
	// Examples:
	//   "object.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True')"
	//   "object.status.readyInstances == object.spec.instances"
	// +required
	Condition string `json:"condition"`

	// timeout is the maximum duration to wait for the condition to become true.
	// +required
	Timeout metav1.Duration `json:"timeout"`
}

// LifecycleAction describes an action to quiesce or resume the target resource,
// along with a readiness check to determine when the action has completed.
type LifecycleAction struct {
	// patch is a strategic merge patch to apply to the target resource.
	// +optional
	Patch *PatchAction `json:"patch,omitempty"`

	// ready defines how to determine that the action has completed.
	// +required
	Ready ReadinessCheck `json:"ready"`
}

// ResourceSelector identifies a set of Kubernetes resources related to the target.
type ResourceSelector struct {
	// apiGroup is the API group of the resource. Empty string means core API group.
	// +optional
	APIGroup string `json:"apiGroup,omitempty"`

	// kind is the kind of the resource (e.g. "Secret", "PersistentVolumeClaim").
	// +required
	Kind string `json:"kind"`

	// selector is a label selector to match resources of this kind.
	// The controller will substitute the target resource's name for empty values,
	// allowing contracts to be written generically.
	// +optional
	Selector map[string]string `json:"selector,omitempty"`
}

// RestoreStep defines a single step in the restore sequence.
type RestoreStep struct {
	// selector identifies which resources to restore in this step.
	// +required
	Selector ResourceSelector `json:"selector"`

	// ready is an optional readiness check to wait for before proceeding
	// to the next step. The check is evaluated against each restored resource
	// matching the selector.
	// +optional
	Ready *ReadinessCheck `json:"ready,omitempty"`
}

// RestorePolicy defines how to restore from a backup.
type RestorePolicy struct {
	// order defines the sequence of restore steps. Resources are restored
	// in the order specified, and each step's readiness check (if any) must
	// pass before proceeding to the next step.
	// +required
	// +kubebuilder:validation:MinItems=1
	Order []RestoreStep `json:"order"`

	// verify defines how to determine that the restore completed successfully.
	// This check is evaluated against the primary target resource after all
	// restore steps have completed.
	// +required
	Verify ReadinessCheck `json:"verify"`
}

// BackupContractSpec defines the backup and restore semantics for an operator.
// A BackupContract is authored by the operator developer and declares how to
// safely backup and restore the operator's managed state.
type BackupContractSpec struct {
	// target identifies the primary custom resource that the operator manages.
	// +required
	Target TargetRef `json:"target"`

	// quiesce defines how to put the application into a safe state for snapshotting.
	// If not specified, the controller will take a crash-consistent snapshot.
	// +optional
	Quiesce *LifecycleAction `json:"quiesce,omitempty"`

	// resume defines how to return the application to normal operation after snapshotting.
	// Required if quiesce is specified.
	// +optional
	Resume *LifecycleAction `json:"resume,omitempty"`

	// critical lists resources that must be captured in the backup.
	// These resources contain state that cannot be reconstructed by the operator.
	// +required
	// +kubebuilder:validation:MinItems=1
	Critical []ResourceSelector `json:"critical"`

	// derivable lists resources that the operator will regenerate on reconciliation.
	// These are excluded from backup to reduce snapshot size and avoid conflicts on restore.
	// +optional
	Derivable []ResourceSelector `json:"derivable,omitempty"`

	// restore defines how to restore from a backup.
	// +required
	Restore RestorePolicy `json:"restore"`
}

// BackupContractStatus defines the observed state of BackupContract.
type BackupContractStatus struct {
	// conditions represent the current state of the BackupContract.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// BackupContract declares the backup and restore semantics for an operator.
// It is authored by the operator developer and consumed by the Janus controller.
type BackupContract struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec BackupContractSpec `json:"spec"`

	// +optional
	Status BackupContractStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupContractList contains a list of BackupContract.
type BackupContractList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BackupContract `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupContract{}, &BackupContractList{})
}
