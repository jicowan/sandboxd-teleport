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
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

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
	appLookup := func(ctx context.Context, name string) (*resume.TemplateSpec, error) {
		var a corev1alpha1.AppTemplate
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &a); err != nil {
			return nil, err
		}
		return appSpecFromCRD(&a), nil
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
				ObjectMeta: metav1.ObjectMeta{
					Name:      sid,
					Namespace: namespace,
					// Mark this CR operator-owned so GC may reap it when the session
					// dies (a user-declared Session carries no such label and is only
					// tombstoned to Absent, never deleted).
					Labels: map[string]string{LabelCreatedBy: CreatedByOperator},
				},
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
		// Capacity is always the pool. Workload source has a precedence
		// (image > appRef > pool's own SandboxTemplate) resolved below
		// (docs/PRD-arbitrary-image-sessions.md §13.3).
		if s.Spec.PoolRef != nil {
			plan.Pool = s.Spec.PoolRef.Name
		}
		switch {
		case s.Spec.Image != "": // inline arbitrary image (admin/kubectl escape hatch, §12).
			plan.Image = s.Spec.Image
		case s.Spec.AppRef != nil: // generic-pool workload from an AppTemplate, decoupled from the pool.
			plan.AppName = s.Spec.AppRef.Name
		case s.Spec.PoolRef != nil: // dedicated-pool mode: the pool's own SandboxTemplate supplies the image.
			tmplName, err := templateForPool(ctx, c, namespace, s.Spec.PoolRef.Name)
			if err != nil {
				return nil, err
			}
			plan.TemplateName = tmplName
		default:
			return nil, fmt.Errorf("session %q has neither poolRef, appRef, nor image", sid)
		}
		if plan.Pool == "" {
			return nil, fmt.Errorf("session %q resolves to no pool", sid)
		}
		return plan, nil
	}

	clientFor := func(podIP string) *sandboxdclient.Client {
		if httpClient != nil {
			return sandboxdclient.NewMTLS(podIP, httpClient)
		}
		return sandboxdclient.New(podIP, nil)
	}

	return resume.New(kv, lookup, clientFor, planFor, opts).
		WithAppLookup(appLookup).
		WithMirror(NewSessionMirror(c, namespace))
}

// BuildSuspender wires resume.Suspender to the operator's cached client + KV.
// It resolves each session's idle policy from its pool's SandboxTemplate.
func BuildSuspender(c client.Client, kv *assign.Client, namespace string, httpClient *http.Client) *resume.Suspender {
	clientFor := func(podIP string) *sandboxdclient.Client {
		if httpClient != nil {
			return sandboxdclient.NewMTLS(podIP, httpClient)
		}
		return sandboxdclient.New(podIP, nil)
	}
	policyFor := func(ctx context.Context, sid string) (resume.IdlePolicy, error) {
		var s corev1alpha1.Session
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sid}, &s); err != nil {
			return resume.IdlePolicy{}, err
		}
		// Session lifecycle idle timeout overrides the config's when set. The config
		// policy is resolved via the shared precedence (image > appRef > pool's
		// SandboxTemplate), so an appRef/generic-pool session gets the same idle
		// policy it would from a dedicated pool.
		var pol resume.IdlePolicy
		if cp, terr := configPolicyForSession(ctx, c, namespace, &s); terr == nil {
			pol.TimeoutSeconds = cp.IdleTimeoutSeconds
			pol.Action = cp.IdleAction
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
		if httpClient != nil {
			return sandboxdclient.NewMTLS(podIP, httpClient)
		}
		return sandboxdclient.New(podIP, nil)
	}
	policyFor := func(ctx context.Context, sid string) (resume.CheckpointPolicy, error) {
		var s corev1alpha1.Session
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sid}, &s); err != nil {
			return resume.CheckpointPolicy{}, err
		}
		// Resolve the config policy via the shared precedence (image > appRef >
		// pool's SandboxTemplate). No template (inline-image or pool-less) => no
		// periodic checkpoint policy.
		cp, terr := configPolicyForSession(ctx, c, namespace, &s)
		if terr != nil {
			return resume.CheckpointPolicy{}, terr
		}
		return resume.CheckpointPolicy{IntervalSeconds: cp.CheckpointIntervalSeconds}, nil
	}
	return resume.NewCheckpointer(kv, clientFor, policyFor, 0, nil).
		WithMirror(NewSessionMirror(c, namespace))
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
	// Stagger against the suspend sweeper (same interval) so the two indexed reads
	// don't fire in lockstep — spreads Valkey load across the period.
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(iv / 2):
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

// sessionConfigPolicy is the idle + periodic-checkpoint policy a session inherits
// from its workload config source (an AppTemplate via appRef, or the pool's
// SandboxTemplate). Zero value = no policy (e.g. an inline-image session).
type sessionConfigPolicy struct {
	IdleTimeoutSeconds        int
	IdleAction                string
	CheckpointIntervalSeconds int
}

