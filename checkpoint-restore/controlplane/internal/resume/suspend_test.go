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

// stubSuspendWorker serves /suspend, returning a snapshot URI.
func stubSuspendWorker(t *testing.T, snap string, gotSuspend *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/suspend") {
			*gotSuspend = true
			var req sbxapi.SuspendRequest
			json.NewDecoder(r.Body).Decode(&req)
			json.NewEncoder(w).Encode(sbxapi.SuspendResponse{
				SandboxID: req.SandboxID, Snapshot: snap, Image: "redis:7-alpine", Suspended: true,
			})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fixedNow(ms int64) func() time.Time {
	return func() time.Time { return time.UnixMilli(ms) }
}

func TestSweepSuspendsIdleSession(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	// Running session, last active 100s ago; worker busy + bound.
	now := int64(1_000_000_000_000)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, Pool: "p",
		WorkerPod: "w1", WorkerPodIP: "10.0.0.1", Image: "redis:7-alpine",
		Ports: []sbxapi.PortMap{{Container: 6379, Host: 6379}}, LastActiveAt: now - 100_000,
		IdleTimeoutSeconds: 30, // deadline = (now-100s)+30s -> due before now
	})
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.0.0.1", State: resumeapi.WorkerBusy, SID: "s1"})

	var gotSuspend bool
	srv := stubSuspendWorker(t, "sandboxes/s1/snap-999", &gotSuspend)
	policy := func(context.Context, string) (IdlePolicy, error) {
		return IdlePolicy{TimeoutSeconds: 30, Action: "suspend"}, nil // 30s < 100s idle -> suspend
	}
	s := NewSuspender(kv, clientForStub(srv), policy, SuspendOptions{Now: fixedNow(now)})

	n, err := s.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || !gotSuspend {
		t.Fatalf("expected 1 suspend, got n=%d gotSuspend=%v", n, gotSuspend)
	}
	e, _ := kv.GetSession(ctx, "s1")
	if e.State != resumeapi.StateSuspended || e.SnapshotURI != "sandboxes/s1/snap-999" || e.WorkerPodIP != "" {
		t.Fatalf("post-suspend entry wrong: %+v", e)
	}
	// worker returned to idle pool
	idle, _ := kv.IdleWorkers(ctx, "p")
	if len(idle) != 1 || idle[0] != "w1" {
		t.Fatalf("worker not returned to idle: %v", idle)
	}
}

func TestSweepSkipsActiveSession(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	now := int64(1_000_000_000_000)
	// active 5s ago, timeout 30s -> deadline in the future -> NOT due in the index
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, Pool: "p", WorkerPod: "w1", WorkerPodIP: "10.0.0.1",
		LastActiveAt: now - 5_000, IdleTimeoutSeconds: 30,
	})
	var gotSuspend bool
	srv := stubSuspendWorker(t, "x", &gotSuspend)
	policy := func(context.Context, string) (IdlePolicy, error) {
		return IdlePolicy{TimeoutSeconds: 30, Action: "suspend"}, nil
	}
	s := NewSuspender(kv, clientForStub(srv), policy, SuspendOptions{Now: fixedNow(now)})
	n, _ := s.SweepOnce(ctx)
	if n != 0 || gotSuspend {
		t.Fatalf("should not suspend active session: n=%d suspend=%v", n, gotSuspend)
	}
}

