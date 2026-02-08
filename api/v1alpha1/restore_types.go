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
)

// RestorePhase represents the current phase of a restore operation.
// +kubebuilder:validation:Enum=Pending;Restoring;Verifying;Completed;Failed
type RestorePhase string

const (
	RestorePhasePending   RestorePhase = "Pending"
	RestorePhaseRestoring RestorePhase = "Restoring"
	RestorePhaseVerifying RestorePhase = "Verifying"
	RestorePhaseCompleted RestorePhase = "Completed"
	RestorePhaseFailed    RestorePhase = "Failed"
)

// RestoreSpec defines a request to restore from a backup.
type RestoreSpec struct {
	// backupRef is the name of the Backup resource to restore from.
	// +required
	BackupRef string `json:"backupRef"`

	// targetNamespace is the namespace to restore into.
	// If not specified, defaults to the original backup namespace.
	// +optional
	TargetNamespace string `json:"targetNamespace,omitempty"`
}

// RestoreStepStatus records the result of a single restore step.
type RestoreStepStatus struct {
	// selector identifies which resources were restored in this step.
	// +required
	Selector ResourceSelector `json:"selector"`

	// resourcesRestored is the number of resources restored in this step.
	// +optional
	ResourcesRestored int `json:"resourcesRestored,omitempty"`

	// ready indicates whether the readiness check for this step passed.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// message provides human-readable details about this step.
	// +optional
	Message string `json:"message,omitempty"`
}

// RestoreStatus defines the observed state of a Restore.
type RestoreStatus struct {
	// phase is the current phase of the restore operation.
	// +optional
	Phase RestorePhase `json:"phase,omitempty"`

	// startedAt is the time the restore operation started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// completedAt is the time the restore operation completed.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// currentStep is the index of the currently executing restore step (0-based).
	// +optional
	CurrentStep int `json:"currentStep,omitempty"`

	// steps records the result of each completed restore step.
	// +optional
	Steps []RestoreStepStatus `json:"steps,omitempty"`

	// message provides human-readable details about the current phase or failure.
	// +optional
	Message string `json:"message,omitempty"`

	// conditions represent the current state of the Restore.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Backup",type=string,JSONPath=`.spec.backupRef`
// +kubebuilder:printcolumn:name="Step",type=integer,JSONPath=`.status.currentStep`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Restore is a request to restore a target resource from a Backup
// using the restore semantics defined in the associated BackupContract.
type Restore struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec RestoreSpec `json:"spec"`

	// +optional
	Status RestoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RestoreList contains a list of Restore.
type RestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Restore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Restore{}, &RestoreList{})
}
