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
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

func handlerKV(t *testing.T) (*assign.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	return assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()})), mr
}

func postResume(t *testing.T, h http.Handler, sid string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(resumeapi.ResumeRequest{SID: sid, Subject: "alice"})
	req := httptest.NewRequest(http.MethodPost, "/resume", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHandlerNoCapacity503(t *testing.T) {
	kv, _ := handlerKV(t)
	// no idle workers
	wf := New(kv, lookupImage("x"), clientForStub(stubWorker(t, 1)), planTemplate("p", "tmpl"),
		Options{ResumeDeadline: time.Second, PollInterval: 5 * time.Millisecond})
	rr := postResume(t, NewHandler(wf), "s1")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d (%s)", rr.Code, rr.Body.String())
	}
	var e resumeapi.ResumeError
	json.Unmarshal(rr.Body.Bytes(), &e)
	if e.RetryAfterSeconds == 0 {
		t.Fatalf("want retryAfterSeconds set, got %+v", e)
	}
}

func TestHandlerColdStart200(t *testing.T) {
	ctx := context.Background()
	kv, _ := handlerKV(t)
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "w1", Pool: "p", PodIP: "10.0.0.7", State: resumeapi.WorkerIdle})
	wf := New(kv, lookupImage("python:3.12"), clientForStub(stubWorker(t, 1)), planTemplate("p", "tmpl"),
		Options{ResumeDeadline: 2 * time.Second, PollInterval: 5 * time.Millisecond})
	rr := postResume(t, NewHandler(wf), "s1")
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	var resp resumeapi.ResumeResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.WorkerPodIP != "10.0.0.7" || resp.State != resumeapi.StateRunning {
		t.Fatalf("unexpected: %+v", resp)
	}
}

func TestHandlerBadRequest(t *testing.T) {
	kv, _ := handlerKV(t)
	wf := New(kv, lookupImage("x"), clientForStub(stubWorker(t, 1)), planTemplate("p", "tmpl"), Options{})
	req := httptest.NewRequest(http.MethodPost, "/resume", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	NewHandler(wf).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}
