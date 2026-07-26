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

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/metrics"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// ErrSuspendTransient is returned by SuspendNow when the session is in a
// TRANSITIONAL state (Resuming/Suspending) and therefore cannot be checkpointed
// right now — as opposed to a genuinely-satisfied state (already Suspended/Absent,
// or no live worker) where the request's intent already holds.
//
// The distinction matters for the on-demand-suspend watermark (issue #1b): the
// SessionReconciler must NOT advance status.lastSuspendHandled on this error, or it
// would report "suspend completed" for a session that was never checkpointed (e.g. a
// session wedged in Resuming because its cold-start outran the resume deadline — the
// sandbox is actually running on the worker but KV still says Resuming). Treat it as
// a retryable no-progress result: requeue, leave the watermark unchanged.
var ErrSuspendTransient = errors.New("session in a transitional state; suspend not yet possible")

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
	notify    assign.PoolNotifier
	mirror    SessionMirror
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
	return &Suspender{kv: kv, clientFor: clientFor, policyFor: policyFor, notify: assign.NopNotifier{}, mirror: nopMirror{}, now: now, opts: opts}
}

// WithNotifier wires a PoolNotifier so a suspend/reset release nudges the
// WarmPool controller to refresh status. Returns s for chaining.
func (s *Suspender) WithNotifier(n assign.PoolNotifier) *Suspender {
	if n != nil {
		s.notify = n
	}
	return s
}

// WithMirror wires a durable SessionMirror (Session.status in etcd). Returns s
// for chaining.
func (s *Suspender) WithMirror(m SessionMirror) *Suspender {
	if m != nil {
		s.mirror = m
	}
	return s
}

// SweepOnce scans all sessions and suspends (or resets) those that have been idle
// past their policy timeout. Returns the number of sessions acted on.
func (s *Suspender) SweepOnce(ctx context.Context) (int, error) {
	t0 := time.Now()
	defer func() { metrics.SweepDuration.WithLabelValues("suspend").Observe(time.Since(t0).Seconds()) }()
	nowMS := s.now().UnixMilli()
	// Read only sessions whose suspend deadline has passed (O(due), not O(N)) —
	// the deadline (lastActiveAt+idleTimeout) is maintained in the suspend:due index
	// by the router's StampActive and the operator's transition writes.
	sessions, err := s.kv.SuspendDue(ctx, nowMS)
	if err != nil {
		return 0, err
	}
	metrics.SweepDue.WithLabelValues("suspend").Set(float64(len(sessions)))
	log := logf.FromContext(ctx).WithName("suspend-sweeper")
	acted := 0
	for _, e := range sessions {
		if e.State != resumeapi.StateRunning || e.WorkerPodIP == "" {
			continue // index may lag a just-changed session; re-check under truth
		}
		// The index says it's due; resolve the action (suspend vs reset vs none).
		// The timeout is already reflected in the index score, so no idle recompute.
		pol, perr := s.policyFor(ctx, e.SID)
		if perr != nil || pol.Action == "none" || pol.Action == "" {
			continue
		}
		if err := s.suspendOne(ctx, e, pol.Action); err != nil {
			// Non-fatal: keep sweeping other sessions. Log it — previously this was
			// counted only in a metric and otherwise silent, so a session whose worker
			// /suspend fails forever (e.g. the sandbox died) looked like nothing was
			// happening while it also shielded the session from GC. The worker-binding
			// reclaim sweep is the backstop that eventually frees such a worker.
			log.Error(err, "idle-suspend failed for session", "sid", e.SID,
				"action", pol.Action, "workerPod", e.WorkerPod)
			metrics.SuspendsTotal.WithLabelValues(pol.Action, metrics.OutcomeError).Inc()
			continue
		}
		metrics.SuspendsTotal.WithLabelValues(pol.Action, metrics.OutcomeSuccess).Inc()
		acted++
	}
	return acted, nil
}

