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

// BackupPhase represents the current phase of a backup operation.
// +kubebuilder:validation:Enum=Pending;Quiescing;Snapshotting;Resuming;Completed;Failed
type BackupPhase string

const (
	BackupPhasePending      BackupPhase = "Pending"
	BackupPhaseQuiescing    BackupPhase = "Quiescing"
	BackupPhaseSnapshotting BackupPhase = "Snapshotting"
	BackupPhaseResuming     BackupPhase = "Resuming"
	BackupPhaseCompleted    BackupPhase = "Completed"
	BackupPhaseFailed       BackupPhase = "Failed"
)

// BackupSpec defines a request to back up a target resource.
type BackupSpec struct {
	// contractRef is the name of the BackupContract that defines backup semantics.
	// +required
	ContractRef string `json:"contractRef"`

	// targetName is the name of the target resource to back up.
	// +required
	TargetName string `json:"targetName"`

	// targetNamespace is the namespace of the target resource.
	// If not specified, defaults to the Backup's namespace.
	// +optional
	TargetNamespace string `json:"targetNamespace,omitempty"`
}

// BackupArtifact records a single resource or volume snapshot captured during backup.
type BackupArtifact struct {
	// apiGroup is the API group of the captured resource.
	// +optional
	APIGroup string `json:"apiGroup,omitempty"`

	// kind is the kind of the captured resource.
	// +required
	Kind string `json:"kind"`

	// name is the name of the captured resource.
	// +required
	Name string `json:"name"`

	// namespace is the namespace of the captured resource.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// dataRef is a reference to where the resource data is stored.
	// For Kubernetes resources, this is the name of the ConfigMap holding the serialized YAML.
	// For PVCs, this is the name of the VolumeSnapshot.
	// +required
	DataRef string `json:"dataRef"`
}

// BackupStatus defines the observed state of a Backup.
type BackupStatus struct {
	// phase is the current phase of the backup operation.
	// +optional
	Phase BackupPhase `json:"phase,omitempty"`

	// startedAt is the time the backup operation started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// completedAt is the time the backup operation completed.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// artifacts lists the resources and volume snapshots captured during backup.
	// +optional
	Artifacts []BackupArtifact `json:"artifacts,omitempty"`

	// message provides human-readable details about the current phase or failure.
	// +optional
	Message string `json:"message,omitempty"`

	// conditions represent the current state of the Backup.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Backup is a request to create a consistent backup of a target resource
// using the semantics defined in a BackupContract.
type Backup struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec BackupSpec `json:"spec"`

	// +optional
	Status BackupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupList contains a list of Backup.
type BackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Backup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Backup{}, &BackupList{})
}
