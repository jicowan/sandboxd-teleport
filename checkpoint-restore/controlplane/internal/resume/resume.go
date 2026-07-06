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

// Package resume implements the control-plane half of PRD §5.1: given a session
// id, get it Running on a worker and return that worker's IP. It is the authority
// for worker assignment and writes the KV assignment table via CAS (the router
// never does). Deliberately free of controller-runtime types at its boundary so
// it can later be lifted into a standalone service (TDD §2.4).
//
// P1 implements the COLD START path (session ABSENT / no checkpoint -> /run).
// The SUSPENDED -> /restore path is P3; it slots into resolve() + the state switch.
package resume

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/metrics"
	"github.com/jicowan/aio-sandbox/controlplane/internal/sandboxdclient"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
	"github.com/jicowan/aio-sandbox/shared/sbxapi"
)

// TemplateSpec is the minimal view of a SandboxTemplate the resume workflow needs
// to start a sandbox. Supplying it via an interface keeps this package free of
// the CRD/k8s types (TDD §2.4 split-readiness).
type TemplateSpec struct {
	Image  string
	Cmd    []string
	Env    []string
	Ports  []sbxapi.PortMap
	Health *sbxapi.Health
}

// SessionPlan is what to run for a session: either a template reference (resolved
// via TemplateLookup) or an inline arbitrary image (O6). Exactly one path is used.
type SessionPlan struct {
	Pool         string // pool to claim a worker from
	TemplateName string // "" when inline image mode
	Image        string // inline image (arbitrary-image mode), else ""
	Cmd          []string
	Env          []string
	Ports        []sbxapi.PortMap
	Health       *sbxapi.Health
}

// TemplateLookup resolves a template name (in a pool's namespace) to its spec.
// Implemented by the operator over its cached client.
type TemplateLookup func(ctx context.Context, name string) (*TemplateSpec, error)

// WorkerClientFactory builds a sandboxd client for a worker pod IP. Injectable so
// P1.5 can supply an mTLS transport and tests can supply a stub.
type WorkerClientFactory func(podIP string) *sandboxdclient.Client

// Options configure a Workflow.
type Options struct {
	// ResumeDeadline bounds the whole resume (TTFB clock, O8). Default 15s.
	ResumeDeadline time.Duration
	// PollInterval is how often to poll sandboxd /status while waiting for ready.
	PollInterval time.Duration
	// MaxConcurrentResumes caps in-flight resumes (backpressure, P5). A restore
	// pulls a checkpoint from S3 and drives a worker; a thundering herd (e.g. a
	// node loss then a traffic spike) could stampede S3/sandboxd. 0 = unlimited
	// (default off). When the cap is reached, a resume that can't acquire a slot
	// before ResumeDeadline returns ErrNoCapacity (-> 503 Retry-After), same as a
	// worker-exhausted pool — the caller retries.
	MaxConcurrentResumes int
}

// Workflow runs the Resume operation. Construct once, call Resume per request.
type Workflow struct {
	kv        *assign.Client
	lookup    TemplateLookup
	clientFor WorkerClientFactory
	planFor   func(ctx context.Context, sid, subject string) (*SessionPlan, error)
	notify    assign.PoolNotifier
	sem       chan struct{} // resume concurrency limiter; nil = unlimited
	opts      Options
}

// New builds a Workflow. planFor resolves a session id (+subject) to what should
// run — for P1 the operator supplies a plan from the Session CR or a default pool.
func New(kv *assign.Client, lookup TemplateLookup, clientFor WorkerClientFactory,
	planFor func(ctx context.Context, sid, subject string) (*SessionPlan, error), opts Options) *Workflow {
	if opts.ResumeDeadline == 0 {
		opts.ResumeDeadline = 15 * time.Second
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 250 * time.Millisecond
	}
	var sem chan struct{}
	if opts.MaxConcurrentResumes > 0 {
		sem = make(chan struct{}, opts.MaxConcurrentResumes)
	}
	return &Workflow{kv: kv, lookup: lookup, clientFor: clientFor, planFor: planFor, notify: assign.NopNotifier{}, sem: sem, opts: opts}
}

// WithNotifier wires a PoolNotifier so a claim/release nudges the WarmPool
// controller to refresh status (near-real-time). Returns wf for chaining.
func (wf *Workflow) WithNotifier(n assign.PoolNotifier) *Workflow {
	if n != nil {
		wf.notify = n
	}
	return wf
}

