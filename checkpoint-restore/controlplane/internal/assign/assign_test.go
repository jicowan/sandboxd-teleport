package assign

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
}

func TestSessionCASCreateAndUpdate(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	// Create (version 0 -> 1).
	e := &resumeapi.SessionEntry{SID: "s1", State: resumeapi.StateResuming, Version: 0}
	if err := c.PutSessionCAS(ctx, e); err != nil {
		t.Fatalf("create: %v", err)
	}
	if e.Version != 1 {
		t.Fatalf("want version 1, got %d", e.Version)
	}

	// Update at correct version (1 -> 2).
	e.State = resumeapi.StateRunning
	if err := c.PutSessionCAS(ctx, e); err != nil {
		t.Fatalf("update: %v", err)
	}
	if e.Version != 2 {
		t.Fatalf("want version 2, got %d", e.Version)
	}

	got, err := c.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != resumeapi.StateRunning || got.Version != 2 {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestSessionCASConflict(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	e := &resumeapi.SessionEntry{SID: "s1", State: resumeapi.StateResuming, Version: 0}
	if err := c.PutSessionCAS(ctx, e); err != nil {
		t.Fatalf("create: %v", err)
	} // version now 1

	// Simulate a second writer holding a stale copy at version 0.
	stale := &resumeapi.SessionEntry{SID: "s1", State: resumeapi.StateSuspended, Version: 0}
	if err := c.PutSessionCAS(ctx, stale); err != ErrVersionConflict {
		t.Fatalf("want ErrVersionConflict, got %v", err)
	}

	// The winning writer (version 1) still succeeds.
	e.State = resumeapi.StateRunning
	if err := c.PutSessionCAS(ctx, e); err != nil {
		t.Fatalf("winner update: %v", err)
	}
}

func TestWorkerUpsertIdleSet(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	w := &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.0.0.1", State: resumeapi.WorkerIdle}
	if err := c.UpsertWorker(ctx, w); err != nil {
		t.Fatalf("upsert idle: %v", err)
	}
	idle, err := c.IdleWorkers(ctx, "p")
	if err != nil || len(idle) != 1 || idle[0] != "w1" {
		t.Fatalf("idle set wrong: %v %v", idle, err)
	}

	// Flip to busy -> leaves the idle set.
	w.State = resumeapi.WorkerBusy
	w.SID = "s1"
	if err := c.UpsertWorker(ctx, w); err != nil {
		t.Fatalf("upsert busy: %v", err)
	}
	idle, _ = c.IdleWorkers(ctx, "p")
	if len(idle) != 0 {
		t.Fatalf("want empty idle set, got %v", idle)
	}

	// Remove entirely.
	if err := c.RemoveWorker(ctx, "w1", "p"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := c.GetWorker(ctx, "w1"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSuspendDueIndex(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	now := int64(1_000_000_000_000)

	// Running + idle past timeout -> due at (lastActiveAt+timeout) < now.
	must(t, c.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "due", State: resumeapi.StateRunning, LastActiveAt: now - 100_000, IdleTimeoutSeconds: 30,
	}))
	// Running but active recently -> deadline in the future, NOT due.
	must(t, c.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "fresh", State: resumeapi.StateRunning, LastActiveAt: now - 5_000, IdleTimeoutSeconds: 30,
	}))
	// Running but no timeout -> never indexed.
	must(t, c.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "notimeout", State: resumeapi.StateRunning, LastActiveAt: now - 100_000,
	}))

	due, err := c.SuspendDue(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].SID != "due" {
		t.Fatalf("want only [due], got %v", sids(due))
	}

	// A non-Running transition removes it from the index.
	e, _ := c.GetSession(ctx, "due")
	e.State = resumeapi.StateSuspended
	must(t, c.PutSessionCAS(ctx, e))
	due, _ = c.SuspendDue(ctx, now)
	if len(due) != 0 {
		t.Fatalf("suspended session must leave the index, got %v", sids(due))
	}
}

