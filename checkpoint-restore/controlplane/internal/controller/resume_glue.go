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

package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/resume"
	"github.com/jicowan/aio-sandbox/controlplane/internal/sandboxdclient"
	"github.com/jicowan/aio-sandbox/shared/sbxapi"
)

// BuildResumeWorkflow wires the resume.Workflow to the operator's cached client
// and the KV table. namespace is where SandboxTemplate/Session objects live
// (single-namespace MVP). httpClient is passed to sandboxd clients (nil = plain
// HTTP for P1; P1.5 supplies an mTLS client).
func BuildResumeWorkflow(c client.Client, kv *assign.Client, namespace string, httpClient *http.Client, opts resume.Options) *resume.Workflow {
	lookup := func(ctx context.Context, name string) (*resume.TemplateSpec, error) {
		var t corev1alpha1.SandboxTemplate
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &t); err != nil {
			return nil, err
		}
		return templateSpecFromCRD(&t), nil
	}

	planFor := func(ctx context.Context, sid, subject, poolHint string) (*resume.SessionPlan, error) {
		// Resolve the plan from the Session CR. If none exists yet, lazily create
		// one from the broker's pool hint (option b: the broker passes only a
		// session id + X-Sandbox-Pool header and stays free of our CRDs). Creating
		// the CR — not just synthesizing a transient plan — is required so the
		// idle-suspend and GC lookups (which read the Session CR) work for
		// broker-created sessions, and so `kubectl get sessions` shows them.
		var s corev1alpha1.Session
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sid}, &s)
		if apierrors.IsNotFound(err) {
			if poolHint == "" {
				return nil, fmt.Errorf("session %q not found and no pool hint given", sid)
			}
			s = corev1alpha1.Session{
				ObjectMeta: metav1.ObjectMeta{Name: sid, Namespace: namespace},
				Spec: corev1alpha1.SessionSpec{
					PoolRef: &corev1alpha1.LocalRef{Name: poolHint},
					Subject: subject,
				},
			}
			if cerr := c.Create(ctx, &s); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
				return nil, fmt.Errorf("create session %q from pool hint %q: %w", sid, poolHint, cerr)
			}
			// re-get if it already existed from a concurrent create
			if apierrors.IsAlreadyExists(err) {
				_ = c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sid}, &s)
			}
		} else if err != nil {
			return nil, err
		}
		plan := &resume.SessionPlan{
			Cmd:   s.Spec.Cmd,
			Env:   s.Spec.Env,
			Ports: portsFromCRD(s.Spec.Ports),
		}
		// Session-level IAM role overrides the template's (resolved in the resume
		// workflow when TemplateName is set).
		if s.Spec.IAM != nil {
			plan.IAMRoleARN = s.Spec.IAM.RoleARN
		}
		switch {
		case s.Spec.Image != "": // arbitrary-image mode (O6)
			plan.Image = s.Spec.Image
			// Arbitrary-image sessions run in their own pool (O6c). If the CR names
			// a pool use it, else the session must reference one.
			if s.Spec.PoolRef != nil {
				plan.Pool = s.Spec.PoolRef.Name
			}
		case s.Spec.PoolRef != nil: // template mode
			plan.Pool = s.Spec.PoolRef.Name
			// The pool's template supplies the image; resolve the pool -> template.
			tmplName, err := templateForPool(ctx, c, namespace, s.Spec.PoolRef.Name)
			if err != nil {
				return nil, err
			}
			plan.TemplateName = tmplName
		default:
			return nil, fmt.Errorf("session %q has neither poolRef nor image", sid)
		}
		if plan.Pool == "" {
			return nil, fmt.Errorf("session %q resolves to no pool", sid)
		}
		return plan, nil
	}

	clientFor := func(podIP string) *sandboxdclient.Client {
		return sandboxdclient.New(podIP, httpClient)
	}

	return resume.New(kv, lookup, clientFor, planFor, opts).
		WithMirror(NewSessionMirror(c, namespace))
}

