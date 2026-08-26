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

// AppTemplateSpec is the WORKLOAD half of a sandbox definition: what runs INSIDE the
// nested gVisor sandbox (image + cmd/env/ports/health/idle/checkpoint/iam). It is
// DELIBERATELY scheduling-free — there is no nodeSelector/tolerations/affinity/
// resources/workerImage here — so an application, by construction (the type has no
// field to set), CANNOT dictate worker placement. Placement is a POOL property
// carried by the SandboxTemplate a WarmPool references; an app runs on whatever
// generic pool's workers it is assigned to (docs/sandboxd/PRD/PRD-arbitrary-image-sessions.md §13).
//
// A Session references an AppTemplate via spec.appRef to run that workload on a
// GENERIC pool (a WarmPool whose SandboxTemplate leaves image empty). This lets many
// apps share one pool's capacity without a dedicated WarmPool per image. The fields
// mirror the workload subset of SandboxTemplateSpec so the resolver treats them
// identically.
//
// Health CONSISTENCY guardrail (issue #2): a health probe is NOT required — a
// portless batch/exec/headless workload is legitimate and becomes Ready when its
// process is running (see the worker's readiness handling). What IS refused is an
// INCONSISTENT probe: a tcp/http probe with no port to probe against would never
// succeed and wedge the session. So the rule only fires when a probe type is set:
// then a probe port is mandatory. No probe (or probe: none) is allowed.
// +kubebuilder:validation:XValidation:rule="!has(self.health) || !has(self.health.probe) || !(self.health.probe in ['tcp','http']) || (has(self.health.probePort) && self.health.probePort > 0)",message="a tcp/http health probe requires health.probePort > 0"
type AppTemplateSpec struct {
	// image is the OCI image run AS THE SANDBOX (nested gVisor workload).
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

	// checkpointIntervalSeconds enables periodic background checkpoints (P5): while a
	// session is Running, checkpoint it to S3 every N seconds so a worker crash loses
	// at most ~N seconds of state. 0 = disabled (default).
	// +kubebuilder:validation:Minimum=0
	// +optional
	CheckpointIntervalSeconds int `json:"checkpointIntervalSeconds,omitempty"`

	// iam optionally lets sandboxes running this app assume an AWS IAM role (temporary
	// credentials vended per session by the worker). Off unless set. A Session may
	// override this per session.
	// +optional
	IAM *IAMSpec `json:"iam,omitempty"`

	// network optionally caps the sandbox's network bandwidth (egress/ingress
	// Mbit/s). Enforced host-side on the interior veth by the worker, so it holds
	// outside the guest and survives teleport. Unset = uncapped.
	// +optional
	Network *NetworkSpec `json:"network,omitempty"`
}

// AppTemplateStatus defines the observed state of AppTemplate.
type AppTemplateStatus struct {
	// conditions represent the current state of the AppTemplate resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=appt

// AppTemplate is the Schema for the apptemplates API — the scheduling-free workload
// half of a sandbox definition (docs/sandboxd/PRD/PRD-arbitrary-image-sessions.md §13.2).
type AppTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of AppTemplate
	// +required
	Spec AppTemplateSpec `json:"spec"`

	// status defines the observed state of AppTemplate
	// +optional
	Status AppTemplateStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// AppTemplateList contains a list of AppTemplate
type AppTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []AppTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &AppTemplate{}, &AppTemplateList{})
		return nil
	})
}
