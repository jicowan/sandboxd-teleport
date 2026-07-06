package router

import (
	"context"
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
	stamps   map[string]int64
}

func newFakeKV() *fakeKV {
	return &fakeKV{sessions: map[string]*resumeapi.SessionEntry{}, stamps: map[string]int64{}}
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

// fakeResumer records calls and returns a fixed IP.
type fakeResumer struct {
	mu    sync.Mutex
	calls int
	ip    string
	err   error
}

func (r *fakeResumer) Resume(_ context.Context, _, _, _ string) (string, error) {
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
	kv.sessions["s1"] = &resumeapi.SessionEntry{SID: "s1", State: resumeapi.StateRunning, WorkerPodIP: host}
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
		SID: "s1", State: resumeapi.StateRunning, WorkerPodIP: host,
		Ports: []sbxapi.PortMap{{Container: port, Host: port}},
	}
	// WorkerPort deliberately WRONG (would 404/refuse); session port must win.
	rt := New(kv, &fakeResumer{}, NewHeaderResolver(), http.DefaultTransport, Options{WorkerPort: 1})

	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req("s1"))
	if rr.Code != 200 || rr.Body.String() != "workload" {
		t.Fatalf("session-port routing failed: %d %q", rr.Code, rr.Body.String())
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
	kv.sessions["s1"] = &resumeapi.SessionEntry{SID: "s1", State: resumeapi.StateRunning, WorkerPodIP: host}
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
