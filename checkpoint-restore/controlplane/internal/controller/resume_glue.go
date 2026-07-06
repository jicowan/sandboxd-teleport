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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
func BuildResumeWorkflow(c client.Client, kv *assign.Client, namespace string, httpClient *http.Client) *resume.Workflow {
	lookup := func(ctx context.Context, name string) (*resume.TemplateSpec, error) {
		var t corev1alpha1.SandboxTemplate
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &t); err != nil {
			return nil, err
		}
		return templateSpecFromCRD(&t), nil
	}

	planFor := func(ctx context.Context, sid, subject string) (*resume.SessionPlan, error) {
		// Resolve the plan from the Session CR (the front door / broker creates it).
		var s corev1alpha1.Session
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sid}, &s); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("session %q not found", sid)
			}
			return nil, err
		}
		plan := &resume.SessionPlan{
			Cmd:    s.Spec.Cmd,
			Env:    s.Spec.Env,
			Ports:  portsFromCRD(s.Spec.Ports),
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

	return resume.New(kv, lookup, clientFor, planFor, resume.Options{})
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
