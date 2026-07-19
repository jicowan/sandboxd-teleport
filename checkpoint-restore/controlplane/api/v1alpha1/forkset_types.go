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

// ForkSetSpec fans out N independent Session children from ONE common source
// (docs/PRD-snapshot-fork.md). The source is selected by baseRef:
//   - baseRef SET   -> fork-from-snapshot: children restore from the BaseSnapshot.
//   - baseRef UNSET -> fork-from-image:    children cold-start from pool's template
//     image (no BaseSnapshot, no pinning).
//
// A ForkSet is to forked Sessions what a WarmPool is to worker pods: the
// controller creates and owns N Session children (ownerRefs) and rolls their
// readiness up into status. It is also the single seam where a fan-out quota is
// enforced.
type ForkSetSpec struct {
	// baseRef optionally selects the BaseSnapshot to fork from (snapshot source).
	// When set, children are restored from the base's snapshot. When unset, this is
	// an image-source ForkSet and children cold-start from pool's template image.
	// +optional
	BaseRef *LocalRef `json:"baseRef,omitempty"`

	// count is the number of fork children (N) to create.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Required
	Count int32 `json:"count"`

	// namePrefix gives fork children deterministic names (sess-fork-<prefix>-<n>) so
	// a harness can address a specific fork. When empty, a generated suffix is used.
	// +optional
	NamePrefix string `json:"namePrefix,omitempty"`

	// pool places the forks and, for the image source, supplies the template image.
	// Required for the image source; for the snapshot source it must be
	// runsc-compatible with the base.
	// +kubebuilder:validation:Required
	Pool string `json:"pool"`

	// activation controls whether forks are materialized immediately (Eager -
	// resume/run all N now) or lazily on first contact (Lazy). Default Lazy.
	// +kubebuilder:validation:Enum=Eager;Lazy
	// +kubebuilder:default=Lazy
	// +optional
	Activation string `json:"activation,omitempty"`

	// lifecycle sets the per-fork idle policy (ephemeral reset vs durable suspend)
	// and retention, applied to every child Session.
	// +optional
	Lifecycle SessionLifecycle `json:"lifecycle,omitempty"`

	// subject is the owner identity for fan-out quota + attribution.
	// +optional
	Subject string `json:"subject,omitempty"`

	// iam optionally lets each fork's sandbox assume an AWS IAM role, overriding the
	// base's / template's iam.
	// +optional
	IAM *IAMSpec `json:"iam,omitempty"`
}

// Activation values.
const (
	ActivationEager = "Eager"
	ActivationLazy  = "Lazy"
)

// ForkSet phase values.
const (
	ForkSetProgressing = "Progressing"
	ForkSetReady       = "Ready"
	ForkSetFailed      = "Failed"
)

// ForkSetStatus reports the fan-out progress and the created child session ids.
type ForkSetStatus struct {
	// desired is the number of fork children requested (mirrors spec.count).
	// +optional
	Desired int32 `json:"desired"`

	// ready is the number of fork children whose Session has reached Running.
	// +optional
	Ready int32 `json:"ready"`

	// forks is the list of created child session ids — what a harness reads back to
	// address each fork.
	// +optional
	Forks []string `json:"forks,omitempty"`

	// phase is the fan-out state.
	// +kubebuilder:validation:Enum=Progressing;Ready;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// conditions represent the current state of the ForkSet resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fork
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desired`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// ForkSet is the Schema for the forksets API.
type ForkSet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired fan-out
	// +required
	Spec ForkSetSpec `json:"spec"`

	// status defines the observed fan-out state
	// +optional
	Status ForkSetStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ForkSetList contains a list of ForkSet.
type ForkSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ForkSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ForkSet{}, &ForkSetList{})
		return nil
	})
}
