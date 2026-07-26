package resume

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/sandboxdclient"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
	"github.com/jicowan/aio-sandbox/shared/sbxapi"
)

func testKV(t *testing.T) *assign.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	return assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
}

// stubWorker serves /run (200) and /status (ready after N polls).
func stubWorker(t *testing.T, readyAfter int) *httptest.Server {
	t.Helper()
	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/run"):
			var req sbxapi.RunRequest
			json.NewDecoder(r.Body).Decode(&req)
			json.NewEncoder(w).Encode(sbxapi.RunResponse{SandboxID: req.SandboxID, Status: "running", Image: req.Image})
		case strings.HasPrefix(r.URL.Path, "/status"):
			polls++
			json.NewEncoder(w).Encode(sbxapi.StatusResponse{Status: "running", Ready: polls >= readyAfter})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// clientForStub returns a factory that always dials the stub server (ignoring IP).
func clientForStub(srv *httptest.Server) WorkerClientFactory {
	return func(string) *sandboxdclient.Client {
		return sandboxdclient.NewForBaseURL(srv.URL, srv.Client())
	}
}

func planTemplate(pool, tmpl string) PlanFunc {
	return func(context.Context, string, string, string, string) (*SessionPlan, error) {
		return &SessionPlan{Pool: pool, TemplateName: tmpl}, nil
	}
}

func lookupImage(img string) TemplateLookup {
	return func(context.Context, string) (*TemplateSpec, error) {
		return &TemplateSpec{Image: img, Ports: []sbxapi.PortMap{{Container: 8080}}}, nil
	}
}

func TestResumeColdStart(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	// one idle worker in pool p
	if err := kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.0.0.1", State: resumeapi.WorkerIdle}); err != nil {
		t.Fatal(err)
	}
	srv := stubWorker(t, 2)
	wf := New(kv, lookupImage("python:3.12"), clientForStub(srv), planTemplate("p", "tmpl"),
		Options{ResumeDeadline: 3 * time.Second, PollInterval: 5 * time.Millisecond})

	ip, err := wf.Resume(ctx, "s1", "alice", "", "")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if ip != "10.0.0.1" {
		t.Fatalf("ip = %q", ip)
	}
	e, _ := kv.GetSession(ctx, "s1")
	if e.State != resumeapi.StateRunning || e.Image != "python:3.12" || e.WorkerPodIP != "10.0.0.1" {
		t.Fatalf("session entry wrong: %+v", e)
	}
	// worker should be busy + out of idle set
	idle, _ := kv.IdleWorkers(ctx, "p")
	if len(idle) != 0 {
		t.Fatalf("worker still idle: %v", idle)
	}
}

func TestResumeNoCapacity(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	srv := stubWorker(t, 1)
	wf := New(kv, lookupImage("x"), clientForStub(srv), planTemplate("p", "tmpl"), Options{})
	_, err := wf.Resume(ctx, "s1", "alice", "", "")
	if err != assign.ErrNoCapacity {
		t.Fatalf("want ErrNoCapacity, got %v", err)
	}
}

func TestResumeReleasesWorkerOnRunFailure(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.0.0.1", State: resumeapi.WorkerIdle})

	// worker that fails /run
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte(`{"error":"pull failed"}`))
	}))
	t.Cleanup(srv.Close)

	wf := New(kv, lookupImage("x"), clientForStub(srv), planTemplate("p", "tmpl"),
		Options{ResumeDeadline: time.Second, PollInterval: 5 * time.Millisecond})
	if _, err := wf.Resume(ctx, "s1", "alice", "", ""); err == nil {
		t.Fatal("expected error")
	}
	// worker released back to idle
	idle, _ := kv.IdleWorkers(ctx, "p")
	if len(idle) != 1 || idle[0] != "w1" {
		t.Fatalf("worker not released: %v", idle)
	}
}

