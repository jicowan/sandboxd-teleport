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

// BaseSnapshotSpec declares a forkable "golden" checkpoint, decoupled from any one
// session's mutable snapshotURI lineage (docs/sandboxd/PRD/PRD-snapshot-fork.md §5.1). The
// controller resolves the source snapshot, S3 server-side-copies it to a fork-stable
// bases/<name>/ prefix (copy-on-promote), and records the restore identity in status.
type BaseSnapshotSpec struct {
	// sourceSessionRef names a SUSPENDED Session whose current snapshot is promoted
	// to this base. (Increment 2 supports promoting an already-suspended session; a
	// checkpoint-a-live-session mode is a follow-up.)
	// +kubebuilder:validation:Required
	SourceSessionRef LocalRef `json:"sourceSessionRef"`

	// pinned keeps the base indefinitely: while true it is never auto-reclaimed even
	// when refCount reaches 0. Set false to let the base be reclaimed once unreferenced.
	// +optional
	Pinned bool `json:"pinned,omitempty"`
}

// BaseSnapshot phase values.
const (
	BaseSnapshotPending   = "Pending"   // resolving source / copy in progress
	BaseSnapshotReady     = "Ready"     // copy-on-promote complete; forkable
	BaseSnapshotReclaimed = "Reclaimed" // being torn down (unpinned + refCount 0)
	BaseSnapshotFailed    = "Failed"    // source not suspended / no snapshot / copy failed
)

// BaseSnapshotStatus records the promoted artifact + the restore identity a fork
// needs, so a fork child needs no back-reference to the origin session.
type BaseSnapshotStatus struct {
	// snapshotURI is the fork-stable S3 prefix (bases/<name>/snap-…) the base was
	// copied to. Set once copy-on-promote completes.
	// +optional
	SnapshotURI string `json:"snapshotURI,omitempty"`

	// The resolved restore identity, copied from the source session at promote time
	// so forks are self-describing.
	// +optional
	Image string `json:"image,omitempty"`
	// +optional
	RunscVersion string `json:"runscVersion,omitempty"`
	// +optional
	Ports []PortMap `json:"ports,omitempty"`
	// +optional
	Health *Health `json:"health,omitempty"`
	// +optional
	IAMRoleARN string `json:"iamRoleArn,omitempty"`

	// refCount is the number of holds keeping the base alive: forks that have not yet
	// completed their first restore, plus explicit pins. A fork needs the base only
	// for its first restore, so this drops as forks materialize. Reclaim is eligible
	// only when pinned==false AND refCount==0 (docs/sandboxd/PRD/PRD-snapshot-fork.md §5.4).
	// +optional
	RefCount int32 `json:"refCount"`

	// ready is true once the base is forkable (copy-on-promote complete).
	// +optional
	Ready bool `json:"ready"`

	// phase is the base lifecycle state.
	// +kubebuilder:validation:Enum=Pending;Ready;Reclaimed;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// conditions represent the current state of the BaseSnapshot resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=basesnap
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Refs",type=integer,JSONPath=`.status.refCount`
// +kubebuilder:printcolumn:name="Pinned",type=boolean,JSONPath=`.spec.pinned`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// BaseSnapshot is the Schema for the basesnapshots API.
type BaseSnapshot struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired base snapshot
	// +required
	Spec BaseSnapshotSpec `json:"spec"`

	// status defines the observed state
	// +optional
	Status BaseSnapshotStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BaseSnapshotList contains a list of BaseSnapshot.
type BaseSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BaseSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BaseSnapshot{}, &BaseSnapshotList{})
		return nil
	})
}
