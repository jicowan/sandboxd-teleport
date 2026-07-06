package sandboxdclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jicowan/aio-sandbox/shared/sbxapi"
)

// newStub spins an httptest server and returns a Client pointed at it. Because
// New() builds http://<host>:8090, we override baseURL directly for the test.
func newStub(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, hc: srv.Client()}
}

func TestRun(t *testing.T) {
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/run" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req sbxapi.RunRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Image != "python:3.12-slim" {
			t.Errorf("image = %q", req.Image)
		}
		json.NewEncoder(w).Encode(sbxapi.RunResponse{SandboxID: req.SandboxID, Status: "running", Image: req.Image})
	})
	out, err := c.Run(context.Background(), sbxapi.RunRequest{SandboxID: "s1", Image: "python:3.12-slim"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "running" || out.SandboxID != "s1" {
		t.Fatalf("unexpected: %+v", out)
	}
}

func TestWaitReady(t *testing.T) {
	var calls int
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		ready := calls >= 3 // not ready first two polls
		json.NewEncoder(w).Encode(sbxapi.StatusResponse{SandboxID: "s1", Status: "running", Ready: ready})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.WaitReady(ctx, "s1", 10*time.Millisecond); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if calls < 3 {
		t.Fatalf("want >=3 polls, got %d", calls)
	}
}

func TestWaitReadyTerminal(t *testing.T) {
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(sbxapi.StatusResponse{SandboxID: "s1", Status: "stopped"})
	})
	err := c.WaitReady(context.Background(), "s1", 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("want terminal error, got %v", err)
	}
}

func TestErrorStatus(t *testing.T) {
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte(`{"error":"pull failed"}`))
	})
	_, err := c.Run(context.Background(), sbxapi.RunRequest{Image: "bad"})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("want 502 error, got %v", err)
	}
}