// SuspendNow performs an on-demand checkpoint+suspend of a single session
// (docs/sandboxd/PRD/PRD-on-demand-suspend.md): checkpoint -> S3 -> mark Suspended -> free the
// worker, using the exact same path the idle sweeper uses (suspendOne with
// action=suspend). It is the exported, request-driven entry point the
// SessionReconciler calls when it observes a new spec.suspendRequest.
//
// Idempotent by state, with a THREE-way outcome (issue #1b):
//   - Running -> checkpoint+suspend; return nil on success (watermark advances).
//   - Genuinely satisfied (Suspended, Absent, or not-found in KV) -> return nil; the
//     request's intent already holds, so the caller advances the watermark.
//   - TRANSITIONAL (Resuming/Suspending) -> return ErrSuspendTransient; nothing was
//     checkpointed, so the caller must NOT advance the watermark (it would falsely
//     report completion). The caller requeues and retries once the state settles.
// Any real checkpoint error is returned too, so the caller leaves the watermark
// unchanged and retries.
func (s *Suspender) SuspendNow(ctx context.Context, sid string) error {
	e, err := s.kv.GetSession(ctx, sid)
	if err != nil {
		if errors.Is(err, assign.ErrNotFound) {
			return nil // no such session in KV -> nothing to suspend (satisfied)
		}
		return err
	}
	// Transitional states: the session is mid-flight (Resuming from a cold-start/
	// restore, or already Suspending). Nothing to checkpoint yet, but the request is
	// NOT satisfied — signal a retryable no-progress so the watermark stays put.
	if e.State == resumeapi.StateResuming || e.State == resumeapi.StateSuspending {
		return ErrSuspendTransient
	}
	if e.State != resumeapi.StateRunning || e.WorkerPodIP == "" {
		return nil // Suspended/Absent (or no worker) -> already at/below the request
	}
	if err := s.suspendOne(ctx, e, "suspend"); err != nil {
		metrics.SuspendsTotal.WithLabelValues("suspend", metrics.OutcomeError).Inc()
		return err
	}
	metrics.SuspendsTotal.WithLabelValues("suspend", metrics.OutcomeSuccess).Inc()
	return nil
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
		s.notify.PoolChanged(e.Pool) // busy->idle: refresh pool status
		if derr := s.kv.DeleteSession(ctx, e.SID); derr != nil {
			return derr
		}
		s.mirror.Delete(ctx, e.SID) // drop the durable record too (best-effort)
		return nil
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
	err = s.kv.ReleaseWorker(ctx, e.WorkerPod, e.Pool)
	s.notify.PoolChanged(e.Pool) // busy->idle: refresh pool status
	return err
}

// SuspendForTerminate checkpoints the session on a worker that is TERMINATING
// (its pod is going away — scale-in, drain, rollout, eviction), so the session
// teleport-resumes losslessly instead of dying with the pod. It mirrors the
// idle-suspend flow (Suspending -> /suspend -> Suspended+snapshotURI) with two
// differences:
//
//   - It targets a specific worker (pod/ip/pool/sid known from the WorkerEntry),
//     not an idle-timed-out session.
//   - It REMOVES the worker from KV rather than returning it to the idle pool: a
//     terminating worker must never be handed a new session (it's about to die).
//
// It is idempotent under the CAS discipline: if idle-suspend already moved the
// session to Suspending/Suspended, this no-ops on the missing Running state.
// Bounded by SuspendDeadline so it can't hang a node drain.
func (s *Suspender) SuspendForTerminate(ctx context.Context, sid, workerPod, workerPodIP, pool string) error {
	ctx, cancel := context.WithTimeout(ctx, s.opts.SuspendDeadline)
	defer cancel()

	// Only a Running session on THIS worker is ours to checkpoint. If it's already
	// Suspending/Suspended (idle-suspend beat us) or bound elsewhere, do nothing but
	// still drop the dying worker's KV entry below.
	cur, err := s.kv.GetSession(ctx, sid)
	if err == nil && cur.State == resumeapi.StateRunning && cur.WorkerPod == workerPod {
		if cerr := s.casSession(ctx, sid, func(se *resumeapi.SessionEntry) {
			se.State = resumeapi.StateSuspending
		}); cerr != nil {
			return fmt.Errorf("mark suspending: %w", cerr)
		}
		resp, serr := s.clientFor(workerPodIP).Suspend(ctx, sid)
		if serr != nil {
			return fmt.Errorf("worker /suspend on terminate: %w", serr)
		}
		if cerr := s.casSession(ctx, sid, func(se *resumeapi.SessionEntry) {
			se.State = resumeapi.StateSuspended
			se.SnapshotURI = resp.Snapshot
			if resp.Image != "" {
				se.Image = resp.Image
			}
			se.WorkerPod = ""
			se.WorkerPodIP = ""
		}); cerr != nil {
			return fmt.Errorf("mark suspended: %w", cerr)
		}
		metrics.SuspendsTotal.WithLabelValues("terminate", metrics.OutcomeSuccess).Inc()
	}

	// Remove the terminating worker from KV (NOT release to idle). Safe even if the
	// session wasn't ours — the pod is going away regardless.
	rerr := s.kv.RemoveWorker(ctx, workerPod, pool)
	s.notify.PoolChanged(pool)
	return rerr
}