// TestSuspendDuePrunesStaleIndexMember guards the dueEntries MGET path: a due-index
// member whose session:<sid> entry no longer exists (deleted out-of-band) must be
// skipped AND pruned from the index, not returned.
func TestSuspendDuePrunesStaleIndexMember(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	now := int64(1_000_000_000_000)

	// One real due session + one whose entry we then delete, leaving a stale index ref.
	must(t, c.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "real", State: resumeapi.StateRunning, LastActiveAt: now - 100_000, IdleTimeoutSeconds: 30,
	}))
	must(t, c.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "ghost", State: resumeapi.StateRunning, LastActiveAt: now - 100_000, IdleTimeoutSeconds: 30,
	}))
	// Delete the ghost's entry directly (bypassing DeleteSession) so its due-index
	// member is left dangling — exactly the stale case dueEntries must tolerate.
	must(t, c.rdb.Del(ctx, sessionKey("ghost")).Err())

	due, err := c.SuspendDue(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].SID != "real" {
		t.Fatalf("want only [real] (ghost skipped), got %v", sids(due))
	}
	// The stale member must have been pruned from the index, so a second sweep is clean.
	due2, _ := c.SuspendDue(ctx, now)
	if len(due2) != 1 || due2[0].SID != "real" {
		t.Fatalf("after prune want [real], got %v", sids(due2))
	}
}

func TestStampActiveSlidesDeadline(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	now := int64(1_000_000_000_000)
	must(t, c.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s", State: resumeapi.StateRunning, LastActiveAt: now - 100_000, IdleTimeoutSeconds: 30,
	}))
	// Due now...
	if due, _ := c.SuspendDue(ctx, now); len(due) != 1 {
		t.Fatalf("expected due before stamp")
	}
	// ...stamp fresh activity -> deadline slides to now+30s, no longer due.
	must(t, c.StampActive(ctx, "s", now))
	if due, _ := c.SuspendDue(ctx, now); len(due) != 0 {
		t.Fatalf("stamp should slide deadline forward; still due: %v", sids(due))
	}
}

func TestCheckpointDueIndex(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	now := int64(1_000_000_000_000)
	// opted-in + interval elapsed since last checkpoint -> due.
	must(t, c.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "ck", State: resumeapi.StateRunning, LastCheckpointAt: now - 100_000, CheckpointIntervalSeconds: 30,
	}))
	// not opted in -> not indexed.
	must(t, c.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "off", State: resumeapi.StateRunning, LastActiveAt: now - 100_000,
	}))
	due, err := c.CheckpointDue(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].SID != "ck" {
		t.Fatalf("want only [ck], got %v", sids(due))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func sids(es []*resumeapi.SessionEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.SID
	}
	return out
}

