package resume

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jicowan/aio-sandbox/shared/resumeapi"
	"github.com/jicowan/aio-sandbox/shared/sbxapi"
)

// stubStatusWorker serves /status with a fixed status/ready, recording call count.
func stubStatusWorker(t *testing.T, status string, ready bool, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/status") {
			if calls != nil {
				*calls++
			}
			json.NewEncoder(w).Encode(sbxapi.StatusResponse{Status: status, Ready: ready})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mkClock returns a now() whose value the test can advance.
func mkClock(start int64) (func() time.Time, func(d time.Duration)) {
	cur := time.UnixMilli(start)
	return func() time.Time { return cur }, func(d time.Duration) { cur = cur.Add(d) }
}

// TestHealerPromotesRunningSandbox: a stuck-Resuming entry whose worker reports the
// sandbox running gets adopted (promoted to Running) once past grace.
func TestHealerPromotesRunningSandbox(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "wedged", State: resumeapi.StateResuming, Pool: "p", WorkerPod: "w1", WorkerPodIP: "10.0.0.1",
	})
	calls := 0
	srv := stubStatusWorker(t, "running", true, &calls)
	now, advance := mkClock(1_000_000_000_000)
	h := NewResumingHealer(kv, clientForStub(srv), 90*time.Second, now)

	// First sweep only RECORDS first-seen (within grace) — no probe, no promote.
	if n, _ := h.SweepOnce(ctx); n != 0 {
		t.Fatalf("first sweep should heal 0 (grace not elapsed), got %d", n)
	}
	if calls != 0 {
		t.Fatalf("must not probe worker within grace; calls=%d", calls)
	}
	// Advance past grace; now it probes and promotes.
	advance(2 * time.Minute)
	n, err := h.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || calls != 1 {
		t.Fatalf("expected 1 heal + 1 status call, got n=%d calls=%d", n, calls)
	}
	e, _ := kv.GetSession(ctx, "wedged")
	if e.State != resumeapi.StateRunning {
		t.Fatalf("entry should be promoted to Running, got %q", e.State)
	}
}

// TestHealerLeavesWhenWorkerStatusFails: if the worker /status errors (can't confirm
// the sandbox), the entry is LEFT as-is (never cleared) for a fresh resume to re-drive.
func TestHealerLeavesWhenWorkerStatusFails(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "wedged2", State: resumeapi.StateResuming, Pool: "p", WorkerPod: "w1", WorkerPodIP: "10.0.0.1",
	})
	// Worker that 404s /status (sandbox not found / worker unhealthy).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	t.Cleanup(srv.Close)
	now, advance := mkClock(1_000_000_000_000)
	h := NewResumingHealer(kv, clientForStub(srv), 90*time.Second, now)

	h.SweepOnce(ctx) // record first-seen
	advance(2 * time.Minute)
	n, err := h.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 heals on status failure, got %d", n)
	}
	e, _ := kv.GetSession(ctx, "wedged2")
	if e.State != resumeapi.StateResuming {
		t.Fatalf("entry must be LEFT Resuming (not cleared), got %q", e.State)
	}
}

// TestHealerIgnoresRunningEntries: entries already Running (or not Resuming) are never
// touched, and their first-seen bookkeeping is pruned.
func TestHealerIgnoresNonResuming(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{SID: "ok", State: resumeapi.StateRunning, Pool: "p", WorkerPodIP: "10.0.0.2"})
	calls := 0
	srv := stubStatusWorker(t, "running", true, &calls)
	now, _ := mkClock(1_000_000_000_000)
	h := NewResumingHealer(kv, clientForStub(srv), 90*time.Second, now)

	n, err := h.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || calls != 0 {
		t.Fatalf("Running entry must be ignored (n=%d calls=%d)", n, calls)
	}
}

// TestHealerSkipsWhenSandboxNotRunning: worker reachable, sandbox not "running", and
// NO snapshotURI (a cold-start that stalled) -> no promote, no rollback; left for retry.
func TestHealerSkipsWhenSandboxNotRunning(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "starting", State: resumeapi.StateResuming, Pool: "p", WorkerPodIP: "10.0.0.1",
		// no SnapshotURI -> nothing to roll back to
	})
	srv := stubStatusWorker(t, "creating", false, nil)
	now, advance := mkClock(1_000_000_000_000)
	h := NewResumingHealer(kv, clientForStub(srv), 90*time.Second, now)
	h.SweepOnce(ctx)
	advance(2 * time.Minute)
	n, _ := h.SweepOnce(ctx)
	if n != 0 {
		t.Fatalf("must not promote/rollback a cold-start with no snapshot, got %d", n)
	}
	e, _ := kv.GetSession(ctx, "starting")
	if e.State != resumeapi.StateResuming {
		t.Fatalf("entry should stay Resuming, got %q", e.State)
	}
}

// TestHealerRollsBackFailedRestore: a stuck-Resuming entry WITH a snapshotURI whose
// worker /status fails (restore failed) is rolled back to Suspended so the next
// request retries the restore — instead of staying Resuming (which would make the next
// resume fall through to the cold-start plan and error "pool is generic"). This is the
// root-cause fix for the snapshot-fork re-resume-after-failed-restore bug.
func TestHealerRollsBackFailedRestore(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "restore-stuck", State: resumeapi.StateResuming, Pool: "p",
		WorkerPod: "w1", WorkerPodIP: "10.0.0.1",
		SnapshotURI: "sandboxes/restore-stuck/snap-1", Image: "img:1",
	})
	// Worker /status errors (restore failed / worker unreachable).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	t.Cleanup(srv.Close)
	now, advance := mkClock(1_000_000_000_000)
	h := NewResumingHealer(kv, clientForStub(srv), 90*time.Second, now)
	h.SweepOnce(ctx) // record first-seen
	advance(2 * time.Minute)
	n, err := h.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 heal (rollback), got %d", n)
	}
	e, _ := kv.GetSession(ctx, "restore-stuck")
	if e.State != resumeapi.StateSuspended {
		t.Fatalf("entry should be rolled back to Suspended, got %q", e.State)
	}
	if e.SnapshotURI == "" || e.WorkerPodIP != "" {
		t.Fatalf("rollback must keep snapshotURI + clear worker: %+v", e)
	}
}

// TestHealerRollsBackNotRunningWithSnapshot: worker reachable but sandbox not running,
// AND the entry has a snapshotURI -> roll back to Suspended (restore didn't take).
func TestHealerRollsBackNotRunningWithSnapshot(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "restore-stopped", State: resumeapi.StateResuming, Pool: "p", WorkerPodIP: "10.0.0.1",
		SnapshotURI: "sandboxes/restore-stopped/snap-1",
	})
	srv := stubStatusWorker(t, "stopped", false, nil)
	now, advance := mkClock(1_000_000_000_000)
	h := NewResumingHealer(kv, clientForStub(srv), 90*time.Second, now)
	h.SweepOnce(ctx)
	advance(2 * time.Minute)
	n, _ := h.SweepOnce(ctx)
	if n != 1 {
		t.Fatalf("expected 1 rollback heal, got %d", n)
	}
	e, _ := kv.GetSession(ctx, "restore-stopped")
	if e.State != resumeapi.StateSuspended {
		t.Fatalf("expected Suspended after rollback, got %q", e.State)
	}
}
