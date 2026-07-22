package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jicowan/aio-sandbox/shared/resumeapi"
	"github.com/jicowan/aio-sandbox/shared/sbxapi"
)

// fakeKV is an in-memory KVReader for router tests.
type fakeKV struct {
	mu       sync.Mutex
	sessions map[string]*resumeapi.SessionEntry
	workers  map[string]*resumeapi.WorkerEntry
	stamps   map[string]int64
}

func newFakeKV() *fakeKV {
	return &fakeKV{
		sessions: map[string]*resumeapi.SessionEntry{},
		workers:  map[string]*resumeapi.WorkerEntry{},
		stamps:   map[string]int64{},
	}
}
func (f *fakeKV) GetSession(_ context.Context, sid string) (*resumeapi.SessionEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.sessions[sid]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return e, nil
}
func (f *fakeKV) StampActive(_ context.Context, sid string, ms int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stamps[sid] = ms
	return nil
}
func (f *fakeKV) GetWorker(_ context.Context, pod string) (*resumeapi.WorkerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.workers[pod]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return w, nil
}

// liveWorker binds a busy worker entry to a session so the fast-path liveness
// gate (workerLive) passes. Tests that want the fast path must register both a
// Running session (with WorkerPod set) and its live worker.
func (f *fakeKV) liveWorker(pod, sid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workers[pod] = &resumeapi.WorkerEntry{Pod: pod, State: resumeapi.WorkerBusy, SID: sid}
}

// fakeResumer records calls and returns a fixed IP.
type fakeResumer struct {
	mu    sync.Mutex
	calls int
	ip    string
	err   error
}

func (r *fakeResumer) Resume(_ context.Context, _, _, _, _ string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.ip, r.err
}

// workerHost extracts host:port -> host, and returns the port so the router's
// WorkerPort matches the stub listener.
func splitHostPort(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()
	u := strings.TrimPrefix(srv.URL, "http://")
	host, portStr, err := net.SplitHostPort(u)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

func req(sid string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/tools/call", nil)
	r.Header.Set("X-Session-ID", sid)
	return r
}

func TestFastPathProxies(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tools/call" {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.Write([]byte("hello from worker"))
	}))
	defer worker.Close()
	host, port := splitHostPort(t, worker)

	kv := newFakeKV()
	kv.sessions["s1"] = &resumeapi.SessionEntry{SID: "s1", State: resumeapi.StateRunning, WorkerPod: "w1", WorkerPodIP: host}
	kv.liveWorker("w1", "s1")
	res := &fakeResumer{}
	rt := New(kv, res, NewHeaderResolver(), http.DefaultTransport, Options{WorkerPort: port})

	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req("s1"))
	if rr.Code != 200 || rr.Body.String() != "hello from worker" {
		t.Fatalf("fast path failed: %d %q", rr.Code, rr.Body.String())
	}
	if res.calls != 0 {
		t.Fatalf("resume should not be called on fast path, got %d", res.calls)
	}
	if kv.stamps["s1"] == 0 {
		t.Fatalf("expected activity stamp")
	}
}

func TestFastPathUsesSessionPort(t *testing.T) {
	// Worker serves on the stub's port; session declares it as the exposed host
	// port. Router must target the session port, not the default WorkerPort.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("workload"))
	}))
	defer worker.Close()
	host, port := splitHostPort(t, worker)

	kv := newFakeKV()
	kv.sessions["s1"] = &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, WorkerPod: "w1", WorkerPodIP: host,
		Ports: []sbxapi.PortMap{{Container: port, Host: port}},
	}
	kv.liveWorker("w1", "s1")
	// WorkerPort deliberately WRONG (would 404/refuse); session port must win.
	rt := New(kv, &fakeResumer{}, NewHeaderResolver(), http.DefaultTransport, Options{WorkerPort: 1})

	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req("s1"))
	if rr.Code != 200 || rr.Body.String() != "workload" {
		t.Fatalf("session-port routing failed: %d %q", rr.Code, rr.Body.String())
	}
}

