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

// WarmPoolSpec defines the desired set of workers built from one SandboxTemplate
// (TDD §3.2). The pool provisions generic sandboxd worker pods; the sandbox image
// comes from the template at /run time, not the worker pod spec.
type WarmPoolSpec struct {
	// templateRef names the SandboxTemplate this pool's workers serve.
	// +kubebuilder:validation:Required
	TemplateRef LocalRef `json:"templateRef"`

	// replicas is the desired number of warm workers. HPA-scalable via the scale
	// subresource.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`

	// minIdle keeps at least N idle workers ready to accept a session. (Enforcement
	// of the autoscaling to maintain minIdle is deferred to a later phase; the
	// field is honored as intent.)
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinIdle int32 `json:"minIdle,omitempty"`

	// arbitraryImage marks this pool as the landing zone for arbitrary-image
	// sessions (O6c): looser warm guarantees, fed by a generic base image.
	// +optional
	ArbitraryImage bool `json:"arbitraryImage,omitempty"`
}

// WarmPoolStatus defines the observed state of WarmPool.
type WarmPoolStatus struct {
	// replicas is the number of worker pods currently owned by this pool.
	// +optional
	Replicas int32 `json:"replicas"`

	// idle is the number of workers currently holding no session.
	// +optional
	Idle int32 `json:"idle"`

	// busy is the number of workers currently holding a session.
	// +optional
	Busy int32 `json:"busy"`

	// selector is the label selector for the scale subresource / HPA.
	// +optional
	Selector string `json:"selector,omitempty"`

	// conditions represent the current state of the WarmPool resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:shortName=wp
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Idle",type=integer,JSONPath=`.status.idle`
// +kubebuilder:printcolumn:name="Busy",type=integer,JSONPath=`.status.busy`

// WarmPool is the Schema for the warmpools API
type WarmPool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of WarmPool
	// +required
	Spec WarmPoolSpec `json:"spec"`

	// status defines the observed state of WarmPool
	// +optional
	Status WarmPoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WarmPoolList contains a list of WarmPool
type WarmPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WarmPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &WarmPool{}, &WarmPoolList{})
		return nil
	})
}
