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

// BuildCollector builds a gc.Collector with an S3 client from the AWS default
// chain (the operator's scoped Pod Identity: list+delete on sandboxes/* only).
// The TTL is resolved from the Session's lifecycle.ttlAfterSuspendSeconds.
func BuildCollector(ctx context.Context, c client.Client, kv *assign.Client, namespace, bucket string) (*gc.Collector, error) {
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
	return gc.New(kv, s3api, bucket, ttlFor, nil), nil
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
			ttlN, orphanN, err := s.Collector.SweepOnce(ctx)
			if err != nil {
				log.Error(err, "checkpoint GC sweep failed")
			} else if ttlN > 0 || orphanN > 0 {
				log.Info("reaped checkpoints", "ttlExpired", ttlN, "orphans", orphanN)
			}
		}
	}
}

var _ manager.Runnable = &GCSweeper{}

// AddGCSweeper registers the periodic checkpoint GC sweeper.
func AddGCSweeper(mgr ctrl.Manager, col *gc.Collector, interval time.Duration) error {
	return mgr.Add(&GCSweeper{Collector: col, Interval: interval})
}