// TestStaleWorkerFallsThroughToResume: session is Running with a WorkerPodIP,
// but the worker KV entry is gone (pod crashed/pruned). The fast-path liveness
// gate must fail and the router must resume instead of proxying to the dead IP.
func TestStaleWorkerFallsThroughToResume(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("resumed-fresh"))
	}))
	defer worker.Close()
	host, port := splitHostPort(t, worker)

	kv := newFakeKV()
	// Running record points at a now-dead IP; NO worker entry registered.
	kv.sessions["s1"] = &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, WorkerPod: "w-dead", WorkerPodIP: "10.255.255.1",
	}
	res := &fakeResumer{ip: host}
	rt := New(kv, res, NewHeaderResolver(), http.DefaultTransport, Options{WorkerPort: port})

	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req("s1"))
	if rr.Code != 200 || rr.Body.String() != "resumed-fresh" {
		t.Fatalf("stale-worker fall-through failed: %d %q", rr.Code, rr.Body.String())
	}
	if res.calls != 1 {
		t.Fatalf("want 1 resume call on stale worker, got %d", res.calls)
	}
}

// deadIP is an unreachable TEST-NET-3 address; the failing transport short-
// circuits it so we don't depend on real dial timeouts.
const deadIP = "203.0.113.1"

// failFor is a RoundTripper that returns a connection error for requests to a
// specific host and delegates everything else to a healthy backend transport.
type failFor struct {
	host string
	ok   http.RoundTripper
}

func (f *failFor) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() == f.host {
		return nil, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	}
	return f.ok.RoundTrip(r)
}

// TestFastPathUpstreamErrorFallsThrough: the worker looks live in KV (crash
// inside the prune window) but the connection fails with nothing written yet.
// The reactive safety net must fall through to resume once and re-send the
// buffered body, rather than returning 502.
func TestFastPathUpstreamErrorFallsThrough(t *testing.T) {
	// Healthy resume target that echoes the request body (proves body survived).
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Write([]byte("resumed:" + string(b)))
	}))
	defer good.Close()
	goodHost, port := splitHostPort(t, good)

	kv := newFakeKV()
	// Session Running on the dead IP, and the worker STILL looks live in KV.
	kv.sessions["s1"] = &resumeapi.SessionEntry{
		SID: "s1", State: resumeapi.StateRunning, WorkerPod: "w1", WorkerPodIP: deadIP,
	}
	kv.liveWorker("w1", "s1")
	res := &fakeResumer{ip: goodHost} // resume returns the healthy worker IP

	// Transport fails for the dead IP (fast path), succeeds for the good IP
	// (resume). Both use the same port, so only the IP differentiates them.
	tr := &failFor{host: deadIP, ok: http.DefaultTransport}
	rt := New(kv, res, NewHeaderResolver(), tr, Options{WorkerPort: port})

	body := `{"tool":"echo","arg":"narwhal"}`
	r := httptest.NewRequest(http.MethodPost, "/tools/call", strings.NewReader(body))
	r.Header.Set("X-Session-ID", "s1")
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, r)

	if res.calls != 1 {
		t.Fatalf("want 1 resume call after upstream failure, got %d", res.calls)
	}
	if rr.Code != 200 || rr.Body.String() != "resumed:"+body {
		t.Fatalf("fall-through/resume failed: %d %q", rr.Code, rr.Body.String())
	}
}

func TestMissTriggersResume(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("resumed"))
	}))
	defer worker.Close()
	host, port := splitHostPort(t, worker)

	kv := newFakeKV() // no session -> miss
	res := &fakeResumer{ip: host}
	rt := New(kv, res, NewHeaderResolver(), http.DefaultTransport, Options{WorkerPort: port})

	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req("s2"))
	if rr.Code != 200 || rr.Body.String() != "resumed" {
		t.Fatalf("miss path failed: %d %q", rr.Code, rr.Body.String())
	}
	if res.calls != 1 {
		t.Fatalf("want 1 resume call, got %d", res.calls)
	}
}

