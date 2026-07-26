package main

// The supervisor is a background loop that gives each sandbox health + lifecycle:
//   - liveness: poll `runsc state`; on unexpected stop, apply restartPolicy.
//   - readiness: probe the workload's port (TCP/HTTP) via the interior IP.
//   - idle detection: if no readiness/activity for idleTimeout, mark idle (the
//     control plane can then suspend it). MVP uses a probe-based liveness/ready
//     signal; true byte-level activity tracking is a later refinement.
//
// restartPolicy:
//   none    - do nothing on crash (default; control plane decides)
//   cold    - re-run from the image (fresh)
//   restore - re-restore from the last checkpoint (warm; resumes state) — the C/R
//             superpower, only possible if a snapshot exists.

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

type health struct {
	Policy         string `json:"restartPolicy"`  // none|cold|restore
	Probe          string `json:"probe"`          // ""|tcp|http
	ProbePort      int    `json:"probePort"`      // interior container port to probe
	ProbePath      string `json:"probePath"`      // http path
	IdleTimeoutSec int    `json:"idleTimeoutSec"` // 0 = never idle
}

// runtime health state (not persisted; rebuilt on reconcile)
type healthState struct {
	ready       bool
	restarts    int
	lastReadyAt time.Time
	idle        bool
	oomReported bool // OOM already diagnosed for the current stop (avoid per-tick re-scan/log)
}

func (s *server) supervise(interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		for _, sb := range s.list() {
			s.checkOne(sb)
		}
	}
}

func (s *server) checkOne(sb *sandbox) {
	st, err := s.runsc.state(sb.ID)
	hs := s.healthState(sb.ID)

	// Liveness: container gone/stopped unexpectedly.
	if err != nil || st == "stopped" {
		// Diagnose an out-of-memory death: if the sandbox blew past its per-sandbox
		// memory limit (agent OOM-protection), its OWN cgroup OOM-killed it. Surface
		// that so it reads as an OOM, not a mysterious exit. Best-effort, and logged
		// once per stop episode (guarded so we don't re-scan /sys/fs/cgroup and
		// re-log every supervise tick while a Policy:none sandbox stays stopped).
		// See PRD-worker-memory-reserve.md.
		if !hs.oomReported {
			if n, ok := sandboxOOMKills("/sys/fs/cgroup", sb.ID); ok && n > 0 {
				log.Printf("sandbox %s stopped after %d OOM-kill(s) in its own cgroup — exceeded its per-sandbox memory limit (agent OOM-protection did its job; the agent was spared)", sb.ID, n)
				metrics.inc("sandbox_oom_kills")
			}
			hs.oomReported = true
			s.putHealthState(sb.ID, hs)
		}
		// only act if we think it should be running (has no snapshot-only lifecycle)
		if sb.Health.Policy == "cold" || sb.Health.Policy == "restore" {
			s.restartSandbox(sb)
		}
		return
	}
	if st != "running" {
		return
	}
	// Running again → clear the one-shot OOM-report latch so a future stop re-reports.
	if hs.oomReported {
		hs.oomReported = false
		s.putHealthState(sb.ID, hs)
	}

	// Readiness probe (needs the sandbox network path).
	if sb.Health.Probe != "" && len(sb.Ports) > 0 {
		ok := s.probe(sb)
		hs.ready = ok
		if ok {
			hs.lastReadyAt = time.Now()
			hs.idle = false
		}
	}
	// Idle detection: ready earlier but nothing recent.
	if sb.Health.IdleTimeoutSec > 0 && !hs.lastReadyAt.IsZero() {
		if time.Since(hs.lastReadyAt) > time.Duration(sb.Health.IdleTimeoutSec)*time.Second {
			hs.idle = true
		}
	}
	s.putHealthState(sb.ID, hs)
}

// probe returns true if the workload answers on its probe port (interior IP).
func (s *server) probe(sb *sandbox) bool {
	port := sb.Health.ProbePort
	if port == 0 && len(sb.Ports) > 0 {
		port = sb.Ports[0].Container
	}
	addr := fmt.Sprintf("%s:%d", interiorIP, port)
	switch sb.Health.Probe {
	case "tcp":
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return false
		}
		c.Close()
		return true
	case "http":
		cl := &http.Client{Timeout: 2 * time.Second}
		path := sb.Health.ProbePath
		if path == "" {
			path = "/"
		}
		resp, err := cl.Get("http://" + addr + path)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode < 500
	}
	return false
}

// restartSandbox applies the restart policy after a crash. Bounded by a simple
// retry ceiling to avoid crash loops hammering the node.
func (s *server) restartSandbox(sb *sandbox) {
	hs := s.healthState(sb.ID)
	if hs.restarts >= 5 {
		return // give up; control plane / operator must intervene
	}
	hs.restarts++
	s.putHealthState(sb.ID, hs)
	metrics.inc("restarts")

	// clear the dead runsc state first
	s.runsc.delete(sb.ID)
	teardownSandboxNet()

	// rebuild network if the sandbox had ports
	if len(sb.Ports) > 0 && s.netns {
		if err := setupSandboxNet(s.podIP, sb.Ports); err != nil {
			metrics.inc("restart_failures")
			return
		}
	}
	switch sb.Health.Policy {
	case "restore":
		if sb.Snapshot == "" || s.s3 == nil {
			metrics.inc("restart_failures")
			return
		}
		imgDir := s.imgDir(sb.ID)
		if err := s.s3.downloadPrefix(bgCtx(), sb.Snapshot, imgDir); err != nil {
			metrics.inc("restart_failures")
			return
		}
		// original spec is in the downloaded checkpoint dir
		s.moveSpecFromImg(sb)
		if err := s.runsc.restore(sb.ID, sb.Bundle, imgDir); err != nil {
			metrics.inc("restart_failures")
		}
	case "cold":
		if err := s.runsc.createStart(sb.ID, sb.Bundle); err != nil {
			metrics.inc("restart_failures")
		}
	}
}
