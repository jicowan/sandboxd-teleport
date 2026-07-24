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
	//
	// OPTIONAL as of the generic-pool model (docs/sandboxd/PRD/PRD-arbitrary-image-sessions.md §13):
	//   - SET   => a DEDICATED pool that runs ONLY this image (poolRef-only sessions
	//              get it; this is the classic/original behavior — aio, redis, etc.).
	//   - EMPTY => a GENERIC pool: worker-shape (scheduling/resources) only, running
	//              whatever workload a Session brings via spec.appRef (an AppTemplate).
	// Leaving it empty is how you declare capacity that many apps can share.
	// +optional
	Image string `json:"image,omitempty"`

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

	// checkpointIntervalSeconds enables periodic background checkpoints (P5):
	// while a session is Running, checkpoint it to S3 (leaving it running) every
	// N seconds so a worker crash loses at most ~N seconds of state instead of
	// everything since the last idle-suspend. 0 = disabled (default). Opt-in
	// because it adds S3 churn + brief checkpoint pauses.
	// +kubebuilder:validation:Minimum=0
	// +optional
	CheckpointIntervalSeconds int `json:"checkpointIntervalSeconds,omitempty"`

	// workerImage optionally overrides the sandboxd WORKER image for this pool's
	// workers (NOT the sandbox workload image — that is .image above). The worker
	// image carries the pinned runsc binary that checkpoint/restore depends on, so
	// it is normally a single global value (operator --worker-image) to keep all
	// workers restore/teleport-compatible. Set this only to canary a new worker
	// build on one pool; sessions cannot teleport across workers running
	// incompatible runsc, so a divergent override steps outside that guarantee.
	// Empty = use the operator's global default.
	// +optional
	WorkerImage string `json:"workerImage,omitempty"`

	// streamConsole enables surfacing the nested workload's stdout/stderr to the
	// worker's stdout (→ kubectl logs) for this pool's workers, via the worker's
	// SANDBOXD_STREAM_CONSOLE flag. Default false. The workload console is
	// attacker-controlled and multi-tenant over a worker's lifetime, so this is
	// opt-in per pool (e.g. on for a debug/AIO pool, off for others). The
	// session-scoped /logs API remains the production path regardless.
	// +optional
	StreamConsole bool `json:"streamConsole,omitempty"`

	// iam optionally lets sandboxes in this pool assume an AWS IAM role (temporary
	// credentials vended per session by the worker). Off unless set. A Session may
	// override this per session.
	// +optional
	IAM *IAMSpec `json:"iam,omitempty"`

	// resources is a worker sizing hint informing the WarmPool pod resources.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// scheduling controls worker-pod placement (nodeSelector/tolerations/spread).
	// Optional; the operator applies gVisor-node defaults when unset.
	// +optional
	Scheduling SchedulingSpec `json:"scheduling,omitempty"`
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
