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