// TestDiscoveryUpsertCannotResurrectBusyWorker reproduces the fork-fan-out
// double-booking race (docs/PRD-snapshot-fork.md): worker-discovery re-registers a
// pod as idle (a pod event firing in the window after a claim) MUST NOT overwrite a
// busy binding or re-add the pod to the idle set — else a second claim SPOPs the same
// worker and two sessions land on it.
func TestDiscoveryUpsertCannotResurrectBusyWorker(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	// One idle worker; claim it for s1 (atomic SPOP + SET busy).
	must(t, c.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", State: resumeapi.WorkerIdle}))
	w, err := c.ClaimIdleWorker(ctx, "p", "s1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if w.Pod != "w1" || w.State != resumeapi.WorkerBusy {
		t.Fatalf("claim result: got pod=%s state=%s", w.Pod, w.State)
	}

	// Discovery fires on the just-claimed pod and tries to register it idle again
	// (the resurrection the bug relied on).
	must(t, c.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", State: resumeapi.WorkerIdle}))

	// The stored entry must still be busy + bound to s1...
	got, err := c.GetWorker(ctx, "w1")
	if err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got.State != resumeapi.WorkerBusy || got.SID != "s1" {
		t.Fatalf("busy binding clobbered by discovery: state=%s sid=%q", got.State, got.SID)
	}
	// ...and it must NOT be back in the idle set, so a second claim finds no capacity.
	idle, total, _ := c.CountWorkers(ctx, "p")
	if idle != 0 || total != 1 {
		t.Fatalf("worker resurrected into idle set: idle=%d total=%d (want 0/1)", idle, total)
	}
	if _, err := c.ClaimIdleWorker(ctx, "p", "s2"); err != ErrNoCapacity {
		t.Fatalf("second claim should find NO capacity, got err=%v (double-booking!)", err)
	}

	// Sanity: a legitimate release DOES return it to idle (busy->idle is allowed via
	// ReleaseWorker, just not via discovery upsert).
	must(t, c.ReleaseWorker(ctx, "w1", "p"))
	idle, _, _ = c.CountWorkers(ctx, "p")
	if idle != 1 {
		t.Fatalf("after ReleaseWorker: want idle=1, got %d", idle)
	}
}

// TestReleaseDoesNotResurrectDeletedWorker guards the phantom-idle-member bug: a
// rollback release of a worker whose entry was already removed (pod deleted / pruned,
// e.g. a failed fork materialization racing scale-in) must NOT re-add it to the idle
// set. A phantom idle member (in pool:idle but no worker:<pod>) is invisible to prune,
// lets a dead pod be claimed, and drives busy = total-idle negative.
func TestReleaseDoesNotResurrectDeletedWorker(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	must(t, c.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", State: resumeapi.WorkerIdle}))
	// Claim then REMOVE the worker (pod deleted) before the rollback release runs.
	if _, err := c.ClaimIdleWorker(ctx, "p", "s1"); err != nil {
		t.Fatal(err)
	}
	must(t, c.RemoveWorker(ctx, "w1", "p"))

	// Rollback release of the now-deleted worker: must be a no-op on the idle set.
	must(t, c.ReleaseWorker(ctx, "w1", "p"))

	idle, total, err := c.CountWorkers(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if idle != 0 || total != 0 {
		t.Fatalf("phantom idle member resurrected: idle=%d total=%d (want 0/0)", idle, total)
	}
	// And a claim must find no capacity (not SPOP a dead pod).
	if _, err := c.ClaimIdleWorker(ctx, "p", "s2"); err != ErrNoCapacity {
		t.Fatalf("claim should find no capacity, got %v", err)
	}
}

func TestCountWorkersUsesSets(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	// two idle + one busy in pool p; one worker in pool q.
	must(t, c.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", State: resumeapi.WorkerIdle}))
	must(t, c.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w2", Pool: "p", State: resumeapi.WorkerIdle}))
	must(t, c.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w3", Pool: "p", State: resumeapi.WorkerBusy}))
	must(t, c.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "q1", Pool: "q", State: resumeapi.WorkerIdle}))

	idle, total, err := c.CountWorkers(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if idle != 2 || total != 3 {
		t.Fatalf("pool p: want idle=2 total=3, got idle=%d total=%d", idle, total)
	}

	// A claim keeps the worker in the pool (total unchanged), moves it out of idle.
	if _, err := c.ClaimIdleWorker(ctx, "p", "s1"); err != nil {
		t.Fatal(err)
	}
	idle, total, _ = c.CountWorkers(ctx, "p")
	if idle != 1 || total != 3 {
		t.Fatalf("after claim: want idle=1 total=3, got idle=%d total=%d", idle, total)
	}

	// Remove drops it from the pool entirely.
	must(t, c.RemoveWorker(ctx, "w3", "p"))
	_, total, _ = c.CountWorkers(ctx, "p")
	if total != 2 {
		t.Fatalf("after remove: want total=2, got %d", total)
	}

	// PoolWorkers returns only pool p's workers.
	pw, err := c.PoolWorkers(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 2 {
		t.Fatalf("PoolWorkers(p): want 2, got %d", len(pw))
	}
}
