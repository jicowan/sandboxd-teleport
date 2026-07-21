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
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/gc"
)

// GCConfig carries the tunable GC policy from the operator flags into
// BuildCollector (PRD-session-garbage-collection §6/§7). All are configurable via
// flag/env in cmd/main.go.
type GCConfig struct {
	Bucket                string
	DefaultTTLSeconds     int  // applied when a Session sets no ttlAfterSuspendSeconds; 0 = keep forever
	AbandonedGraceSeconds int  // how long a session must look dead before reaping; 0 = abandoned/orphan-CR passes off
	DryRun                bool // true = classify + record, never mutate (default when arming)
}

// BuildCollector builds a gc.Collector with an S3 client from the AWS default
// chain (the operator's scoped Pod Identity: list+delete on sandboxes/* only).
// Per-session TTL is resolved from the Session's lifecycle.ttlAfterSuspendSeconds,
// falling back to gc.DefaultTTLSeconds. A SessionReaper (CR delete/tombstone) and
// a CRLister (orphan-CR pass) are wired over the cached client.
func BuildCollector(ctx context.Context, c client.Client, kv *assign.Client, namespace string, gcfg GCConfig) (*gc.Collector, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	s3api := s3.NewFromConfig(cfg)
	ttlFor := func(ctx context.Context, sid string) (int, error) {
		var s corev1alpha1.Session
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sid}, &s); err != nil {
			return 0, err
		}
		return s.Spec.Lifecycle.TTLAfterSuspendSeconds, nil
	}
	return gc.New(kv, s3api, gc.Config{
		Bucket:                gcfg.Bucket,
		TTLFor:                ttlFor,
		DefaultTTLSeconds:     gcfg.DefaultTTLSeconds,
		AbandonedGraceSeconds: gcfg.AbandonedGraceSeconds,
		DryRun:                gcfg.DryRun,
		Reaper:                NewSessionReaper(c, namespace, gcfg.DryRun),
		CRs:                   &crLister{client: c, namespace: namespace},
	}), nil
}

// crLister adapts the cached client to gc.CRLister — enumerating Session CRs with
// the minimal view the orphan-CR pass needs (id, phase, ownership, creation time).
type crLister struct {
	client    client.Client
	namespace string
}

func (l *crLister) ListSessions(ctx context.Context) ([]gc.CRSession, error) {
	var list corev1alpha1.SessionList
	if err := l.client.List(ctx, &list, client.InNamespace(l.namespace)); err != nil {
		return nil, err
	}
	out := make([]gc.CRSession, 0, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i]
		out = append(out, gc.CRSession{
			SID:              s.Name,
			Phase:            s.Status.Phase,
			OperatorOwned:    s.Labels[LabelCreatedBy] == CreatedByOperator,
			CreatedUnixMilli: s.CreationTimestamp.UnixMilli(),
		})
	}
	return out, nil
}

// GCSweeper is a manager Runnable that periodically reaps stale checkpoints.
type GCSweeper struct {
	Collector *gc.Collector
	Interval  time.Duration
}

// Start runs the GC sweep loop until the manager context is cancelled.
func (s *GCSweeper) Start(ctx context.Context) error {
	iv := s.Interval
	if iv == 0 {
		iv = 5 * time.Minute
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	log := logf.FromContext(ctx).WithName("gc-sweeper")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r, err := s.Collector.SweepOnce(ctx)
			if err != nil {
				log.Error(err, "session GC sweep failed")
			} else if r.Total() > 0 {
				verb := "reaped"
				if r.DryRun {
					verb = "dry-run: would reap"
				}
				log.Info(verb+" session footprint",
					"ttlExpired", r.TTL, "abandoned", r.Abandoned,
					"orphanS3", r.OrphanS3, "orphanCR", r.OrphanCR, "dryRun", r.DryRun)
			}
		}
	}
}

var _ manager.Runnable = &GCSweeper{}

// AddGCSweeper registers the periodic checkpoint GC sweeper.
func AddGCSweeper(mgr ctrl.Manager, col *gc.Collector, interval time.Duration) error {
	return mgr.Add(&GCSweeper{Collector: col, Interval: interval})
}
