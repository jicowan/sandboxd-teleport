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
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// KVReader is the read-only slice of the assignment table the router needs.
// (assign.Client satisfies it; the router must not write.) GetWorker mirrors the
// operator resume path's workerHolds() fence so the fast path can verify the
// worker is still live before proxying.
type KVReader interface {
	GetSession(ctx context.Context, sid string) (*resumeapi.SessionEntry, error)
	StampActive(ctx context.Context, sid string, unixMillis int64) error
	GetWorker(ctx context.Context, pod string) (*resumeapi.WorkerEntry, error)
}

// maxBufferBody bounds how large a request body the router will buffer to enable
// a fast-path retry (see ServeHTTP). MCP tool calls are small JSON POSTs; bodies
// larger than this stream directly and forgo the retry safety net.
const maxBufferBody = 1 << 20 // 1 MiB

// Resumer triggers a resume on a KV miss (implemented by ResumeClient). poolHint
// (X-Session-Pool) and appHint (X-Session-App) let the operator lazily create a
// Session CR when none exists yet — capacity and workload respectively.
type Resumer interface {
	Resume(ctx context.Context, sid, subject, poolHint, appHint string) (string, error)
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

	// Warm primitive (protocol-agnostic): "ensure this session is Running" without
	// proxying anything to the workload. Lets a front door pre-warm on connect
	// (e.g. MCP initialize) without the router knowing anything about the payload
	// protocol. Returns 204 on success, 503 Retry-After on no capacity.
	if r.URL.Path == "/_warm" {
		rt.handleWarm(w, r, id)
		return
	}

	// Fast path: session already Running AND the bound worker is still live.
	// Liveness mirrors the operator resume path's workerHolds() fence
	// (resume.go): the worker:<pod> KV entry must still exist, be busy, and be
	// bound to THIS session. Without this a crashed/pruned worker's stale
	// Running record would proxy to a dead IP (a long dial timeout, then 502)
	// until a resume re-verified it. On a stale/missing worker we fall straight
	// through to ensureRunning (transparent teleport-restore).
	if e, err := rt.kv.GetSession(r.Context(), id.SID); err == nil &&
		e.State == resumeapi.StateRunning && e.WorkerPodIP != "" &&
		rt.workerLive(r.Context(), e.WorkerPod, id.SID) {
		rt.stamp(id.SID)       // O3 bracketing: active at start
		defer rt.stamp(id.SID) // and at end
		// Reactive safety net: a worker can die inside the ~30s prune window, so
		// the KV entry above may still look live. Buffer a small body so that if
		// the upstream connection fails BEFORE any bytes reach the client, we can
		// fall through to a resume once instead of returning 502.
		body, ok := bufferBody(r)
		if ok {
			if rt.proxyWithRetry(w, r, e.WorkerPodIP, hostPort(e), body) {
				return
			}
			// upstream failed with nothing written yet — fall through to resume,
			// rewinding the buffered body for the retried request.
			r.Body = io.NopCloser(bytes.NewReader(body))
		} else {
			rt.proxyTo(w, r, e.WorkerPodIP, hostPort(e))
			return
		}
	}

	ip, port, err := rt.ensureRunning(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNoCapacity) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "no capacity, retry", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "resume failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	rt.stamp(id.SID)
	defer rt.stamp(id.SID)
	rt.proxyTo(w, r, ip, port)
}

// workerLive reports whether the worker bound to a Running session is still
// alive in the assignment table — the router-side equivalent of the resume
// path's workerHolds(). A missing/idle/rebound worker entry means the Running
// record is stale (pod crashed or was pruned). An empty pod name (older record)
// is treated as NOT live so we re-resume rather than trust a dangling IP.
func (rt *Router) workerLive(ctx context.Context, pod, sid string) bool {
	if pod == "" {
		return false
	}
	w, err := rt.kv.GetWorker(ctx, pod)
	if err != nil {
		return false
	}
	return resumeapi.WorkerHolds(w, sid)
}

// bufferBody reads a request body into memory when it is small enough to enable
// a fast-path retry. Returns (nil, true) for an empty/absent body, (buf, true)
// when fully buffered, and (nil, false) when the body is too large or unknown-
// length — in which case the caller must stream without the retry net. On a
// successful buffer the request's Body is replaced with a reader over buf.
func bufferBody(r *http.Request) ([]byte, bool) {
	if r.Body == nil || r.ContentLength == 0 {
		return nil, true
	}
	if r.ContentLength < 0 || r.ContentLength > maxBufferBody {
		return nil, false // unknown or too large: stream, no retry
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxBufferBody+1))
	r.Body.Close()
	if err != nil || int64(len(buf)) > maxBufferBody {
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(buf))
	return buf, true
}

