/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package router is the thin, session-aware data-plane proxy (PRD §5, TDD §5.2).
// Per request: resolve identity -> KV read -> if Running, stream-proxy to the
// worker (fast path); else singleflight a /resume call to the operator, then
// proxy. The router never assigns workers or writes assignment state (that is the
// operator's job, O2); it only READS the KV table and stamps lastActiveAt (O3).
package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// KVReader is the read-only slice of the assignment table the router needs.
// (assign.Client satisfies it; the router must not write.)
type KVReader interface {
	GetSession(ctx context.Context, sid string) (*resumeapi.SessionEntry, error)
	StampActive(ctx context.Context, sid string, unixMillis int64) error
}

// Resumer triggers a resume on a KV miss (implemented by ResumeClient).
type Resumer interface {
	Resume(ctx context.Context, sid, subject string) (string, error)
}

// Options configure the Router.
type Options struct {
	// WorkerPort is sandboxd's HTTP/data port on the worker pod (default 8090).
	WorkerPort int
	// ResumeDeadline bounds the time-to-first-byte / warm-up clock (O8). It does
	// NOT cap a streaming response; that is the transport's idle timeout. Default 15s.
	ResumeDeadline time.Duration
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// Router is the http.Handler for the data plane.
type Router struct {
	kv      KVReader
	resumer Resumer
	resolve Resolver
	proxy   *httputil.ReverseProxy
	single  singleflight.Group
	opts    Options
}

// New builds a Router. transport is the RoundTripper used to reach workers (P1.5
// supplies mTLS); nil uses http.DefaultTransport.
func New(kv KVReader, resumer Resumer, resolve Resolver, transport http.RoundTripper, opts Options) *Router {
	if opts.WorkerPort == 0 {
		opts.WorkerPort = 8090
	}
	if opts.ResumeDeadline == 0 {
		opts.ResumeDeadline = 15 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	rt := &Router{kv: kv, resumer: resumer, resolve: resolve, opts: opts}
	rt.proxy = &httputil.ReverseProxy{
		// Director set per-request via context (see proxyTo).
		Director: func(*http.Request) {},
		// FlushInterval -1: flush immediately so SSE / chunked agent output streams
		// token-by-token instead of buffering (PRD §5.5). Mandatory.
		FlushInterval: -1,
		Transport:     transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
	return rt
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := rt.resolve.Resolve(r)
	if err != nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	// Fast path: session already Running on a live worker.
	if e, err := rt.kv.GetSession(r.Context(), id.SID); err == nil &&
		e.State == resumeapi.StateRunning && e.WorkerPodIP != "" {
		rt.stamp(id.SID)       // O3 bracketing: active at start
		defer rt.stamp(id.SID) // and at end
		rt.proxyTo(w, r, e.WorkerPodIP, hostPort(e))
		return
	}

	// Miss: resume via the operator, coalescing concurrent misses for this sid.
	ctx, cancel := context.WithTimeout(r.Context(), rt.opts.ResumeDeadline)
	defer cancel()
	v, err, _ := rt.single.Do(id.SID, func() (any, error) {
		return rt.resumer.Resume(ctx, id.SID, id.Subject)
	})
	if err != nil {
		if errors.Is(err, ErrNoCapacity) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "no capacity, retry", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "resume failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	ip, _ := v.(string)
	if ip == "" {
		http.Error(w, "resume returned no worker", http.StatusBadGateway)
		return
	}
	// Re-read the (now Running) entry to learn the exposed host port to target.
	port := rt.opts.WorkerPort
	if e, gerr := rt.kv.GetSession(r.Context(), id.SID); gerr == nil {
		port = hostPort(e)
	}
	rt.stamp(id.SID)
	defer rt.stamp(id.SID)
	rt.proxyTo(w, r, ip, port)
}

// hostPort returns the worker-pod host port to proxy to: the session's first
// exposed port (host, or container if host==0). Falls back to 0 (caller uses its
// default) when the session declares no ports.
func hostPort(e *resumeapi.SessionEntry) int {
	if len(e.Ports) == 0 {
		return 0
	}
	p := e.Ports[0]
	if p.Host != 0 {
		return p.Host
	}
	return p.Container
}

// proxyTo streams the request to the worker at podIP:port. port==0 falls back to
// the configured WorkerPort (sandboxd control port) — only used when a session
// declares no exposed ports.
func (rt *Router) proxyTo(w http.ResponseWriter, r *http.Request, podIP string, port int) {
	if port == 0 {
		port = rt.opts.WorkerPort
	}
	target := &url.URL{Scheme: "http", Host: podIP + ":" + strconv.Itoa(port)}
	// Per-request rewrite: point at the worker, preserve path/query.
	outreq := r.Clone(r.Context())
	outreq.URL.Scheme = target.Scheme
	outreq.URL.Host = target.Host
	outreq.Host = target.Host
	rt.proxy.ServeHTTP(w, outreq)
}

// stamp records request activity for idle detection (O3). Best-effort: a failed
// stamp must never break request serving.
func (rt *Router) stamp(sid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = rt.kv.StampActive(ctx, sid, rt.opts.Now().UnixMilli())
}
