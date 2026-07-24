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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/snapshot"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// BaseReaper maintains each BaseSnapshot's refCount and reclaims a base once it is
// unpinned + unreferenced + past a grace (docs/sandboxd/PRD/PRD-snapshot-fork.md §5.4). It is
// deliberately conservative: CR-existence is the retention guarantee, refCount only
// GATES reclaim, and reclaim errs toward over-retention. A missed count never frees
// a base early because reclaim also requires the grace to elapse from the moment
// refCount is *observed* to be 0.
//
// refCount = snapshot-source fork children not yet materialized (their KV entry is
// absent or still Suspended — they haven't done their first /restore) + an implicit
// hold while pinned. A fork needs the base only for its first restore, so a child
// that has reached Running no longer counts.
type BaseReaper struct {
	client    client.Client
	kv        *assign.Client
	store     *snapshot.Store
	namespace string
	grace     time.Duration
	interval  time.Duration
	dryRun    bool
	now       func() time.Time
}

// NewBaseReaper builds a BaseReaper. store is retained for symmetry/future use but
// S3 reclaim is now owned by the BaseSnapshot finalizer (the reaper only deletes the
// CR, which triggers it). now defaults to time.Now.
func NewBaseReaper(c client.Client, kv *assign.Client, store *snapshot.Store, namespace string, grace, interval time.Duration, dryRun bool) *BaseReaper {
	return &BaseReaper{client: c, kv: kv, store: store, namespace: namespace, grace: grace, interval: interval, dryRun: dryRun, now: time.Now}
}

// refCountFor derives a base's current refCount: the number of its snapshot-source
// fork children that have NOT yet completed their first restore (KV absent or still
// Suspended). Requires KV; without it returns -1 (unknown → never reclaim).
func (r *BaseReaper) refCountFor(ctx context.Context, baseName string) (int32, error) {
	var children corev1alpha1.SessionList
	if err := r.client.List(ctx, &children, client.InNamespace(r.namespace),
		client.MatchingLabels{LabelForkBase: baseName}); err != nil {
		return -1, err
	}
	if r.kv == nil {
		return -1, nil
	}
	var refs int32
	for i := range children.Items {
		e, err := r.kv.GetSession(ctx, children.Items[i].Name)
		if err == assign.ErrNotFound {
			refs++ // not yet seeded/materialized → still needs the base
			continue
		}
		if err != nil {
			return -1, err
		}
		// Still depends on the base until it has moved past Suspended (first restore).
		if e.State == resumeapi.StateSuspended || e.State == resumeapi.StateResuming {
			refs++
		}
	}
	return refs, nil
}

// reconcileOnce updates every BaseSnapshot's refCount and reclaims eligible bases.
func (r *BaseReaper) reconcileOnce(ctx context.Context) {
	log := logf.FromContext(ctx).WithName("base-reaper")
	var bases corev1alpha1.BaseSnapshotList
	if err := r.client.List(ctx, &bases, client.InNamespace(r.namespace)); err != nil {
		log.Error(err, "list basesnapshots")
		return
	}
	nowT := r.now()
	for i := range bases.Items {
		b := &bases.Items[i]
		if !b.Status.Ready {
			continue // not promoted yet
		}
		refs, err := r.refCountFor(ctx, b.Name)
		if err != nil {
			log.Error(err, "refcount", "base", b.Name)
			continue
		}
		// Update status.refCount if it changed (observability + the reclaim gate).
		if refs >= 0 && refs != b.Status.RefCount {
			b.Status.RefCount = refs
			if err := r.client.Status().Update(ctx, b); err != nil {
				log.V(1).Info("refcount status update failed", "base", b.Name, "err", err)
			}
		}
		// Reclaim gate: unpinned AND refCount==0 AND past grace. Grace is measured
		// from the "Unreferenced" condition's transition time, set the first pass we
		// observe refCount==0 — so a base is never freed the instant its last fork
		// materializes; it must look unreferenced for the whole grace window.
		if b.Spec.Pinned || refs != 0 {
			r.clearUnreferenced(ctx, b)
			continue
		}
		since := r.markUnreferenced(ctx, b) // returns when it first went unreferenced
		if since.IsZero() || nowT.Sub(since) < r.grace {
			continue // within grace
		}
		if r.dryRun {
			log.Info("dry-run: would reclaim base", "base", b.Name)
			continue
		}
		// Delete the CR; the BaseSnapshot finalizer reclaims the bases/<id>/ S3
		// objects before the object is removed (single source of truth for S3
		// reclaim — same path as a direct/pinned delete).
		if err := r.client.Delete(ctx, b); err != nil {
			log.Error(err, "delete basesnapshot CR", "base", b.Name)
			continue
		}
		log.Info("reclaimed base snapshot (finalizer clears S3)", "base", b.Name)
	}
}

// markUnreferenced sets an "Unreferenced" condition the first time refCount hits 0
// and returns its transition time (the grace clock start).
func (r *BaseReaper) markUnreferenced(ctx context.Context, b *corev1alpha1.BaseSnapshot) time.Time {
	for i := range b.Status.Conditions {
		if b.Status.Conditions[i].Type == "Unreferenced" && b.Status.Conditions[i].Status == metav1.ConditionTrue {
			return b.Status.Conditions[i].LastTransitionTime.Time
		}
	}
	now := metav1.NewTime(r.now())
	setCondition(&b.Status.Conditions, metav1.Condition{
		Type: "Unreferenced", Status: metav1.ConditionTrue, Reason: "RefCountZero",
		Message: "no forks reference this base", ObservedGeneration: b.Generation, LastTransitionTime: now,
	})
	if err := r.client.Status().Update(ctx, b); err != nil {
		logf.FromContext(ctx).V(1).Info("mark unreferenced failed", "base", b.Name, "err", err)
		return time.Time{}
	}
	return now.Time
}

// clearUnreferenced removes the Unreferenced condition when a base is referenced
// again (a new ForkSet re-references it), resetting the grace clock.
func (r *BaseReaper) clearUnreferenced(ctx context.Context, b *corev1alpha1.BaseSnapshot) {
	for i := range b.Status.Conditions {
		if b.Status.Conditions[i].Type == "Unreferenced" && b.Status.Conditions[i].Status == metav1.ConditionTrue {
			setCondition(&b.Status.Conditions, metav1.Condition{
				Type: "Unreferenced", Status: metav1.ConditionFalse, Reason: "Referenced",
				Message: "forks reference this base", ObservedGeneration: b.Generation, LastTransitionTime: metav1.NewTime(r.now()),
			})
			_ = r.client.Status().Update(ctx, b)
			return
		}
	}
}

// setCondition replaces-or-appends a condition keyed by type.
func setCondition(conds *[]metav1.Condition, c metav1.Condition) {
	for i := range *conds {
		if (*conds)[i].Type == c.Type {
			(*conds)[i] = c
			return
		}
	}
	*conds = append(*conds, c)
}

// Start runs the reaper loop until the manager context is cancelled.
func (r *BaseReaper) Start(ctx context.Context) error {
	iv := r.interval
	if iv == 0 {
		iv = 60 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.reconcileOnce(ctx)
		}
	}
}

var _ manager.Runnable = &BaseReaper{}

// AddBaseReaper registers the base reaper as a manager Runnable.
func AddBaseReaper(mgr ctrl.Manager, reaper *BaseReaper) error {
	return mgr.Add(reaper)
}
