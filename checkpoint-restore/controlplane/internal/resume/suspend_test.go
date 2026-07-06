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
	// active 5s ago, timeout 30s -> NOT idle
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, Pool: "p", WorkerPod: "w1", WorkerPodIP: "10.0.0.1",
		LastActiveAt: now - 5_000,
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

func TestSweepSkipsWhenPolicyNone(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	now := int64(1_000_000_000_000)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, Pool: "p", WorkerPod: "w1", WorkerPodIP: "10.0.0.1",
		LastActiveAt: now - 100_000,
	})
	srv := stubSuspendWorker(t, "x", new(bool))
	policy := func(context.Context, string) (IdlePolicy, error) {
		return IdlePolicy{TimeoutSeconds: 0, Action: "none"}, nil
	}
	s := NewSuspender(kv, clientForStub(srv), policy, SuspendOptions{Now: fixedNow(now)})
	n, _ := s.SweepOnce(ctx)
	if n != 0 {
		t.Fatalf("policy none should not suspend: n=%d", n)
	}
}