// ReleaseForDelete tears down a session's authoritative KV footprint when its
// Session CR is being DELETED (direct delete, ForkSet ownerRef cascade, or ForkSet
// scale-in). This is the delete-time counterpart of the idle-`reset` path
// (suspendOne action=="reset"): the session is being discarded, so we do NOT
// checkpoint — we free the worker and drop the session entry.
//
// Without this, deleting a Session CR is a silent no-op against Valkey: the
// `session:<sid>` entry stays Running and the `worker:<pod>` stays busy, so the
// worker is never returned to the pool and the WarmPool never scales back down (the
// self-healing sweeps all key on the KV entry, which the CR delete leaves intact).
// A Session finalizer calls this before removing its finalizer.
//
// Idempotent: a missing session entry (already gone) is success. If a live worker is
// still bound, its sandbox is reset (best-effort — the worker frees it on next use
// regardless) and the worker returned to the idle pool.
func (s *Suspender) ReleaseForDelete(ctx context.Context, sid string) error {
	ctx, cancel := context.WithTimeout(ctx, s.opts.SuspendDeadline)
	defer cancel()

	e, err := s.kv.GetSession(ctx, sid)
	if err != nil {
		if errors.Is(err, assign.ErrNotFound) {
			return nil // already gone — nothing to release
		}
		return fmt.Errorf("get session %q for delete: %w", sid, err)
	}

	// If a worker is bound, best-effort reset its sandbox (discard) and return it to
	// idle. A reset failure (worker gone/unreachable) must not block the release — the
	// worker binding is dropped below regardless so the pool recovers.
	if e.WorkerPod != "" {
		if e.WorkerPodIP != "" {
			if rerr := s.clientFor(e.WorkerPodIP).Reset(ctx, sid); rerr != nil {
				metrics.SuspendsTotal.WithLabelValues("delete", metrics.OutcomeError).Inc()
			}
		}
		_ = s.kv.ReleaseWorker(ctx, e.WorkerPod, e.Pool)
		s.notify.PoolChanged(e.Pool) // busy->idle: refresh pool status
	}

	if derr := s.kv.DeleteSession(ctx, sid); derr != nil {
		return fmt.Errorf("delete session %q: %w", sid, derr)
	}
	s.mirror.Delete(ctx, sid) // drop the durable record too (best-effort)
	metrics.SuspendsTotal.WithLabelValues("delete", metrics.OutcomeSuccess).Inc()
	return nil
}

// casSession mirrors the resume workflow's CAS-with-retry.
// TECH-DEBT (R5, issue #8): duplicated with Workflow.casSessionSeed (resume.go) and
// checkpointOne (checkpoint.go); consolidation deferred — see the note on
// casSessionSeed for why.
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
			mirrorIfDurable(ctx, s.mirror, e) // etcd mirror only on durability-critical transitions
			return nil
		}
		if !errors.Is(err, assign.ErrVersionConflict) {
			return err
		}
	}
	return assign.ErrVersionConflict
}
