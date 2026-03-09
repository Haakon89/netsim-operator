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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.
type Traffic struct {
	Steps []TrafficStep `json:"steps,omitempty"`
}

type TrafficStep struct {
	Type  string `json:"type"`
	From  string `json:"from,omitempty"`
	To    string `json:"to"`
	Count int    `json:"count,omitempty"`
}

// NetworkSimulationSpec defines the desired state of NetworkSimulation
type NetworkSimulationSpec struct {
	DeviceCount int     `json:"deviceCount"`
	DeviceImage string  `json:"deviceImage,omitempty"`
	Traffic     Traffic `json:"traffic,omitempty"`
}

// NetworkSimulationStatus defines the observed state of NetworkSimulation.
type NetworkSimulationStatus struct {
	Phase          string `json:"phase,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
	TrafficJobName string `json:"trafficJobName"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// NetworkSimulation is the Schema for the networksimulations API
type NetworkSimulation struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of NetworkSimulation
	// +required
	Spec NetworkSimulationSpec `json:"spec,omitempty"`

	// status defines the observed state of NetworkSimulation
	// +optional
	Status NetworkSimulationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NetworkSimulationList contains a list of NetworkSimulation
type NetworkSimulationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NetworkSimulation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NetworkSimulation{}, &NetworkSimulationList{})
}