// Resume gets sid Running and returns the worker pod IP. Idempotent: if the
// session is already Running on a live worker, it returns immediately. On no
// capacity it returns assign.ErrNoCapacity (the handler maps that to 503).
// This is a thin instrumented wrapper around resume() (metrics: kind, outcome,
// duration).
func (wf *Workflow) Resume(ctx context.Context, sid, subject string) (string, error) {
	start := time.Now()
	ip, kind, fast, err := wf.resume(ctx, sid, subject)
	if fast {
		return ip, err // continuation fast path: not a resume, don't record
	}
	outcome := metrics.OutcomeSuccess
	switch {
	case errors.Is(err, assign.ErrNoCapacity):
		outcome = metrics.OutcomeNoCapacity
	case err != nil:
		outcome = metrics.OutcomeError
	}
	metrics.ResumesTotal.WithLabelValues(kind, outcome).Inc()
	if err == nil {
		metrics.ResumeDuration.WithLabelValues(kind).Observe(time.Since(start).Seconds())
	}
	return ip, err
}

// resume returns (ip, kind, fastPath, err). kind is cold_start|restore; fastPath
// is true when the session was already Running (no resume performed).
func (wf *Workflow) resume(ctx context.Context, sid, subject string) (string, string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, wf.opts.ResumeDeadline)
	defer cancel()

	// Read the current entry: it decides cold-start vs restore, and short-circuits
	// if already Running.
	cur, curErr := wf.kv.GetSession(ctx, sid)
	if curErr == nil && cur.State == resumeapi.StateRunning && cur.WorkerPodIP != "" {
		return cur.WorkerPodIP, "", true, nil // fast path (no slot needed)
	}

	// Backpressure (P5): acquire a resume slot before doing real work. The fast
	// path above is exempt (it's just a KV read). If we can't get a slot before
	// the deadline, treat it as no-capacity so the caller gets 503 Retry-After.
	kindGuess := metrics.KindColdStart
	if curErr == nil && cur.State == resumeapi.StateSuspended {
		kindGuess = metrics.KindRestore
	}
	if wf.sem != nil {
		select {
		case wf.sem <- struct{}{}:
			defer func() { <-wf.sem }()
		case <-ctx.Done():
			return "", kindGuess, false, assign.ErrNoCapacity
		}
	}

	// RESTORE branch (P3): the session was suspended and has a checkpoint in S3.
	// The image/ports were recorded at suspend time and travel with the session —
	// we restore those, NOT the template (the sandbox may have diverged from it).
	if curErr == nil && cur.State == resumeapi.StateSuspended && cur.SnapshotURI != "" {
		ip, err := wf.resumeFromSnapshot(ctx, cur)
		return ip, metrics.KindRestore, false, err
	}

	// COLD START branch (P1): ABSENT / no checkpoint -> /run from the plan.
	plan, err := wf.planFor(ctx, sid, subject)
	if err != nil {
		return "", metrics.KindColdStart, false, fmt.Errorf("resolve session plan: %w", err)
	}
	img := plan.Image
	cmd, env, ports, health := plan.Cmd, plan.Env, plan.Ports, plan.Health
	if plan.TemplateName != "" {
		tmpl, terr := wf.lookup(ctx, plan.TemplateName)
		if terr != nil {
			return "", metrics.KindColdStart, false, fmt.Errorf("lookup template %q: %w", plan.TemplateName, terr)
		}
		img = tmpl.Image
		if len(cmd) == 0 {
			cmd = tmpl.Cmd
		}
		if len(env) == 0 {
			env = tmpl.Env
		}
		if len(ports) == 0 {
			ports = tmpl.Ports
		}
		if health == nil {
			health = tmpl.Health
		}
	}
	if img == "" {
		return "", metrics.KindColdStart, false, errors.New("no image to run (empty template and no inline image)")
	}

	w, err := wf.kv.ClaimIdleWorker(ctx, plan.Pool, sid)
	if err != nil {
		return "", metrics.KindColdStart, false, err // ErrNoCapacity -> 503 at the handler
	}
	wf.notify.PoolChanged(w.Pool) // idle->busy: refresh pool status
	ip, err := wf.startAndBind(ctx, sid, w, false, img, cmd, env, ports, health, "")
	if err != nil {
		_ = wf.kv.ReleaseWorker(ctx, w.Pod, w.Pool)
		wf.notify.PoolChanged(w.Pool) // busy->idle (rolled back)
		return "", metrics.KindColdStart, false, err
	}
	return ip, metrics.KindColdStart, false, nil
}

