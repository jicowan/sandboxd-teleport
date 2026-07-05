// Package sbxapi holds the wire types for sandboxd's HTTP API, shared by sandboxd
// (server), the control plane, and any client. Keeping them here gives
// compile-time contract safety across components without protobuf/gRPC (PRD O9).
//
// JSON tags MUST match sandboxd's existing on-wire format exactly; these are the
// canonical definitions that sandboxd's in-package structs are migrated to.
package sbxapi

// PortMap maps a worker pod port to a sandbox container port (DNAT).
type PortMap struct {
	Container int `json:"container"`      // port inside the sandbox
	Host      int `json:"host,omitempty"` // port on the worker pod IP (0 => Container)
}

// Health describes how the worker probes the sandbox and restarts it.
type Health struct {
	RestartPolicy  string `json:"restartPolicy,omitempty"` // none|cold|restore
	Probe          string `json:"probe,omitempty"`         // ""|tcp|http
	ProbePort      int    `json:"probePort,omitempty"`
	ProbePath      string `json:"probePath,omitempty"`
	IdleTimeoutSec int    `json:"idleTimeoutSec,omitempty"` // 0 = never idle
}

// RunRequest is the body of POST /run.
type RunRequest struct {
	Image     string    `json:"image"`
	Cmd       []string  `json:"cmd,omitempty"`
	Env       []string  `json:"env,omitempty"`
	SandboxID string    `json:"sandboxId,omitempty"`
	Ports     []PortMap `json:"ports,omitempty"`
	Health    *Health   `json:"health,omitempty"`
}

// RunResponse is the body of a successful POST /run.
type RunResponse struct {
	SandboxID string    `json:"sandboxId"`
	Status    string    `json:"status"`
	Image     string    `json:"image"`
	Ports     []PortMap `json:"ports,omitempty"`
}

// CheckpointRequest is the body of POST /checkpoint.
type CheckpointRequest struct {
	SandboxID    string `json:"sandboxId"`
	LeaveRunning bool   `json:"leaveRunning,omitempty"`
	Compress     *bool  `json:"compress,omitempty"`
}

// CheckpointResponse is the body of a successful POST /checkpoint.
type CheckpointResponse struct {
	SandboxID    string `json:"sandboxId"`
	Snapshot     string `json:"snapshot"`
	SizeBytes    int64  `json:"sizeBytes"`
	Image        string `json:"image"`
	Digest       string `json:"digest"`
	RunscVersion string `json:"runscVersion"`
}

// RestoreRequest is the body of POST /restore.
type RestoreRequest struct {
	SandboxID    string    `json:"sandboxId"`
	Image        string    `json:"image"`
	Snapshot     string    `json:"snapshot"`
	RunscVersion string    `json:"runscVersion,omitempty"`
	Ports        []PortMap `json:"ports,omitempty"`
	Health       *Health   `json:"health,omitempty"`
}

// RestoreResponse is the body of a successful POST /restore.
type RestoreResponse struct {
	SandboxID    string    `json:"sandboxId"`
	Status       string    `json:"status"`
	RestoredFrom string    `json:"restoredFrom"`
	Ports        []PortMap `json:"ports,omitempty"`
}

// StatusResponse is the body of GET /status?sandboxId=.
type StatusResponse struct {
	SandboxID string `json:"sandboxId"`
	Status    string `json:"status"`
	Ready     bool   `json:"ready"`
	Idle      bool   `json:"idle"`
	Restarts  int    `json:"restarts"`
}

// CapacityResponse is the body of GET /capacity.
type CapacityResponse struct {
	Busy      bool   `json:"busy"`
	Count     int    `json:"count"`
	SandboxID string `json:"sandboxId,omitempty"`
	Idle      bool   `json:"idle,omitempty"`
}