// ensureRunning resolves the session to a Running worker, resuming via the
// operator on a miss (singleflight-coalesced). Returns the worker IP + the host
// port to target.
func (rt *Router) ensureRunning(reqCtx context.Context, id Identity) (string, int, error) {
	ctx, cancel := context.WithTimeout(reqCtx, rt.opts.ResumeDeadline)
	defer cancel()
	v, err, _ := rt.single.Do(id.SID, func() (any, error) {
		return rt.resumer.Resume(ctx, id.SID, id.Subject, id.PoolHint, id.AppHint)
	})
	if err != nil {
		return "", 0, err
	}
	ip, _ := v.(string)
	if ip == "" {
		return "", 0, errNoWorker
	}
	port := rt.opts.WorkerPort
	if e, gerr := rt.kv.GetSession(reqCtx, id.SID); gerr == nil {
		port = hostPort(e)
	}
	return ip, port, nil
}

// handleWarm ensures the session is Running and returns 204 — no proxy. Generic
// (no workload-protocol awareness).
func (rt *Router) handleWarm(w http.ResponseWriter, r *http.Request, id Identity) {
	if _, _, err := rt.ensureRunning(r.Context(), id); err != nil {
		if errors.Is(err, ErrNoCapacity) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "no capacity, retry", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "warm failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	rt.stamp(id.SID)
	w.WriteHeader(http.StatusNoContent)
}

var errNoWorker = errors.New("router: resume returned no worker")

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

// retryWriter wraps a ResponseWriter to record whether the upstream response has
// begun reaching the client. Once wrote==true a request can no longer be safely
// retried (the client has seen part of a response), so the fast-path fall-through
// must give up. It also swallows the proxy's ErrorHandler write when nothing has
// been sent yet, letting the caller retry with a clean writer.
type retryWriter struct {
	http.ResponseWriter
	wrote  bool // any header or body byte forwarded to the client
	failed bool // proxy ErrorHandler fired before anything was written
}

func (rw *retryWriter) WriteHeader(code int) {
	rw.wrote = true
	rw.ResponseWriter.WriteHeader(code)
}
func (rw *retryWriter) Write(b []byte) (int, error) {
	rw.wrote = true
	return rw.ResponseWriter.Write(b)
}
func (rw *retryWriter) Flush() {
	if fl, ok := rw.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// proxyWithRetry proxies to the worker and reports whether the request was
// served (true) or failed at the transport BEFORE any bytes reached the client
// (false). A false return means the caller may safely retry via resume: nothing
// was written, so no partial/duplicate response is possible. body is the
// buffered request payload, re-attached to the cloned upstream request so the
// proxy can send it.
func (rt *Router) proxyWithRetry(w http.ResponseWriter, r *http.Request, podIP string, port int, body []byte) bool {
	if port == 0 {
		port = rt.opts.WorkerPort
	}
	target := &url.URL{Scheme: "http", Host: podIP + ":" + strconv.Itoa(port)}
	rw := &retryWriter{ResponseWriter: w}

	outreq := r.Clone(r.Context())
	outreq.URL.Scheme = target.Scheme
	outreq.URL.Host = target.Host
	outreq.Host = target.Host
	outreq.Body = io.NopCloser(bytes.NewReader(body))
	outreq.ContentLength = int64(len(body))

	// Per-request ErrorHandler: if the upstream fails with nothing yet written,
	// mark failed (and DON'T emit a 502) so the caller can fall through. If bytes
	// were already forwarded, surface the 502 — retry is no longer safe.
	proxy := *rt.proxy // shallow copy: per-request ErrorHandler without racing others
	proxy.ErrorHandler = func(hw http.ResponseWriter, hr *http.Request, err error) {
		if !rw.wrote {
			rw.failed = true
			return
		}
		http.Error(hw, "upstream error: "+err.Error(), http.StatusBadGateway)
	}
	proxy.ServeHTTP(rw, outreq)
	return !rw.failed
}

// stamp records request activity for idle detection (O3). Best-effort: a failed
// stamp must never break request serving.
func (rt *Router) stamp(sid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = rt.kv.StampActive(ctx, sid, rt.opts.Now().UnixMilli())
}
