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
	// poolRef selects a WarmPool — the session's CAPACITY + PLACEMENT source (which
	// pool a worker is claimed from). On a DEDICATED pool (its SandboxTemplate pins an
	// image) poolRef alone runs that image. On a GENERIC pool (its SandboxTemplate has
	// no image — worker-shape only) the pool supplies only capacity and the workload
	// comes from appRef below (docs/sandboxd/PRD/PRD-arbitrary-image-sessions.md §13). poolRef is
	// effectively always required — an appRef/image session still needs a pool to run
	// on.
	// +optional
	PoolRef *LocalRef `json:"poolRef,omitempty"`

	// image is a caller-supplied arbitrary image (inline arbitrary-image mode, O6):
	// admin/kubectl escape hatch, NOT exposed through the front door. When set it is
	// the workload image directly; cmd/env/ports may accompany it. Highest-precedence
	// workload source. Mutually exclusive with appRef.
	// +optional
	Image string `json:"image,omitempty"`

	// appRef optionally names an AppTemplate that supplies the WORKLOAD
	// (image + cmd/env/ports/health/idle/checkpoint/iam), DECOUPLED from poolRef
	// (capacity + placement). This is how a session runs a workload on a GENERIC pool
	// (a WarmPool whose SandboxTemplate has no image): the pool supplies worker-shape,
	// the AppTemplate supplies what to run (docs/sandboxd/PRD/PRD-arbitrary-image-sessions.md §13).
	// An AppTemplate cannot specify scheduling — placement is always the pool's.
	// Resolution precedence: image > appRef > poolRef's own (dedicated-pool) image.
	// Mutually exclusive with image. Additive/optional: inert unless set, so classic
	// poolRef and inline-image sessions are unaffected.
	// +optional
	AppRef *LocalRef `json:"appRef,omitempty"`

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

	// iam optionally lets this session's sandbox assume an AWS IAM role, overriding
	// the pool template's iam. Authorization is enforced at the front door / control
	// plane before the Session is created.
	// +optional
	IAM *IAMSpec `json:"iam,omitempty"`

	// lifecycle overrides template idle/TTL for this session.
	// +optional
	Lifecycle SessionLifecycle `json:"lifecycle,omitempty"`

	// forkFrom records that this Session is a fork child and, for the snapshot
	// source, the base it was seeded from. Set by the ForkSet controller; empty for
	// normal and image-source sessions. It makes a child self-describing (a rebuild
	// knows its base) and is the ref-count decrement key for base reclaim
	// (docs/sandboxd/PRD/PRD-snapshot-fork.md §5.4).
	// +optional
	ForkFrom *ForkProvenance `json:"forkFrom,omitempty"`

	// suspendRequest is an EDGE-TRIGGERED, one-shot request to checkpoint+suspend
	// this session now (docs/sandboxd/PRD/PRD-on-demand-suspend.md). Set it to a fresh OPAQUE
	// token (uuid/timestamp/etc.); when it differs from status.lastSuspendHandled the
	// operator performs exactly one checkpoint->S3->Suspended->free-worker, then sets
	// the watermark equal. It is NOT a level-triggered desired-state: reactive resume
	// (a request to the router) may bring the session back to Running afterward and
	// will NOT be re-suspended, because the token is already handled. Empty = no
	// request.
	// +optional
	SuspendRequest string `json:"suspendRequest,omitempty"`
}

// ForkProvenance records a fork child's origin (docs/sandboxd/PRD/PRD-snapshot-fork.md §5.5).
type ForkProvenance struct {
	// baseRef names the BaseSnapshot this fork was seeded from (snapshot source).
	// Empty for an image-source fork.
	// +optional
	BaseRef *LocalRef `json:"baseRef,omitempty"`

	// snapshotURI is the base snapshot this fork restored from (snapshot source).
	// Empty for an image-source fork.
	// +optional
	SnapshotURI string `json:"snapshotURI,omitempty"`
}

// SessionLifecycle overrides idle and checkpoint-TTL for a single session.
type SessionLifecycle struct {
	// idleTimeoutSeconds overrides the template idle timeout.
	// +optional
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds,omitempty"`

	// idleAction overrides the template idle action for this session: suspend
	// (checkpoint->S3, free worker), reset (discard state, free worker), or none.
	// Lets an ephemeral fork choose reset-on-idle without a dedicated pool
	// (docs/sandboxd/PRD/PRD-snapshot-fork.md §5.3). Empty = inherit the template's action.
	// +kubebuilder:validation:Enum=suspend;reset;none
	// +optional
	IdleAction string `json:"idleAction,omitempty"`

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

	// runtime is the sandbox engine that produced snapshotURI ("gvisor"|"microvm"),
	// mirrored so a rebuilt KV entry still refuses a cross-runtime restore.
	// +optional
	Runtime string `json:"runtime,omitempty"`

	// engineVersion is the engine version that produced snapshotURI (restore identity).
	// +optional
	EngineVersion string `json:"engineVersion,omitempty"`

	// The following fields make the Session.status a LOSSLESS durable mirror of the
	// KV assignment entry, so the Valkey cache can be rebuilt after a restart
	// (PRD-durable-assignment-state) without re-resolving a possibly-changed
	// template. They mirror the resumeapi.SessionEntry.

	// pool is the WarmPool the session's worker is claimed from.
	// +optional
	Pool string `json:"pool,omitempty"`

	// workerPod is the worker pod name currently bound (fencing key); empty when suspended.
	// +optional
	WorkerPod string `json:"workerPod,omitempty"`

	// ports are the session's exposed port mappings (replayed on restore).
	// +optional
	Ports []PortMap `json:"ports,omitempty"`

	// health is the readiness/restart config (replayed on restore).
	// +optional
	Health *Health `json:"health,omitempty"`

	// iamRoleArn is the session's assumable AWS role (replayed on restore).
	// +optional
	IAMRoleARN string `json:"iamRoleArn,omitempty"`

	// lastActiveAt is the last request time stamped by the router (O3). Mirrored
	// coarsely (on transitions, not every request) to avoid write amplification.
	// +optional
	LastActiveAt *metav1.Time `json:"lastActiveAt,omitempty"`

	// lastSuspendHandled is the watermark for the on-demand suspend request
	// (docs/sandboxd/PRD/PRD-on-demand-suspend.md): the spec.suspendRequest token the operator
	// most recently COMPLETED — set only after the checkpoint is durably in S3 and
	// the session is Suspended. A requester waits for
	// status.lastSuspendHandled == spec.suspendRequest (&& snapshotURI != "") to know
	// the snapshot is safe to promote/fork. Equal ⇒ done; differing ⇒ pending.
	// +optional
	LastSuspendHandled string `json:"lastSuspendHandled,omitempty"`

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