// resumeFromSnapshot claims a (typically different) idle worker and restores the
// session's S3 checkpoint onto it — the teleport (PRD §3 path 2). Pool, image,
// ports, and snapshot all come from the recorded SessionEntry.
func (wf *Workflow) resumeFromSnapshot(ctx context.Context, cur *resumeapi.SessionEntry) (string, error) {
	if cur.Pool == "" {
		return "", errors.New("suspended session has no pool to restore into")
	}
	w, err := wf.kv.ClaimIdleWorker(ctx, cur.Pool, cur.SID)
	if err != nil {
		return "", err
	}
	wf.notify.PoolChanged(w.Pool) // idle->busy: refresh pool status
	ip, err := wf.startAndBind(ctx, cur.SID, w, true, cur.Image, nil, nil, cur.Ports, cur.Health, cur.SnapshotURI)
	if err != nil {
		_ = wf.kv.ReleaseWorker(ctx, w.Pod, w.Pool)
		wf.notify.PoolChanged(w.Pool)
		return "", err
	}
	return ip, nil
}

// startAndBind records Resuming, drives sandboxd /run (cold) or /restore (from a
// snapshot), waits for ready, then records Running. sid is the sandbox id on the
// worker (one per worker).
func (wf *Workflow) startAndBind(ctx context.Context, sid string, w *resumeapi.WorkerEntry,
	restore bool, img string, cmd, env []string, ports []sbxapi.PortMap, health *sbxapi.Health,
	snapshot string) (string, error) {

	// Record Resuming (CAS from whatever we last read; create if absent).
	if err := wf.casSession(ctx, sid, func(e *resumeapi.SessionEntry) {
		e.State = resumeapi.StateResuming
		e.Pool = w.Pool
		e.WorkerPod = w.Pod
		e.WorkerPodIP = w.PodIP
		e.Image = img
		e.Ports = ports
		if health != nil {
			e.Health = health // record so restore-on-connect can replay the probe
		}
	}); err != nil {
		return "", fmt.Errorf("mark resuming: %w", err)
	}

	cl := wf.clientFor(w.PodIP)
	if restore {
		if _, err := cl.Restore(ctx, sbxapi.RestoreRequest{
			SandboxID: sid, Image: img, Snapshot: snapshot, Ports: ports, Health: health,
		}); err != nil {
			return "", fmt.Errorf("worker /restore: %w", err)
		}
	} else {
		if _, err := cl.Run(ctx, sbxapi.RunRequest{
			SandboxID: sid, Image: img, Cmd: cmd, Env: env, Ports: ports, Health: health,
		}); err != nil {
			return "", fmt.Errorf("worker /run: %w", err)
		}
	}
	if err := cl.WaitReady(ctx, sid, wf.opts.PollInterval); err != nil {
		return "", fmt.Errorf("wait ready: %w", err)
	}

	if err := wf.casSession(ctx, sid, func(e *resumeapi.SessionEntry) {
		e.State = resumeapi.StateRunning
		e.WorkerPodIP = w.PodIP
	}); err != nil {
		return "", fmt.Errorf("mark running: %w", err)
	}
	return w.PodIP, nil
}

// casSession loads the current entry (or a fresh one), applies mutate, and writes
// with CAS, retrying on version conflict a bounded number of times.
func (wf *Workflow) casSession(ctx context.Context, sid string, mutate func(*resumeapi.SessionEntry)) error {
	const maxTries = 5
	for i := 0; i < maxTries; i++ {
		e, err := wf.kv.GetSession(ctx, sid)
		if errors.Is(err, assign.ErrNotFound) {
			e = &resumeapi.SessionEntry{SID: sid, Version: 0}
		} else if err != nil {
			return err
		}
		mutate(e)
		err = wf.kv.PutSessionCAS(ctx, e)
		if err == nil {
			return nil
		}
		if !errors.Is(err, assign.ErrVersionConflict) {
			return err
		}
	}
	return assign.ErrVersionConflict
}