func TestSuspendForTerminateCheckpointsAndRemovesWorker(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, Pool: "p",
		WorkerPod: "w1", WorkerPodIP: "10.0.0.1", Image: "redis:7-alpine",
	})
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.0.0.1", State: resumeapi.WorkerBusy, SID: "s1"})

	var gotSuspend bool
	srv := stubSuspendWorker(t, "sandboxes/s1/snap-term", &gotSuspend)
	s := NewSuspender(kv, clientForStub(srv), nil, SuspendOptions{})

	if err := s.SuspendForTerminate(ctx, "s1", "w1", "10.0.0.1", "p"); err != nil {
		t.Fatal(err)
	}
	if !gotSuspend {
		t.Fatal("expected /suspend to be called on terminate")
	}
	e, _ := kv.GetSession(ctx, "s1")
	if e.State != resumeapi.StateSuspended || e.SnapshotURI != "sandboxes/s1/snap-term" || e.WorkerPodIP != "" {
		t.Fatalf("post-terminate session wrong: %+v", e)
	}
	// The terminating worker must NOT be returned to the idle pool (it's dying).
	idle, _ := kv.IdleWorkers(ctx, "p")
	if len(idle) != 0 {
		t.Fatalf("terminating worker must not be idle-returned, got %v", idle)
	}
	// And its worker entry is gone.
	if _, err := kv.GetWorker(ctx, "w1"); err == nil {
		t.Fatal("expected worker entry removed on terminate")
	}
}

func TestSuspendForTerminateIdempotentWhenAlreadySuspended(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	// idle-suspend already moved it: Suspended, no worker binding.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateSuspended, Pool: "p", SnapshotURI: "sandboxes/s1/snap-old",
	})
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.0.0.1", State: resumeapi.WorkerBusy, SID: "s1"})

	var gotSuspend bool
	srv := stubSuspendWorker(t, "sandboxes/s1/snap-new", &gotSuspend)
	s := NewSuspender(kv, clientForStub(srv), nil, SuspendOptions{})

	if err := s.SuspendForTerminate(ctx, "s1", "w1", "10.0.0.1", "p"); err != nil {
		t.Fatal(err)
	}
	if gotSuspend {
		t.Fatal("must NOT re-checkpoint a session that's already Suspended")
	}
	e, _ := kv.GetSession(ctx, "s1")
	if e.State != resumeapi.StateSuspended || e.SnapshotURI != "sandboxes/s1/snap-old" {
		t.Fatalf("session should be untouched: %+v", e)
	}
	// worker still removed (pod is dying regardless)
	if _, err := kv.GetWorker(ctx, "w1"); err == nil {
		t.Fatal("expected worker entry removed even when session already suspended")
	}
}

func TestSweepSkipsWhenPolicyNone(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	now := int64(1_000_000_000_000)
	// Due in the index (idle past timeout), but the policy says "none" -> the
	// sweeper's action re-check must skip it.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, Pool: "p", WorkerPod: "w1", WorkerPodIP: "10.0.0.1",
		LastActiveAt: now - 100_000, IdleTimeoutSeconds: 30,
	})
	srv := stubSuspendWorker(t, "x", new(bool))
	policy := func(context.Context, string) (IdlePolicy, error) {
		return IdlePolicy{TimeoutSeconds: 30, Action: "none"}, nil
	}
	s := NewSuspender(kv, clientForStub(srv), policy, SuspendOptions{Now: fixedNow(now)})
	n, _ := s.SweepOnce(ctx)
	if n != 0 {
		t.Fatalf("policy none should not suspend: n=%d", n)
	}
}

// countingMirror records Mirror/Delete calls by resulting state.
type countingMirror struct {
	states  []string // e.State at each Mirror call
	deletes int
}

func (m *countingMirror) Mirror(_ context.Context, e *resumeapi.SessionEntry) {
	m.states = append(m.states, e.State)
}
func (m *countingMirror) Delete(_ context.Context, _ string) { m.deletes++ }

func TestMirrorFiresOnlyOnSuspended(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	now := int64(1_000_000_000_000)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, Pool: "p",
		WorkerPod: "w1", WorkerPodIP: "10.0.0.1", Image: "redis:7-alpine",
		LastActiveAt: now - 100_000, IdleTimeoutSeconds: 30,
	})
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.0.0.1", State: resumeapi.WorkerBusy, SID: "s1"})

	m := &countingMirror{}
	srv := stubSuspendWorker(t, "sandboxes/s1/snap-1", new(bool))
	s := NewSuspender(kv, clientForStub(srv), func(context.Context, string) (IdlePolicy, error) {
		return IdlePolicy{TimeoutSeconds: 30, Action: "suspend"}, nil
	}, SuspendOptions{Now: fixedNow(now)}).WithMirror(m)

	if _, err := s.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// suspendOne does two casSessions: Suspending then Suspended. Only Suspended
	// must mirror.
	if len(m.states) != 1 || m.states[0] != resumeapi.StateSuspended {
		t.Fatalf("mirror should fire once, on Suspended; got %v", m.states)
	}
}

