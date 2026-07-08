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

import corev1 "k8s.io/api/core/v1"

// Shared subtypes used across SandboxTemplate / WarmPool / Session (TDD §3).
// PortMap and Health mirror sandboxd's on-wire JSON tags so the operator can hand
// them straight to sandboxd's /run and /restore; the client wire copies live in
// the shared/sbxapi module with identical tags.

// PortMap maps a worker pod port to a sandbox container port (DNAT).
type PortMap struct {
	// container is the port the sandbox process listens on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Container int `json:"container"`

	// host is the port on the worker pod IP. 0 defaults to container.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Host int `json:"host,omitempty"`
}

// Health describes how the worker probes the sandbox and how it restarts it.
type Health struct {
	// restartPolicy on unexpected sandbox exit.
	// +kubebuilder:validation:Enum=none;cold;restore
	// +optional
	RestartPolicy string `json:"restartPolicy,omitempty"`

	// probe type used to determine readiness.
	// +kubebuilder:validation:Enum=none;tcp;http
	// +optional
	Probe string `json:"probe,omitempty"`

	// probePort is the interior container port to probe.
	// +optional
	ProbePort int `json:"probePort,omitempty"`

	// probePath is the HTTP path to probe (http probe only).
	// +optional
	ProbePath string `json:"probePath,omitempty"`
}

// IdlePolicy controls suspend-on-idle behavior.
type IdlePolicy struct {
	// timeoutSeconds of inactivity before the action fires. 0 = never auto-suspend.
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=0
	// +optional
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// action taken when idle: suspend (checkpoint->S3, free worker),
	// reset (discard state, free worker), or none.
	// +kubebuilder:validation:Enum=suspend;reset;none
	// +kubebuilder:default=suspend
	// +optional
	Action string `json:"action,omitempty"`
}

// LocalRef references another object by name in the same namespace.
type LocalRef struct {
	// name of the referenced object.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// IAMSpec configures the AWS IAM role a sandbox may assume. When set, the worker
// vends temporary credentials for this role to the sandbox via a container-
// credentials endpoint on the interior gateway (the AWS SDK auto-refreshes them);
// the credentials are per-session, never the worker's own identity, and survive
// teleport. Empty/unset = the sandbox has no AWS identity. See
// docs/PRD-sandbox-iam-credentials.md.
type IAMSpec struct {
	// roleArn is the ARN of the IAM role the sandbox may assume. Authorization for
	// which sessions may use which role is enforced at the front door / control
	// plane, not here.
	// +optional
	RoleARN string `json:"roleArn,omitempty"`
}

// SchedulingSpec controls how the pool's worker pods are placed. It is a
// pass-through of the standard Kubernetes scheduling primitives — the operator
// injects NO defaults. Whatever you set here is applied verbatim to the worker
// pod spec; whatever you leave empty is simply not set.
//
// This makes placement an explicit operator decision. Choose one:
//   - topologySpreadConstraints — distribute workers across nodes/zones,
//   - affinity — (anti-)affinity rules for finer control,
//   - or set nothing — the scheduler places workers at will.
//
// Note: to pin workers to gVisor nodes you must set nodeSelector/tolerations
// explicitly (e.g. nodeSelector {sandbox: gvisor} + the matching toleration);
// this is no longer assumed.
type SchedulingSpec struct {
	// nodeSelector constrains which nodes workers may land on. Applied verbatim.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations applied to worker pods. Applied verbatim.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// affinity (node/pod/anti- affinity) applied verbatim to the worker pods.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// topologySpreadConstraints applied verbatim to the worker pods.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
}