func TestNoCapacityReturns503(t *testing.T) {
	kv := newFakeKV()
	res := &fakeResumer{err: ErrNoCapacity}
	rt := New(kv, res, NewHeaderResolver(), http.DefaultTransport, Options{WorkerPort: 8090})
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req("s3"))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatalf("want Retry-After header")
	}
}

func TestUnauthenticated(t *testing.T) {
	rt := New(newFakeKV(), &fakeResumer{}, NewHeaderResolver(), http.DefaultTransport, Options{})
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil)) // no header
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

// TestStreamingFlushes verifies the proxy streams chunks as they are produced
// (FlushInterval -1) rather than buffering the whole response.
func TestStreamingFlushes(t *testing.T) {
	release := make(chan struct{})
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("no flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "chunk1\n")
		fl.Flush()
		<-release // hold the response open
		io.WriteString(w, "chunk2\n")
		fl.Flush()
	}))
	defer worker.Close()
	host, port := splitHostPort(t, worker)

	kv := newFakeKV()
	kv.sessions["s1"] = &resumeapi.SessionEntry{SID: "s1", State: resumeapi.StateRunning, WorkerPod: "w1", WorkerPodIP: host}
	kv.liveWorker("w1", "s1")
	rt := New(kv, &fakeResumer{}, NewHeaderResolver(), http.DefaultTransport, Options{WorkerPort: port})

	srv := httptest.NewServer(rt)
	defer srv.Close()

	client := srv.Client()
	r, _ := http.NewRequest(http.MethodGet, srv.URL+"/stream", nil)
	r.Header.Set("X-Session-ID", "s1")
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read the first chunk BEFORE releasing the second — proves streaming.
	buf := make([]byte, 7)
	done := make(chan string, 1)
	go func() {
		n, _ := io.ReadFull(resp.Body, buf)
		done <- string(buf[:n])
	}()
	select {
	case got := <-done:
		if got != "chunk1\n" {
			t.Fatalf("first chunk = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first streamed chunk (buffering?)")
	}
	close(release)
}

func TestWarmEndpoint(t *testing.T) {
	// /_warm ensures a session is Running via resume and returns 204 — no proxy.
	kv := newFakeKV()
	res := &fakeResumer{ip: "10.0.0.9"}
	rt := New(kv, res, NewHeaderResolver(), http.DefaultTransport, Options{WorkerPort: 8090})

	r := httptest.NewRequest(http.MethodPost, "/_warm", nil)
	r.Header.Set("X-Session-ID", "s1")
	r.Header.Set("X-Session-Pool", "aio-pool")
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, r)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d (%s)", rr.Code, rr.Body.String())
	}
	if res.calls != 1 {
		t.Fatalf("warm should trigger exactly one resume, got %d", res.calls)
	}
}

func TestWarmNoCapacity(t *testing.T) {
	kv := newFakeKV()
	res := &fakeResumer{err: ErrNoCapacity}
	rt := New(kv, res, NewHeaderResolver(), http.DefaultTransport, Options{})
	r := httptest.NewRequest(http.MethodPost, "/_warm", nil)
	r.Header.Set("X-Session-ID", "s1")
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, r)
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("want 503+Retry-After, got %d", rr.Code)
	}
}

func TestHeaderResolverParsesAppHint(t *testing.T) {
	h := NewHeaderResolver()
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("X-Session-ID", "s1")
	r.Header.Set("X-Session-Pool", "generic-pool")
	r.Header.Set("X-Session-App", "aio-app")
	id, err := h.Resolve(r)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id.PoolHint != "generic-pool" || id.AppHint != "aio-app" {
		t.Fatalf("want pool=generic-pool app=aio-app, got pool=%q app=%q", id.PoolHint, id.AppHint)
	}
	// Absent app header => empty AppHint (dedicated-pool path unaffected).
	r2 := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r2.Header.Set("X-Session-ID", "s2")
	id2, _ := h.Resolve(r2)
	if id2.AppHint != "" {
		t.Fatalf("want empty AppHint when header absent, got %q", id2.AppHint)
	}
}