// TestResumeRollsBackToSuspendedOnRestoreFailure: a Suspended session with a
// snapshotURI whose /restore FAILS must be rolled BACK to Suspended (not left in
// Resuming). Otherwise the next resume skips the restore branch (which keys on
// State==Suspended) and falls through to the cold-start plan — which for a
// snapshot-fork on a generic pool errors "pool is generic, nothing to run". This is
// the root-cause fix for the snapshot-fork re-resume-after-failed-restore bug.
func TestResumeRollsBackToSuspendedOnRestoreFailure(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.0.0.1", State: resumeapi.WorkerIdle})
	// A suspended snapshot-fork-style entry: has a snapshotURI, no appRef/image plan.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "fork1", State: resumeapi.StateSuspended, Pool: "p",
		Image: "img:1", SnapshotURI: "sandboxes/fork1/snap-1",
	})
	// Worker whose /restore fails (e.g. transient image pull / S3 error).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte(`{"error":"pull failed"}`))
	}))
	t.Cleanup(srv.Close)

	wf := New(kv, lookupImage("x"), clientForStub(srv), planTemplate("p", "tmpl"),
		Options{ResumeDeadline: time.Second, PollInterval: 5 * time.Millisecond})
	if _, err := wf.Resume(ctx, "fork1", "alice", "", ""); err == nil {
		t.Fatal("expected restore failure")
	}
	// The entry must be back to Suspended with its snapshotURI, worker cleared — so the
	// next request retries the restore instead of stalling in Resuming.
	e, err := kv.GetSession(ctx, "fork1")
	if err != nil {
		t.Fatal(err)
	}
	if e.State != resumeapi.StateSuspended {
		t.Fatalf("expected rollback to Suspended, got %q", e.State)
	}
	if e.SnapshotURI == "" || e.WorkerPodIP != "" {
		t.Fatalf("rollback must keep snapshotURI + clear worker: %+v", e)
	}
	// worker released back to idle too
	idle, _ := kv.IdleWorkers(ctx, "p")
	if len(idle) != 1 || idle[0] != "w1" {
		t.Fatalf("worker not released: %v", idle)
	}
}

// stubWorkerRestore serves /restore + /status; records whether /restore (not
// /run) was called.
func stubWorkerRestore(t *testing.T, gotRestore *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/restore"):
			*gotRestore = true
			var req sbxapi.RestoreRequest
			json.NewDecoder(r.Body).Decode(&req)
			json.NewEncoder(w).Encode(sbxapi.RestoreResponse{SandboxID: req.SandboxID, Status: "running", RestoredFrom: req.Snapshot})
		case strings.HasPrefix(r.URL.Path, "/run"):
			t.Error("expected /restore, got /run")
			w.WriteHeader(500)
		case strings.HasPrefix(r.URL.Path, "/status"):
			json.NewEncoder(w).Encode(sbxapi.StatusResponse{Status: "running", Ready: true})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResumeFromSnapshot(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	// a SUSPENDED session with a recorded checkpoint + image + pool
	if err := kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateSuspended, Pool: "p",
		Image: "redis:7-alpine", SnapshotURI: "sandboxes/s1/snap-123",
		Ports: []sbxapi.PortMap{{Container: 6379, Host: 6379}},
	}); err != nil {
		t.Fatal(err)
	}
	// idle worker to restore ONTO (a different worker than the original)
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w2", Pool: "p", PodIP: "10.0.0.2", State: resumeapi.WorkerIdle})

	var gotRestore bool
	srv := stubWorkerRestore(t, &gotRestore)
	// planFor should NOT be consulted on the restore path; make it fail if called.
	plan := func(context.Context, string, string, string, string) (*SessionPlan, error) {
		return nil, fmt.Errorf("planFor must not be called on restore")
	}
	wf := New(kv, lookupImage("SHOULD-NOT-BE-USED"), clientForStub(srv), plan,
		Options{ResumeDeadline: 3 * time.Second, PollInterval: 5 * time.Millisecond})

	ip, err := wf.Resume(ctx, "s1", "alice", "", "")
	if err != nil {
		t.Fatalf("Resume(restore): %v", err)
	}
	if ip != "10.0.0.2" {
		t.Fatalf("restored on wrong worker: %q", ip)
	}
	if !gotRestore {
		t.Fatal("expected /restore to be called")
	}
	e, _ := kv.GetSession(ctx, "s1")
	if e.State != resumeapi.StateRunning || e.WorkerPodIP != "10.0.0.2" || e.Image != "redis:7-alpine" {
		t.Fatalf("post-restore entry wrong: %+v", e)
	}
}

