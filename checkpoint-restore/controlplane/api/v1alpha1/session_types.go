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

// SessionSpec defines a user's durable sandbox instance (TDD §3.3). Per O1 the KV
// assignment table is authoritative; this CR is an optional reconciled projection
// for observability and a declarative create path for the front door.
// The object's metadata.name is the session ID.
type SessionSpec struct {
	// poolRef selects a WarmPool (template mode). Mutually exclusive with image.
	// +optional
	PoolRef *LocalRef `json:"poolRef,omitempty"`

	// image is a caller-supplied arbitrary image (arbitrary-image mode, O6):
	// authz-gated at the front door before creation. Mutually exclusive with
	// poolRef. When set, cmd/env/ports may accompany it.
	// +optional
	Image string `json:"image,omitempty"`

	// cmd overrides the entrypoint for arbitrary-image mode.
	// +optional
	Cmd []string `json:"cmd,omitempty"`

	// env for arbitrary-image mode.
	// +optional
	Env []string `json:"env,omitempty"`

	// ports for arbitrary-image mode.
	// +optional
	Ports []PortMap `json:"ports,omitempty"`

	// subject is the opaque identity the router matches the JWT-derived subject
	// against (O4).
	// +optional
	Subject string `json:"subject,omitempty"`

	// lifecycle overrides template idle/TTL for this session.
	// +optional
	Lifecycle SessionLifecycle `json:"lifecycle,omitempty"`
}

// SessionLifecycle overrides idle and checkpoint-TTL for a single session.
type SessionLifecycle struct {
	// idleTimeoutSeconds overrides the template idle timeout.
	// +optional
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds,omitempty"`

	// ttlAfterSuspendSeconds is how long the S3 checkpoint is retained after
	// suspend before GC.
	// +optional
	TTLAfterSuspendSeconds int `json:"ttlAfterSuspendSeconds,omitempty"`
}

// SessionStatus mirrors the authoritative KV assignment entry for observability.
type SessionStatus struct {
	// phase mirrors the KV session state.
	// +kubebuilder:validation:Enum=Absent;Running;Suspending;Suspended;Resuming
	// +optional
	Phase string `json:"phase,omitempty"`

	// workerPodIP is set while Running/Resuming.
	// +optional
	WorkerPodIP string `json:"workerPodIP,omitempty"`

	// snapshotURI is the current checkpoint location (set once one exists).
	// +optional
	SnapshotURI string `json:"snapshotURI,omitempty"`

	// image is the resolved image (recorded for restore identity).
	// +optional
	Image string `json:"image,omitempty"`

	// lastActiveAt is the last request time stamped by the router (O3).
	// +optional
	LastActiveAt *metav1.Time `json:"lastActiveAt,omitempty"`

	// conditions represent the current state of the Session resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sess
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Worker",type=string,JSONPath=`.status.workerPodIP`

// Session is the Schema for the sessions API
type Session struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Session
	// +required
	Spec SessionSpec `json:"spec"`

	// status defines the observed state of Session
	// +optional
	Status SessionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SessionList contains a list of Session
type SessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Session `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Session{}, &SessionList{})
		return nil
	})
}
