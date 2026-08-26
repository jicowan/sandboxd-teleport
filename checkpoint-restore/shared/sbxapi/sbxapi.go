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
	// IAMRoleARN, when set, makes the worker vend temporary AWS credentials for this
	// role to the sandbox via a container-credentials endpoint on the interior
	// gateway (the sandbox's AWS SDK auto-refreshes them). Empty = no AWS identity.
	IAMRoleARN string `json:"iamRoleArn,omitempty"`
	// EgressMbps/IngressMbps cap the sandbox's network bandwidth in Mbit/s (0 =
	// uncapped for that direction), enforced host-side on the interior veth (tc token
	// bucket). Per-session and re-established on restore. See
	// PRD-sandbox-network-bandwidth-limits.md.
	EgressMbps  int `json:"egressMbps,omitempty"`
	IngressMbps int `json:"ingressMbps,omitempty"`
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
	SandboxID string `json:"sandboxId"`
	Snapshot  string `json:"snapshot"`
	SizeBytes int64  `json:"sizeBytes"`
	Image     string `json:"image"`
	Digest    string `json:"digest"`
	// Runtime + EngineVersion identify the engine that produced this snapshot so a
	// restore can refuse a cross-runtime or incompatible-version image. RunscVersion
	// is retained as the back-compat alias for EngineVersion (empty Runtime =>
	// "gvisor"; empty EngineVersion => RunscVersion). See
	// docs/sandboxd/PRD/PRD-microvm-runtime-cloud-hypervisor.md §5.2.
	Runtime       string `json:"runtime,omitempty"`
	EngineVersion string `json:"engineVersion,omitempty"`
	RunscVersion  string `json:"runscVersion"`
}

// RestoreRequest is the body of POST /restore.
type RestoreRequest struct {
	SandboxID string `json:"sandboxId"`
	Image     string `json:"image"`
	Snapshot  string `json:"snapshot"`
	// Runtime + EngineVersion pin the snapshot's engine. The worker refuses a
	// cross-runtime restore (hard, version-independent) and an incompatible engine
	// version within the same runtime. RunscVersion is the back-compat alias for
	// EngineVersion. See PRD-microvm-runtime-cloud-hypervisor.md §5.2.
	Runtime       string    `json:"runtime,omitempty"`
	EngineVersion string    `json:"engineVersion,omitempty"`
	RunscVersion  string    `json:"runscVersion,omitempty"`
	Ports         []PortMap `json:"ports,omitempty"`
	Health        *Health   `json:"health,omitempty"`
	// IAMRoleARN re-establishes the session's AWS credential vending after teleport
	// (same role the session ran with). Travels with the session. See RunRequest.
	IAMRoleARN string `json:"iamRoleArn,omitempty"`
	// EgressMbps/IngressMbps re-establish the session's bandwidth caps after teleport
	// (host-side tc state isn't in the checkpoint). Travels with the session. See RunRequest.
	EgressMbps  int `json:"egressMbps,omitempty"`
	IngressMbps int `json:"ingressMbps,omitempty"`
}

// RestoreResponse is the body of a successful POST /restore.
type RestoreResponse struct {
	SandboxID    string    `json:"sandboxId"`
	Status       string    `json:"status"`
	RestoredFrom string    `json:"restoredFrom"`
	Ports        []PortMap `json:"ports,omitempty"`
}

// SuspendRequest is the body of POST /suspend (checkpoint -> S3 -> free worker).
type SuspendRequest struct {
	SandboxID string `json:"sandboxId"`
}

// SuspendResponse is the body of a successful POST /suspend.
type SuspendResponse struct {
	SandboxID string `json:"sandboxId"`
	Snapshot  string `json:"snapshot"` // S3 prefix of the checkpoint
	Image     string `json:"image"`
	Suspended bool   `json:"suspended"`
	// Runtime + EngineVersion identify the engine that produced this checkpoint, so
	// the control plane can record them on the session and refuse a cross-runtime /
	// incompatible-version restore later. See PRD-microvm-runtime-cloud-hypervisor.md §5.2.
	Runtime       string `json:"runtime,omitempty"`
	EngineVersion string `json:"engineVersion,omitempty"`
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
