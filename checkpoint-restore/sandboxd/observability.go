package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// ---- request IDs + access logging ----

type ctxKey string

const reqIDKey ctxKey = "reqID"

func newReqID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// withRequestLog assigns a request id, logs method/path/status/duration, and
// recovers panics (so one bad request can't crash the worker).
func withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := newReqID()
		r = r.WithContext(context.WithValue(r.Context(), reqIDKey, rid))
		sw := &statusWriter{ResponseWriter: w, code: 200}
		t0 := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[req %s] PANIC %s %s: %v", rid, r.Method, r.URL.Path, rec)
				metrics.inc("panics")
				if !sw.wrote {
					writeErr(sw, 500, fmt.Sprintf("internal error: %v", rec))
				}
			}
			log.Printf("[req %s] %s %s -> %d (%s)", rid, r.Method, r.URL.Path, sw.code, time.Since(t0))
			metrics.inc("requests")
		}()
		next.ServeHTTP(sw, r)
	})
}

// reqLogger returns a per-operation logger that prefixes the request id + op + id.
func reqLogger(r *http.Request, op, id string) func(string, ...any) {
	rid, _ := r.Context().Value(reqIDKey).(string)
	return func(format string, args ...any) {
		log.Printf("[req %s][%s %s] %s", rid, op, id, fmt.Sprintf(format, args...))
	}
}

type statusWriter struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (s *statusWriter) WriteHeader(c int) { s.code = c; s.wrote = true; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// ---- lightweight metrics ----

type counters struct {
	mu sync.Mutex
	m  map[string]int64
}

var metrics = &counters{m: map[string]int64{}}

func (c *counters) inc(k string) {
	c.mu.Lock()
	c.m[k]++
	c.mu.Unlock()
}
func (c *counters) snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int64{}
	for k, v := range c.m {
		out[k] = v
	}
	return out
}
