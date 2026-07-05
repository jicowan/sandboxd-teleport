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