// BuildSuspender wires resume.Suspender to the operator's cached client + KV.
// It resolves each session's idle policy from its pool's SandboxTemplate.
func BuildSuspender(c client.Client, kv *assign.Client, namespace string, httpClient *http.Client) *resume.Suspender {
	clientFor := func(podIP string) *sandboxdclient.Client {
		return sandboxdclient.New(podIP, httpClient)
	}
	policyFor := func(ctx context.Context, sid string) (resume.IdlePolicy, error) {
		var s corev1alpha1.Session
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sid}, &s); err != nil {
			return resume.IdlePolicy{}, err
		}
		// Session lifecycle idle timeout overrides the template's when set.
		var pol resume.IdlePolicy
		if s.Spec.PoolRef != nil {
			tmplName, err := templateForPool(ctx, c, namespace, s.Spec.PoolRef.Name)
			if err == nil {
				var t corev1alpha1.SandboxTemplate
				if e := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: tmplName}, &t); e == nil {
					pol.TimeoutSeconds = t.Spec.Idle.TimeoutSeconds
					pol.Action = t.Spec.Idle.Action
				}
			}
		}
		if s.Spec.Lifecycle.IdleTimeoutSeconds > 0 {
			pol.TimeoutSeconds = s.Spec.Lifecycle.IdleTimeoutSeconds
		}
		return pol, nil
	}
	return resume.NewSuspender(kv, clientFor, policyFor, resume.SuspendOptions{}).
		WithMirror(NewSessionMirror(c, namespace))
}

// BuildCheckpointer wires resume.Checkpointer to the cached client + KV, resolving
// each session's periodic-checkpoint interval from its pool's template (P5).
func BuildCheckpointer(c client.Client, kv *assign.Client, namespace string, httpClient *http.Client) *resume.Checkpointer {
	clientFor := func(podIP string) *sandboxdclient.Client {
		return sandboxdclient.New(podIP, httpClient)
	}
	policyFor := func(ctx context.Context, sid string) (resume.CheckpointPolicy, error) {
		var s corev1alpha1.Session
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sid}, &s); err != nil {
			return resume.CheckpointPolicy{}, err
		}
		if s.Spec.PoolRef == nil {
			return resume.CheckpointPolicy{}, nil
		}
		tmplName, err := templateForPool(ctx, c, namespace, s.Spec.PoolRef.Name)
		if err != nil {
			return resume.CheckpointPolicy{}, err
		}
		var t corev1alpha1.SandboxTemplate
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: tmplName}, &t); err != nil {
			return resume.CheckpointPolicy{}, err
		}
		return resume.CheckpointPolicy{IntervalSeconds: t.Spec.CheckpointIntervalSeconds}, nil
	}
	return resume.NewCheckpointer(kv, clientFor, policyFor, 0, nil)
}

// CheckpointSweeper is a manager Runnable that periodically checkpoints opted-in
// long-lived Running sessions (P5).
type CheckpointSweeper struct {
	Checkpointer *resume.Checkpointer
	Interval     time.Duration
}

// Start runs the checkpoint sweep loop until the manager context is cancelled.
func (s *CheckpointSweeper) Start(ctx context.Context) error {
	iv := s.Interval
	if iv == 0 {
		iv = 15 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	log := logf.FromContext(ctx).WithName("checkpoint-sweeper")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if n, err := s.Checkpointer.SweepOnce(ctx); err != nil {
				log.Error(err, "periodic checkpoint sweep failed")
			} else if n > 0 {
				log.Info("periodic-checkpointed running sessions", "count", n)
			}
		}
	}
}

var _ manager.Runnable = &CheckpointSweeper{}