func TestResumeIdempotentWhenRunning(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	// A genuinely-live Running session: the worker entry exists, busy-bound to s1.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{SID: "s1", State: resumeapi.StateRunning, WorkerPod: "w1", WorkerPodIP: "10.9.9.9"})
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.9.9.9", State: resumeapi.WorkerBusy, SID: "s1"})
	wf := New(kv, lookupImage("x"), clientForStub(stubWorker(t, 1)), planTemplate("p", "tmpl"), Options{})
	ip, err := wf.Resume(ctx, "s1", "alice", "", "")
	if err != nil || ip != "10.9.9.9" {
		t.Fatalf("idempotent resume failed: ip=%q err=%v", ip, err)
	}
}

// TestFencingStaleRunningRestores verifies that a RUNNING entry whose worker is
// gone (crash: no WorkerEntry) is fenced — we do NOT route to the dead IP; we
// restore from the last checkpoint onto a fresh worker instead.
func TestFencingStaleRunningRestores(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	// RUNNING pointing at a dead worker (no WorkerEntry for w-dead), but a
	// checkpoint exists (e.g. from a periodic checkpoint before the crash).
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, Pool: "p",
		WorkerPod: "w-dead", WorkerPodIP: "10.0.0.9",
		Image: "redis:7-alpine", SnapshotURI: "sandboxes/s1/snap-1",
		Ports: []sbxapi.PortMap{{Container: 6379, Host: 6379}},
	})
	// a fresh idle worker to restore onto
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w-new", Pool: "p", PodIP: "10.0.0.2", State: resumeapi.WorkerIdle})

	var gotRestore bool
	srv := stubWorkerRestore(t, &gotRestore)
	wf := New(kv, lookupImage("unused"), clientForStub(srv),
		func(context.Context, string, string, string, string) (*SessionPlan, error) {
			return nil, fmt.Errorf("planFor must not be called; should restore")
		}, Options{ResumeDeadline: 3 * time.Second, PollInterval: 5 * time.Millisecond})

	ip, err := wf.Resume(ctx, "s1", "alice", "", "")
	if err != nil {
		t.Fatalf("fenced resume: %v", err)
	}
	if ip != "10.0.0.2" || !gotRestore {
		t.Fatalf("expected restore onto fresh worker 10.0.0.2, got ip=%q restore=%v", ip, gotRestore)
	}
}

func TestBackpressureCapReturnsNoCapacity(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.0.0.1", State: resumeapi.WorkerIdle})
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w2", Pool: "p", PodIP: "10.0.0.2", State: resumeapi.WorkerIdle})

	inWait := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/run") {
			json.NewEncoder(w).Encode(sbxapi.RunResponse{Status: "running"})
			return
		}
		// /status: signal we're holding the slot, then block until released.
		select {
		case inWait <- struct{}{}:
		default:
		}
		<-release
		json.NewEncoder(w).Encode(sbxapi.StatusResponse{Status: "running", Ready: true})
	}))
	t.Cleanup(srv.Close)

	// Holder: long deadline so it keeps the single slot for the whole test.
	holder := New(kv, lookupImage("img"), clientForStub(srv), planTemplate("p", "tmpl"),
		Options{ResumeDeadline: 10 * time.Second, PollInterval: 5 * time.Millisecond, MaxConcurrentResumes: 1})
	go holder.Resume(ctx, "s1", "alice", "", "")
	<-inWait // holder is now blocked in WaitReady, slot held

	// Contender shares the SAME workflow (same semaphore). Give it a short caller
	// context so it fails fast when it can't acquire the slot (resume derives its
	// deadline from the passed ctx).
	cctx, ccancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer ccancel()
	_, err := holder.Resume(cctx, "s2", "bob", "", "")
	if err != assign.ErrNoCapacity {
		t.Fatalf("want ErrNoCapacity from backpressure, got %v", err)
	}
	close(release)
}
