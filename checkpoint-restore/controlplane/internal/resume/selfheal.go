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
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/metrics"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// ResumingHealer reconciles the SPLIT-BRAIN where a KV session entry is stuck in
// state `Resuming` while the sandbox is actually alive+ready on its worker (issue
// #1a). This happens when a resume's cold-start (image pull + boot) outruns the
// operator's resume deadline: the operator's context times out and it returns 502
// WITHOUT writing KV=Running, but the worker keeps going and the sandbox comes up.
// KV is then permanently wrong — /_warm 502s forever and a suspend can't proceed
// (see ErrSuspendTransient / #1b) — until something reconciles it.
//
// The healer scans for entries stuck in `Resuming` past a grace period, asks the
// bound worker for the sandbox's real /status, and:
//   - sandbox running+ready  -> CAS-promote the entry to Running (the resume that
//     "timed out" actually succeeded; adopt it).
//   - sandbox absent/errored -> leave the entry as-is; the next request re-drives a
//     fresh resume through the normal path (we do NOT delete a binding we can't
//     confirm is dead, to avoid racing a resume that's genuinely still in flight).
//
// It is CAS-guarded like every other KV writer, so it can never fight a concurrent
// resume: if a resume flips the entry to Running first, the promote CAS is a no-op
// re-check. Grace ensures we don't touch a resume that is legitimately still within
// its deadline.
type ResumingHealer struct {
	kv        *assign.Client
	clientFor WorkerClientFactory
	notify    assign.PoolNotifier
	mirror    SessionMirror
	// grace is how long an entry must have sat in Resuming before we consider it
	// stuck (must exceed the resume deadline, or we'd race live resumes). The entry's
	// version alone doesn't carry a timestamp, so we gate on how long WE have observed
	// it Resuming (see seenResuming).
	grace time.Duration
	now   func() time.Time
	// seenResuming remembers when we first observed a given sid stuck in Resuming, so
	// grace is measured from first-observation (the entry carries no transition time).
	// sid -> first-seen time. Pruned when an entry leaves Resuming.
	seenResuming map[string]time.Time
}

// NewResumingHealer builds a ResumingHealer. grace should exceed the resume deadline.
func NewResumingHealer(kv *assign.Client, clientFor WorkerClientFactory, grace time.Duration, now func() time.Time) *ResumingHealer {
	if now == nil {
		now = time.Now
	}
	return &ResumingHealer{
		kv:           kv,
		clientFor:    clientFor,
		notify:       assign.NopNotifier{},
		mirror:       nopMirror{},
		grace:        grace,
		now:          now,
		seenResuming: map[string]time.Time{},
	}
}

// WithNotifier wires a PoolNotifier (nudge WarmPool status on a promote). Chains.
func (h *ResumingHealer) WithNotifier(n assign.PoolNotifier) *ResumingHealer {
	if n != nil {
		h.notify = n
	}
	return h
}

// WithMirror wires the durable SessionMirror so a healed promote is also mirrored.
func (h *ResumingHealer) WithMirror(m SessionMirror) *ResumingHealer {
	if m != nil {
		h.mirror = m
	}
	return h
}

