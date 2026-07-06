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

package resume

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// IdlePolicy is the per-session idle behavior the suspender needs, resolved from
// the SandboxTemplate/Session by the operator. TimeoutSeconds<=0 means never
// auto-suspend.
type IdlePolicy struct {
	TimeoutSeconds int
	Action         string // "suspend" | "reset" | "none"
}

// IdlePolicyLookup resolves a session's idle policy. The operator implements it
// over its cached client (session -> pool -> template).
type IdlePolicyLookup func(ctx context.Context, sid string) (IdlePolicy, error)

// Suspender checkpoints idle Running sessions to S3 and frees their workers
// (PRD §6 / P2). It is the counterpart to Resume: Resume binds a worker, Suspend
// releases one, and both are sole-writers of the KV assignment table via the same
// CAS discipline.
type Suspender struct {
	kv        *assign.Client
	clientFor WorkerClientFactory
	policyFor IdlePolicyLookup
	now       func() time.Time
	opts      SuspendOptions
}

// SuspendOptions configure the suspender.
type SuspendOptions struct {
	// SuspendDeadline bounds a single /suspend (checkpoint+upload can take a few
	// seconds). Default 60s.
	SuspendDeadline time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// NewSuspender builds a Suspender.
func NewSuspender(kv *assign.Client, clientFor WorkerClientFactory, policyFor IdlePolicyLookup, opts SuspendOptions) *Suspender {
	if opts.SuspendDeadline == 0 {
		opts.SuspendDeadline = 60 * time.Second
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Suspender{kv: kv, clientFor: clientFor, policyFor: policyFor, now: now, opts: opts}
}

// SweepOnce scans all sessions and suspends (or resets) those that have been idle
// past their policy timeout. Returns the number of sessions acted on.
func (s *Suspender) SweepOnce(ctx context.Context) (int, error) {
	sessions, err := s.kv.ListSessions(ctx)
	if err != nil {
		return 0, err
	}
	nowMS := s.now().UnixMilli()
	acted := 0
	for _, e := range sessions {
		if e.State != resumeapi.StateRunning || e.WorkerPodIP == "" {
			continue // only Running sessions can be idle-suspended
		}
		pol, perr := s.policyFor(ctx, e.SID)
		if perr != nil || pol.TimeoutSeconds <= 0 || pol.Action == "none" || pol.Action == "" {
			continue
		}
		// lastActiveAt==0 (never stamped) is treated as "active now" to avoid
		// suspending a session that just started before its first request lands.
		if e.LastActiveAt == 0 {
			continue
		}
		idleMS := nowMS - e.LastActiveAt
		if idleMS < int64(pol.TimeoutSeconds)*1000 {
			continue
		}
		if err := s.suspendOne(ctx, e, pol.Action); err != nil {
			// non-fatal: log-and-continue is the caller's job; surface via return only
			// if nothing else acted. Keep sweeping other sessions.
			continue
		}
		acted++
	}
	return acted, nil
}

// suspendOne checkpoints (action=suspend) or discards (action=reset) one idle
// session and returns its worker to the idle pool.
func (s *Suspender) suspendOne(ctx context.Context, e *resumeapi.SessionEntry, action string) error {
	ctx, cancel := context.WithTimeout(ctx, s.opts.SuspendDeadline)
	defer cancel()

	// Mark Suspending (CAS) so a concurrent resume/request sees the transition.
	if err := s.casSession(ctx, e.SID, func(se *resumeapi.SessionEntry) {
		se.State = resumeapi.StateSuspending
	}); err != nil {
		return fmt.Errorf("mark suspending: %w", err)
	}

	cl := s.clientFor(e.WorkerPodIP)
	if action == "reset" {
		if err := cl.Reset(ctx, e.SID); err != nil {
			return fmt.Errorf("worker /reset: %w", err)
		}
		// Discarded: delete the session entry entirely.
		_ = s.kv.ReleaseWorker(ctx, e.WorkerPod, e.Pool)
		return s.kv.DeleteSession(ctx, e.SID)
	}

	// action == suspend: checkpoint -> S3 -> worker freed by sandboxd.
	resp, err := cl.Suspend(ctx, e.SID)
	if err != nil {
		return fmt.Errorf("worker /suspend: %w", err)
	}

	// Record Suspended + snapshot; the image/ports already travel in the entry.
	if err := s.casSession(ctx, e.SID, func(se *resumeapi.SessionEntry) {
		se.State = resumeapi.StateSuspended
		se.SnapshotURI = resp.Snapshot
		if resp.Image != "" {
			se.Image = resp.Image
		}
		se.WorkerPod = ""
		se.WorkerPodIP = ""
	}); err != nil {
		return fmt.Errorf("mark suspended: %w", err)
	}

	// Return the (now sandbox-free) worker to the idle pool so it can accept the
	// next session. sandboxd already deleted the sandbox during /suspend.
	return s.kv.ReleaseWorker(ctx, e.WorkerPod, e.Pool)
}

// casSession mirrors the resume workflow's CAS-with-retry.
func (s *Suspender) casSession(ctx context.Context, sid string, mutate func(*resumeapi.SessionEntry)) error {
	const maxTries = 5
	for i := 0; i < maxTries; i++ {
		e, err := s.kv.GetSession(ctx, sid)
		if errors.Is(err, assign.ErrNotFound) {
			return assign.ErrNotFound
		} else if err != nil {
			return err
		}
		mutate(e)
		err = s.kv.PutSessionCAS(ctx, e)
		if err == nil {
			return nil
		}
		if !errors.Is(err, assign.ErrVersionConflict) {
			return err
		}
	}
	return assign.ErrVersionConflict
}
