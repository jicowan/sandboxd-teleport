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
	Version      int64            `json:"version"`
	LastActiveAt int64            `json:"lastActiveAt,omitempty"` // unix ms; stamped by router (O3)
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
