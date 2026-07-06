package resume

import (
	"context"
	"encoding/json"
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

func planTemplate(pool, tmpl string) func(context.Context, string, string) (*SessionPlan, error) {
	return func(context.Context, string, string) (*SessionPlan, error) {
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

	ip, err := wf.Resume(ctx, "s1", "alice")
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
	_, err := wf.Resume(ctx, "s1", "alice")
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
	if _, err := wf.Resume(ctx, "s1", "alice"); err == nil {
		t.Fatal("expected error")
	}
	// worker released back to idle
	idle, _ := kv.IdleWorkers(ctx, "p")
	if len(idle) != 1 || idle[0] != "w1" {
		t.Fatalf("worker not released: %v", idle)
	}
}

func TestResumeIdempotentWhenRunning(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{SID: "s1", State: resumeapi.StateRunning, WorkerPodIP: "10.9.9.9"})
	wf := New(kv, lookupImage("x"), clientForStub(stubWorker(t, 1)), planTemplate("p", "tmpl"), Options{})
	ip, err := wf.Resume(ctx, "s1", "alice")
	if err != nil || ip != "10.9.9.9" {
		t.Fatalf("idempotent resume failed: ip=%q err=%v", ip, err)
	}
}
