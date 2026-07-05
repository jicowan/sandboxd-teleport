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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SandboxTemplateSpec is an operator-authored blueprint for what to run as a
// sandbox. Its image/cmd/env/ports/health map directly onto sandboxd's /run body
// (see TDD §3.1); idle policy maps onto the worker supervisor + /suspend|/reset.
type SandboxTemplateSpec struct {
	// image is the OCI image run AS THE SANDBOX (nested gVisor workload), not the
	// worker pod image.
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// cmd overrides the image entrypoint+cmd (optional; image default otherwise).
	// +optional
	Cmd []string `json:"cmd,omitempty"`

	// env is added to the sandbox process environment.
	// +optional
	Env []string `json:"env,omitempty"`

	// ports are exposed via the worker's DNAT (podIP:host -> interiorIP:container).
	// +optional
	Ports []PortMap `json:"ports,omitempty"`

	// health drives the worker readiness probe (and thus router health/idle).
	// +optional
	Health *Health `json:"health,omitempty"`

	// idle controls how long a sandbox may be idle before checkpoint + reclaim.
	// +optional
	Idle IdlePolicy `json:"idle,omitempty"`

	// resources is a worker sizing hint informing the WarmPool pod resources.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// SandboxTemplateStatus defines the observed state of SandboxTemplate.
type SandboxTemplateStatus struct {
	// conditions represent the current state of the SandboxTemplate resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbxt

// SandboxTemplate is the Schema for the sandboxtemplates API
type SandboxTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SandboxTemplate
	// +required
	Spec SandboxTemplateSpec `json:"spec"`

	// status defines the observed state of SandboxTemplate
	// +optional
	Status SandboxTemplateStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SandboxTemplateList contains a list of SandboxTemplate
type SandboxTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SandboxTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SandboxTemplate{}, &SandboxTemplateList{})
		return nil
	})
}