func TestMirrorDeleteOnReset(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	now := int64(1_000_000_000_000)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, Pool: "p",
		WorkerPod: "w1", WorkerPodIP: "10.0.0.1", LastActiveAt: now - 100_000, IdleTimeoutSeconds: 30,
	})
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.0.0.1", State: resumeapi.WorkerBusy, SID: "s1"})
	m := &countingMirror{}
	// reset drives /reset on the worker, not /suspend — serve 200 for it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	s := NewSuspender(kv, clientForStub(srv), func(context.Context, string) (IdlePolicy, error) {
		return IdlePolicy{TimeoutSeconds: 30, Action: "reset"}, nil
	}, SuspendOptions{Now: fixedNow(now)}).WithMirror(m)
	if _, err := s.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if m.deletes != 1 {
		t.Fatalf("reset should Delete the durable record once; got %d", m.deletes)
	}
}

// --- #1b: SuspendNow three-way outcome (transitional vs satisfied vs checkpoint) ---

// TestSuspendNowTransientOnResuming: a session stuck Resuming must return
// ErrSuspendTransient (NOT nil) so the reconciler does not advance the watermark and
// falsely report "suspend completed" for a session that was never checkpointed.
func TestSuspendNowTransientOnResuming(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "sr", State: resumeapi.StateResuming, Pool: "p", WorkerPodIP: "10.0.0.9",
	})
	var gotSuspend bool
	srv := stubSuspendWorker(t, "unused", &gotSuspend)
	s := NewSuspender(kv, clientForStub(srv), nil, SuspendOptions{})

	err := s.SuspendNow(ctx, "sr")
	if err != ErrSuspendTransient {
		t.Fatalf("expected ErrSuspendTransient for Resuming, got %v", err)
	}
	if gotSuspend {
		t.Fatal("must NOT call worker /suspend for a Resuming session")
	}
	// Also Suspending is transitional.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{SID: "ss", State: resumeapi.StateSuspending, Pool: "p", WorkerPodIP: "10.0.0.9"})
	if err := s.SuspendNow(ctx, "ss"); err != ErrSuspendTransient {
		t.Fatalf("expected ErrSuspendTransient for Suspending, got %v", err)
	}
}

// TestSuspendNowSatisfiedStates: genuinely-satisfied states return nil (the request's
// intent already holds), so the reconciler advances the watermark.
func TestSuspendNowSatisfiedStates(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	var gotSuspend bool
	srv := stubSuspendWorker(t, "unused", &gotSuspend)
	s := NewSuspender(kv, clientForStub(srv), nil, SuspendOptions{})

	// Already Suspended -> nil.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{SID: "sd", State: resumeapi.StateSuspended, Pool: "p", SnapshotURI: "sandboxes/sd/snap-1"})
	if err := s.SuspendNow(ctx, "sd"); err != nil {
		t.Fatalf("Suspended should be nil (satisfied), got %v", err)
	}
	// Absent -> nil.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{SID: "sa", State: resumeapi.StateAbsent, Pool: "p"})
	if err := s.SuspendNow(ctx, "sa"); err != nil {
		t.Fatalf("Absent should be nil, got %v", err)
	}
	// Not-found in KV -> nil.
	if err := s.SuspendNow(ctx, "nope"); err != nil {
		t.Fatalf("missing session should be nil, got %v", err)
	}
	if gotSuspend {
		t.Fatal("no worker /suspend should fire for satisfied/absent states")
	}
}