// SweepOnce scans all sessions, adopts stuck-Resuming entries whose sandbox is
// actually running, and returns the number healed.
func (h *ResumingHealer) SweepOnce(ctx context.Context) (int, error) {
	t0 := h.now()
	defer func() { metrics.SweepDuration.WithLabelValues("resuming-heal").Observe(time.Since(t0).Seconds()) }()
	log := logf.FromContext(ctx).WithName("resuming-healer")

	sessions, err := h.kv.ListSessions(ctx)
	if err != nil {
		return 0, err
	}

	// Track which sids are currently Resuming so we can prune seenResuming for any
	// that have moved on (avoid unbounded growth).
	current := map[string]bool{}
	healed := 0
	for _, e := range sessions {
		if e.State != resumeapi.StateResuming || e.WorkerPodIP == "" {
			continue
		}
		current[e.SID] = true

		// Record first-observation; only act once the grace has elapsed since then.
		first, seen := h.seenResuming[e.SID]
		if !seen {
			h.seenResuming[e.SID] = h.now()
			continue // give it at least one grace window before probing
		}
		if h.now().Sub(first) < h.grace {
			continue // still within grace — a live resume may legitimately be in flight
		}

		// Grace elapsed and still Resuming: ask the worker for the truth.
		cl := h.clientFor(e.WorkerPodIP)
		st, serr := cl.Status(ctx, e.SID)
		if serr != nil {
			// Can't confirm the sandbox on this worker (worker gone / restore failed /
			// transient). If the entry has a snapshotURI it's a RESTORABLE suspended
			// session whose restore got stuck in Resuming — roll it back to Suspended so
			// the next request retries the restore (issue #1a: otherwise it stays
			// Resuming forever and the next resume falls through to the cold-start plan,
			// erroring "pool is generic, nothing to run" for a snapshot-fork). Without a
			// snapshotURI there's nothing to restore to; leave it (a dead worker is the
			// worker-reclaim sweep's job).
			if e.SnapshotURI != "" {
				if rb := h.rollback(ctx, e.SID); rb {
					healed++
					log.Info("healed stuck-Resuming: rolled back to Suspended for restore retry",
						"session", e.SID, "worker", e.WorkerPodIP, "reason", serr.Error())
				}
				continue
			}
			log.V(1).Info("stuck-Resuming: worker /status failed; leaving for retry",
				"session", e.SID, "worker", e.WorkerPodIP, "err", serr.Error())
			continue
		}
		if st.Status != "running" {
			// Sandbox isn't running (restore failed, or stopped). Same as above: if
			// restorable (has a snapshotURI), roll back to Suspended so the next request
			// retries the restore rather than getting stuck.
			if e.SnapshotURI != "" {
				if rb := h.rollback(ctx, e.SID); rb {
					healed++
					log.Info("healed stuck-Resuming: rolled back to Suspended (sandbox not running)",
						"session", e.SID, "status", st.Status)
				}
				continue
			}
			log.V(1).Info("stuck-Resuming: sandbox not running yet",
				"session", e.SID, "status", st.Status)
			continue
		}

		// The sandbox IS running on the worker — the timed-out resume actually
		// succeeded. Adopt it: CAS-promote the entry to Running. CAS re-reads, so if a
		// concurrent resume already flipped it, our mutate is applied on the latest
		// version (idempotent).
		if err := h.promote(ctx, e.SID, e.WorkerPodIP); err != nil {
			log.Error(err, "stuck-Resuming: promote to Running failed", "session", e.SID)
			continue
		}
		delete(h.seenResuming, e.SID)
		h.notify.PoolChanged(e.Pool)
		healed++
		log.Info("healed stuck-Resuming session (adopted running sandbox)",
			"session", e.SID, "worker", e.WorkerPodIP)
	}

	// Prune first-seen entries for sids no longer Resuming.
	for sid := range h.seenResuming {
		if !current[sid] {
			delete(h.seenResuming, sid)
		}
	}
	return healed, nil
}

// rollback CAS-reverts a stuck-Resuming entry that has a snapshotURI back to
// Suspended (dropping the worker binding), so the next request retries the restore.
// Only acts while still Resuming with a snapshotURI (never disturbs a concurrent
// resume). Returns true if it rolled back. Prunes seenResuming on success.
func (h *ResumingHealer) rollback(ctx context.Context, sid string) bool {
	const maxTries = 5
	for i := 0; i < maxTries; i++ {
		e, err := h.kv.GetSession(ctx, sid)
		if err != nil {
			return false
		}
		if e.State != resumeapi.StateResuming || e.SnapshotURI == "" {
			return false // moved on, or nothing to restore to
		}
		e.State = resumeapi.StateSuspended
		e.WorkerPod, e.WorkerPodIP = "", ""
		if err := h.kv.PutSessionCAS(ctx, e); err == nil {
			mirrorIfDurable(ctx, h.mirror, e)
			delete(h.seenResuming, sid)
			return true
		} else if err != assign.ErrVersionConflict {
			return false
		}
	}
	return false
}

// promote CAS-transitions a stuck-Resuming entry to Running, but ONLY if it is still
// Resuming on the same worker (guards against a concurrent resume/suspend having
// already moved it).
func (h *ResumingHealer) promote(ctx context.Context, sid, workerPodIP string) error {
	const maxTries = 5
	for i := 0; i < maxTries; i++ {
		e, err := h.kv.GetSession(ctx, sid)
		if err != nil {
			return err
		}
		if e.State != resumeapi.StateResuming || e.WorkerPodIP != workerPodIP {
			return nil // already moved on (a real resume won the race) — nothing to do
		}
		e.State = resumeapi.StateRunning
		if err := h.kv.PutSessionCAS(ctx, e); err == nil {
			mirrorIfDurable(ctx, h.mirror, e)
			return nil
		} else if err != assign.ErrVersionConflict {
			return err
		}
		// version conflict -> re-read and retry
	}
	return nil
}