// AddCheckpointSweeper registers the periodic-checkpoint sweeper.
func AddCheckpointSweeper(mgr ctrl.Manager, cp *resume.Checkpointer, interval time.Duration) error {
	return mgr.Add(&CheckpointSweeper{Checkpointer: cp, Interval: interval})
}

// SuspendSweeper is a manager Runnable that periodically suspends idle sessions.
type SuspendSweeper struct {
	Suspender *resume.Suspender
	Interval  time.Duration
}

// Start runs the sweep loop until the manager context is cancelled.
func (s *SuspendSweeper) Start(ctx context.Context) error {
	iv := s.Interval
	if iv == 0 {
		iv = 15 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	log := logf.FromContext(ctx).WithName("suspend-sweeper")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if n, err := s.Suspender.SweepOnce(ctx); err != nil {
				log.Error(err, "idle-suspend sweep failed")
			} else if n > 0 {
				log.Info("suspended idle sessions", "count", n)
			}
		}
	}
}

var _ manager.Runnable = &SuspendSweeper{}

// AddSuspendSweeper registers the periodic idle-suspend sweeper.
func AddSuspendSweeper(mgr ctrl.Manager, susp *resume.Suspender, interval time.Duration) error {
	return mgr.Add(&SuspendSweeper{Suspender: susp, Interval: interval})
}

// templateForPool returns the SandboxTemplate name a WarmPool references.
func templateForPool(ctx context.Context, c client.Client, ns, pool string) (string, error) {
	var wp corev1alpha1.WarmPool
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: pool}, &wp); err != nil {
		return "", fmt.Errorf("get warmpool %q: %w", pool, err)
	}
	return wp.Spec.TemplateRef.Name, nil
}

func templateSpecFromCRD(t *corev1alpha1.SandboxTemplate) *resume.TemplateSpec {
	ts := &resume.TemplateSpec{
		Image: t.Spec.Image,
		Cmd:   t.Spec.Cmd,
		Env:   t.Spec.Env,
		Ports: portsFromCRD(t.Spec.Ports),
	}
	if t.Spec.IAM != nil {
		ts.IAMRoleARN = t.Spec.IAM.RoleARN
	}
	if t.Spec.Health != nil {
		ts.Health = &sbxapi.Health{
			RestartPolicy: t.Spec.Health.RestartPolicy,
			Probe:         t.Spec.Health.Probe,
			ProbePort:     t.Spec.Health.ProbePort,
			ProbePath:     t.Spec.Health.ProbePath,
		}
		// Map the template idle policy onto the worker's idle-timeout field.
		if t.Spec.Idle.TimeoutSeconds > 0 {
			ts.Health.IdleTimeoutSec = t.Spec.Idle.TimeoutSeconds
		}
	}
	return ts
}

func portsFromCRD(in []corev1alpha1.PortMap) []sbxapi.PortMap {
	if len(in) == 0 {
		return nil
	}
	out := make([]sbxapi.PortMap, len(in))
	for i, p := range in {
		out[i] = sbxapi.PortMap{Container: p.Container, Host: p.Host}
	}
	return out
}

// ResumeServer is a manager Runnable that serves the internal /resume endpoint.
// P1: plain HTTP on addr. P1.5 will wrap with SPIRE mTLS.
type ResumeServer struct {
	Addr    string
	Handler http.Handler
}

// NewResumeServer builds the Runnable.
func NewResumeServer(addr string, h http.Handler) *ResumeServer {
	return &ResumeServer{Addr: addr, Handler: h}
}

// Start runs the HTTP server until the manager context is cancelled.
func (s *ResumeServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/resume", s.Handler)
	srv := &http.Server{Addr: s.Addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// interface assertion: ResumeServer is a manager.Runnable.
var _ manager.Runnable = &ResumeServer{}

// AddResumeServer registers the resume HTTP server with the manager.
func AddResumeServer(mgr ctrl.Manager, addr string, h http.Handler) error {
	return mgr.Add(NewResumeServer(addr, h))
}
