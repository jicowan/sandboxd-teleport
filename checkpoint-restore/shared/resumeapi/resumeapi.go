// Package resumeapi holds the types for the router<->control-plane Resume contract
// and the KV assignment-table records (TDD §4, §5.1). Shared so the router and the
// operator agree on the wire + record shapes at compile time (PRD O9).
package resumeapi

import "github.com/jicowan/aio-sandbox/shared/sbxapi"

// Session states in the assignment table.
const (
	StateAbsent     = "Absent"
	StateRunning    = "Running"
	StateSuspending = "Suspending"
	StateSuspended  = "Suspended"
	StateResuming   = "Resuming"
)

// Worker states.
const (
	WorkerIdle = "idle"
	WorkerBusy = "busy"
)

// SessionEntry is the authoritative assignment record for a session (key
// session:<sid>). The operator is the sole writer; the router reads it per
// request. Version is the CAS token.
type SessionEntry struct {
	SID          string           `json:"sid"`
	State        string           `json:"state"`
	Pool         string           `json:"pool,omitempty"`
	WorkerPodIP  string           `json:"workerPodIP,omitempty"`  // set while Running/Resuming
	WorkerPod    string           `json:"workerPod,omitempty"`
	Image        string           `json:"image,omitempty"`        // replayed on restore
	SnapshotURI  string           `json:"snapshotURI,omitempty"`  // current checkpoint (one lineage)
	Ports        []sbxapi.PortMap `json:"ports,omitempty"`
	Health       *sbxapi.Health   `json:"health,omitempty"`       // replayed on restore (probe config)
	IAMRoleARN   string           `json:"iamRoleArn,omitempty"`   // session's assumable AWS role; replayed on restore
	// IdleTimeoutSeconds is the session's resolved idle-suspend timeout (from the
	// template/session policy), recorded by the operator on resume so the router's
	// StampActive can compute the suspend deadline (lastActiveAt + timeout) and
	// maintain the suspend:due index WITHOUT a policy lookup on the hot path. 0 =
	// never auto-suspend (not indexed).
	IdleTimeoutSeconds int    `json:"idleTimeoutSeconds,omitempty"`
	// CheckpointIntervalSeconds is the resolved periodic-checkpoint interval (0 =
	// off), recorded so the checkpoint index can be maintained without a lookup.
	CheckpointIntervalSeconds int `json:"checkpointIntervalSeconds,omitempty"`
	Version          int64 `json:"version"`
	LastActiveAt     int64 `json:"lastActiveAt,omitempty"`     // unix ms; stamped by router (O3)
	LastCheckpointAt int64 `json:"lastCheckpointAt,omitempty"` // unix ms; periodic checkpoint (P5)
}

// WorkerEntry registers a worker in the assignment table (key worker:<pod>).
// Written by the operator's pod-label informer (TDD §4.3), never by the worker.
type WorkerEntry struct {
	Pod     string `json:"pod"`
	Pool    string `json:"pool"`
	PodIP   string `json:"podIP"`
	State   string `json:"state"` // idle|busy
	SID     string `json:"sid,omitempty"`
	Version int64  `json:"version"`
}

// ResumeRequest is the router -> operator POST /resume body.
type ResumeRequest struct {
	SID     string `json:"sid"`
	Subject string `json:"subject,omitempty"`
	// Pool is an optional hint (from the broker's X-Session-Pool header): when the
	// session has no Session CR yet, the operator lazily creates one referencing
	// this pool. Ignored once the session exists.
	Pool string `json:"pool,omitempty"`
	// App is an optional hint (from the broker's X-Session-App header): the
	// AppTemplate to run on a GENERIC pool. When set on lazy Session creation, the
	// operator sets Spec.AppRef so the workload comes from that AppTemplate (the pool
	// supplies only capacity). Empty ⇒ classic behavior (a dedicated pool's own image).
	// Ignored once the session exists. See docs/PRD-arbitrary-image-sessions.md §13.
	App string `json:"app,omitempty"`
}

// ResumeResponse is a successful POST /resume body.
type ResumeResponse struct {
	WorkerPodIP string `json:"workerPodIP"`
	State       string `json:"state"`
}

// ResumeError is returned with a non-200 status (503 no capacity, 409 conflict).
type ResumeError struct {
	Error             string `json:"error"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`
}