// configPolicyForSession resolves a session's idle/checkpoint policy from its
// workload CONFIG source, following the same precedence as the resume path
// (image > appRef > pool's own SandboxTemplate; §13.3). It is the single resolver
// the suspender and checkpointer share so their policy lookups honor appRef
// identically to cold start. Returns a zero policy (no error) when the config is
// not template-derived: an inline-image session, or a session with no pool.
func configPolicyForSession(ctx context.Context, c client.Client, ns string, s *corev1alpha1.Session) (sessionConfigPolicy, error) {
	switch {
	case s.Spec.Image != "":
		return sessionConfigPolicy{}, nil // inline image: no template policy
	case s.Spec.AppRef != nil:
		var a corev1alpha1.AppTemplate
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: s.Spec.AppRef.Name}, &a); err != nil {
			return sessionConfigPolicy{}, err
		}
		return sessionConfigPolicy{
			IdleTimeoutSeconds:        a.Spec.Idle.TimeoutSeconds,
			IdleAction:                a.Spec.Idle.Action,
			CheckpointIntervalSeconds: a.Spec.CheckpointIntervalSeconds,
		}, nil
	case s.Spec.PoolRef != nil:
		tmplName, err := templateForPool(ctx, c, ns, s.Spec.PoolRef.Name)
		if err != nil {
			return sessionConfigPolicy{}, err
		}
		var t corev1alpha1.SandboxTemplate
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: tmplName}, &t); err != nil {
			return sessionConfigPolicy{}, err
		}
		return sessionConfigPolicy{
			IdleTimeoutSeconds:        t.Spec.Idle.TimeoutSeconds,
			IdleAction:                t.Spec.Idle.Action,
			CheckpointIntervalSeconds: t.Spec.CheckpointIntervalSeconds,
		}, nil
	default:
		return sessionConfigPolicy{}, nil
	}
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
	// Record the resolved idle/checkpoint policy so the KV due-indexes are
	// maintained without a hot-path lookup (PRD-control-plane-scalability).
	ts.IdleTimeoutSeconds = t.Spec.Idle.TimeoutSeconds
	ts.CheckpointIntervalSeconds = t.Spec.CheckpointIntervalSeconds
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

// appSpecFromCRD maps an AppTemplate onto the same resume.TemplateSpec the resolver
// consumes, so an appRef workload resolves identically to a SandboxTemplate one
// (docs/PRD-arbitrary-image-sessions.md §13). AppTemplate has no worker-shape fields,
// so only the workload subset is mapped.
func appSpecFromCRD(a *corev1alpha1.AppTemplate) *resume.TemplateSpec {
	ts := &resume.TemplateSpec{
		Image: a.Spec.Image,
		Cmd:   a.Spec.Cmd,
		Env:   a.Spec.Env,
		Ports: portsFromCRD(a.Spec.Ports),
	}
	if a.Spec.IAM != nil {
		ts.IAMRoleARN = a.Spec.IAM.RoleARN
	}
	ts.IdleTimeoutSeconds = a.Spec.Idle.TimeoutSeconds
	ts.CheckpointIntervalSeconds = a.Spec.CheckpointIntervalSeconds
	if a.Spec.Health != nil {
		ts.Health = &sbxapi.Health{
			RestartPolicy: a.Spec.Health.RestartPolicy,
			Probe:         a.Spec.Health.Probe,
			ProbePort:     a.Spec.Health.ProbePort,
			ProbePath:     a.Spec.Health.ProbePath,
		}
		if a.Spec.Idle.TimeoutSeconds > 0 {
			ts.Health.IdleTimeoutSec = a.Spec.Idle.TimeoutSeconds
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
// P1: plain HTTP on addr. P1.5: when TLSConfig is set (SPIFFE mTLS authorizing the
// router's SPIFFE ID) the server requires+verifies a client SVID; nil = plain HTTP.
type ResumeServer struct {
	Addr      string
	Handler   http.Handler
	TLSConfig *tls.Config // nil = plain HTTP (rollout fallback)
}

// NewResumeServer builds the Runnable. tlsCfg nil => plain HTTP.
func NewResumeServer(addr string, h http.Handler, tlsCfg *tls.Config) *ResumeServer {
	return &ResumeServer{Addr: addr, Handler: h, TLSConfig: tlsCfg}
}

// Start runs the HTTP(S) server until the manager context is cancelled.
func (s *ResumeServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/resume", s.Handler)
	srv := &http.Server{Addr: s.Addr, Handler: mux, TLSConfig: s.TLSConfig}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	var err error
	if s.TLSConfig != nil {
		// certs come from the SVID in TLSConfig; empty cert/key file args are correct.
		err = srv.ListenAndServeTLS("", "")
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// interface assertion: ResumeServer is a manager.Runnable.
var _ manager.Runnable = &ResumeServer{}

// AddResumeServer registers the resume HTTP server with the manager.
func AddResumeServer(mgr ctrl.Manager, addr string, h http.Handler, tlsCfg *tls.Config) error {
	return mgr.Add(NewResumeServer(addr, h, tlsCfg))
}
