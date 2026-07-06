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

func stubCheckpointWorker(t *testing.T, snap string, got *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/checkpoint") {
			*got = true
			var req sbxapi.CheckpointRequest
			json.NewDecoder(r.Body).Decode(&req)
			if !req.LeaveRunning {
				t.Error("periodic checkpoint must set leaveRunning=true")
			}
			json.NewEncoder(w).Encode(sbxapi.CheckpointResponse{SandboxID: req.SandboxID, Snapshot: snap})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckpointerRunsWhenIntervalElapsed(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	now := int64(1_000_000_000_000)
	// Running session, last checkpointed 100s ago.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, Pool: "p", WorkerPod: "w1", WorkerPodIP: "10.0.0.1",
		LastCheckpointAt: now - 100_000,
	})
	var got bool
	srv := stubCheckpointWorker(t, "sandboxes/s1/snap-new", &got)
	pol := func(context.Context, string) (CheckpointPolicy, error) {
		return CheckpointPolicy{IntervalSeconds: 30}, nil // 30s < 100s -> checkpoint
	}
	cp := NewCheckpointer(kv, clientForStub(srv), pol, time.Second, fixedNow(now))

	n, err := cp.SweepOnce(ctx)
	if err != nil || n != 1 || !got {
		t.Fatalf("want 1 checkpoint, got n=%d got=%v err=%v", n, got, err)
	}
	e, _ := kv.GetSession(ctx, "s1")
	if e.State != resumeapi.StateRunning {
		t.Fatalf("session must stay Running, got %s", e.State)
	}
	if e.SnapshotURI != "sandboxes/s1/snap-new" || e.LastCheckpointAt != now {
		t.Fatalf("snapshot/lastCheckpointAt not advanced: %+v", e)
	}
}

func TestCheckpointerSkipsWhenNotElapsedOrDisabled(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	now := int64(1_000_000_000_000)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, Pool: "p", WorkerPod: "w1", WorkerPodIP: "10.0.0.1",
		LastCheckpointAt: now - 5_000, // 5s ago
	})
	var got bool
	srv := stubCheckpointWorker(t, "x", &got)

	// Not elapsed (interval 30s > 5s).
	cp := NewCheckpointer(kv, clientForStub(srv), func(context.Context, string) (CheckpointPolicy, error) {
		return CheckpointPolicy{IntervalSeconds: 30}, nil
	}, time.Second, fixedNow(now))
	if n, _ := cp.SweepOnce(ctx); n != 0 || got {
		t.Fatalf("should not checkpoint (not elapsed): n=%d got=%v", n, got)
	}

	// Disabled (interval 0).
	cp2 := NewCheckpointer(kv, clientForStub(srv), func(context.Context, string) (CheckpointPolicy, error) {
		return CheckpointPolicy{IntervalSeconds: 0}, nil
	}, time.Second, fixedNow(now))
	if n, _ := cp2.SweepOnce(ctx); n != 0 || got {
		t.Fatalf("should not checkpoint (disabled): n=%d got=%v", n, got)
	}
}
