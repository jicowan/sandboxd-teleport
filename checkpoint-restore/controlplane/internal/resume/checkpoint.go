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
	"github.com/jicowan/aio-sandbox/controlplane/internal/metrics"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// CheckpointPolicy is the per-session periodic-checkpoint config, resolved from
// the template. IntervalSeconds<=0 means disabled.
type CheckpointPolicy struct {
	IntervalSeconds int
}

// CheckpointPolicyLookup resolves a session's periodic-checkpoint policy.
type CheckpointPolicyLookup func(ctx context.Context, sid string) (CheckpointPolicy, error)

// Checkpointer periodically checkpoints long-lived Running sessions to S3 while
// leaving them running (P5, opt-in). It reduces data loss on a worker crash: a
// crashed Running session restores from its most recent periodic checkpoint
// instead of only the last idle-suspend (or nothing). It does NOT change session
// state — the session stays Running; only SnapshotURI/LastCheckpointAt advance.
type Checkpointer struct {
	kv        *assign.Client
	clientFor WorkerClientFactory
	policyFor CheckpointPolicyLookup
	mirror    SessionMirror
	now       func() time.Time
	deadline  time.Duration
}

// NewCheckpointer builds a Checkpointer. deadline bounds a single /checkpoint
// (checkpoint+upload); default 60s.
func NewCheckpointer(kv *assign.Client, clientFor WorkerClientFactory, policyFor CheckpointPolicyLookup, deadline time.Duration, now func() time.Time) *Checkpointer {
	if deadline == 0 {
		deadline = 60 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &Checkpointer{kv: kv, clientFor: clientFor, policyFor: policyFor, mirror: nopMirror{}, now: now, deadline: deadline}
}

// WithMirror wires a durable SessionMirror so a periodic-checkpoint advance (which
// moves the durable snapshot forward while the session stays Running) is recorded
// in etcd — the one Running transition that affects recovery. Returns c.
func (c *Checkpointer) WithMirror(m SessionMirror) *Checkpointer {
	if m != nil {
		c.mirror = m
	}
	return c
}

// SweepOnce checkpoints Running sessions whose opt-in interval has elapsed since
// their last (periodic) checkpoint. Returns the number checkpointed.
func (c *Checkpointer) SweepOnce(ctx context.Context) (int, error) {
	t0 := time.Now()
	defer func() { metrics.SweepDuration.WithLabelValues("checkpoint").Observe(time.Since(t0).Seconds()) }()
	nowMS := c.now().UnixMilli()
	// Read only sessions whose next periodic-checkpoint deadline has passed. The
	// index is empty unless a session opted in (checkpointIntervalSeconds>0), so
	// this is a no-op when the feature is off — no full-table scan.
	sessions, err := c.kv.CheckpointDue(ctx, nowMS)
	if err != nil {
		return 0, err
	}
	metrics.SweepDue.WithLabelValues("checkpoint").Set(float64(len(sessions)))
	done := 0
	for _, e := range sessions {
		if e.State != resumeapi.StateRunning || e.WorkerPodIP == "" {
			continue // index may lag a just-changed session; re-check under truth
		}
		if err := c.checkpointOne(ctx, e); err != nil {
			metrics.CheckpointsTotal.WithLabelValues(metrics.OutcomeError).Inc()
			continue
		}
		metrics.CheckpointsTotal.WithLabelValues(metrics.OutcomeSuccess).Inc()
		done++
	}
	return done, nil
}

// checkpointOne checkpoints a single Running session in place and records the new
// snapshot. The session stays Running and the worker stays busy — this is NOT a
// suspend. CAS-guards the metadata update so it can't clobber a concurrent
// suspend/resume transition (if the session is no longer Running, skip).
func (c *Checkpointer) checkpointOne(ctx context.Context, e *resumeapi.SessionEntry) error {
	ctx, cancel := context.WithTimeout(ctx, c.deadline)
	defer cancel()

	resp, err := c.clientFor(e.WorkerPodIP).Checkpoint(ctx, e.SID, true /* leaveRunning */)
	if err != nil {
		return fmt.Errorf("worker /checkpoint: %w", err)
	}

	// Advance SnapshotURI/LastCheckpointAt only if still Running on the same worker
	// (a suspend/resume may have moved it meanwhile — don't overwrite that).
	// Seed the first CAS attempt from the entry the sweeper already loaded (e);
	// only re-GET on an actual version conflict. Correctness is unchanged: the CAS
	// version guard + the State/WorkerPod re-check below still gate every write.
	// TECH-DEBT (R5, issue #8): this CAS-retry loop duplicates Workflow.casSessionSeed
	// (resume.go) and Suspender.casSession (suspend.go); consolidation deferred — see
	// the note on casSessionSeed.
	const maxTries = 5
	cur := e
	for i := 0; i < maxTries; i++ {
		if i > 0 {
			var gerr error
			cur, gerr = c.kv.GetSession(ctx, e.SID)
			if gerr != nil {
				return gerr
			}
		}
		if cur.State != resumeapi.StateRunning || cur.WorkerPod != e.WorkerPod {
			return nil // moved on; the periodic snapshot is stale, drop it silently
		}
		cur.SnapshotURI = resp.Snapshot
		cur.LastCheckpointAt = c.now().UnixMilli()
		err = c.kv.PutSessionCAS(ctx, cur)
		if err == nil {
			// Mirror the advanced snapshot to etcd: this Running transition DOES move
			// the durable recovery point forward, so it's worth persisting (unlike
			// resume's Running writes). Low frequency (once per opt-in interval).
			c.mirror.Mirror(ctx, cur)
			return nil
		}
		if !errors.Is(err, assign.ErrVersionConflict) {
			return err
		}
	}
	return assign.ErrVersionConflict
}
