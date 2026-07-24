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
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/metrics"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// WorkerDiscoveryReconciler watches sandboxd worker pods (label app=worker) and
// writes their registration into the KV assignment table (TDD §4.3). The
// operator is the SOLE writer of KV worker state — workers never self-register,
// so sandboxd holds no Valkey credentials.
//
// A pod that is Ready → upsert WorkerEntry{state:idle} (unless it already holds a
// session, which only the resume/lifecycle path sets). A pod deleted or NotReady
// → remove it from the idle set / delete the entry, so a dead worker never keeps
// a stale idle slot.
type WorkerDiscoveryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	KV     *assign.Client
	// Namespace is where worker pods live (used by the prune sweep to look them up).
	Namespace string
	// TerminateSuspender, if set, is invoked when a BUSY worker's pod enters
	// Terminating: it checkpoints the session to S3 before the pod dies so the
	// session teleport-resumes losslessly (checkpoint-on-terminate). Optional
	// (nil disables the behavior — falls back to just removing the KV entry).
	TerminateSuspender TerminateSuspender

	// Notify, if set, is nudged after a reclaim returns a worker to idle so the
	// WarmPool status refreshes immediately (busy->idle). Optional.
	Notify assign.PoolNotifier
	// ReclaimGrace is how long a worker's busy binding must remain ANOMALOUS
	// (orphaned / bound to a Suspended or rebound session) before the reclaim
	// sweep returns it to the idle pool. It MUST exceed the resume deadline so an
	// in-flight claim->bind (worker marked busy before its session entry exists)
	// is never mistaken for a leak. 0 disables reclaim. See
	// docs/sandboxd/PRD/DESIGN-worker-binding-reclaim.md.
	ReclaimGrace time.Duration
	// Now is the clock (injectable for tests); defaults to time.Now.
	Now func() time.Time

	// anomalous tracks, per busy pod, when its binding was first seen anomalous and
	// the worker Version observed then. A binding is only reclaimed once it has been
	// anomalous across at least two sweeps AND its version is unchanged AND the grace
	// has elapsed — so a legitimate claim (which bumps Version within the resume
	// deadline) can never be reclaimed. Cleared when a pod goes healthy/idle/gone.
	// Reconcile runs single-threaded per informer, and the sweep loop is the only
	// other reader/writer; access is serialized by the manager's runnable scheduling.
	anomalous map[string]anomalyMark
}

// anomalyMark records the first-seen time and worker version of an anomalous busy
// binding, for the two-strike + version-stable reclaim gate.
type anomalyMark struct {
	firstSeen time.Time
	version   int64
}

// TerminateSuspender checkpoints a session on a terminating worker. Implemented
// by resume.Suspender.SuspendForTerminate; an interface here to keep the
// controller package free of a hard dependency and easy to fake in tests.
type TerminateSuspender interface {
	SuspendForTerminate(ctx context.Context, sid, workerPod, workerPodIP, pool string) error
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func (r *WorkerDiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pod corev1.Pod
	err := r.Get(ctx, req.NamespacedName, &pod)
	if apierrors.IsNotFound(err) {
		// Pod gone: we can't read its pool label anymore, so remove from any pool
		// idle set by pod name. The pool is encoded in the KV entry; look it up.
		r.removeByPod(ctx, req.Name)
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	pool := pod.Labels[LabelPool]
	if pool == "" {
		return ctrl.Result{}, nil // not a pool worker
	}

	// Terminating pod → if it holds a live session, checkpoint that session to S3
	// before the pod dies (checkpoint-on-terminate) so it teleport-resumes
	// losslessly; otherwise just drop the KV entry. The worker's own SIGTERM
	// handler drain-waits (keeps serving until its sandbox is gone) to give this
	// /suspend time to land within the pod's termination grace period.
	if pod.DeletionTimestamp != nil {
		if r.TerminateSuspender != nil {
			if w, gerr := r.KV.GetWorker(ctx, pod.Name); gerr == nil &&
				w.State == resumeapi.WorkerBusy && w.SID != "" {
				if err := r.TerminateSuspender.SuspendForTerminate(ctx, w.SID, w.Pod, w.PodIP, w.Pool); err != nil {
					// Best-effort: log and still drop the entry. The pod is going away;
					// a failed checkpoint degrades to today's behavior (resume from the
					// last snapshot / cold start), never a wedge.
					log.Error(err, "checkpoint-on-terminate failed", "pod", pod.Name, "sid", w.SID)
					r.remove(ctx, pod.Name, pool)
				}
				// SuspendForTerminate already removed the worker entry on success.
				return ctrl.Result{}, nil
			}
		}
		r.remove(ctx, pod.Name, pool)
		return ctrl.Result{}, nil
	}

	if !podReady(&pod) || pod.Status.PodIP == "" {
		// Not serving yet (or lost readiness): drop from idle set so we don't
		// hand a session to a worker that can't accept it. If it never became
		// busy, removing the entry is safe; resume path is the only busy-writer.
		r.remove(ctx, pod.Name, pool)
		return ctrl.Result{}, nil
	}

	// Ready worker. Preserve a busy assignment if one exists (resume path owns it);
	// otherwise register/refresh as idle.
	existing, gerr := r.KV.GetWorker(ctx, pod.Name)
	if gerr == nil && existing.State == resumeapi.WorkerBusy {
		// keep busy state; just refresh IP/pool if changed
		if existing.PodIP != pod.Status.PodIP || existing.Pool != pool {
			existing.PodIP = pod.Status.PodIP
			existing.Pool = pool
			if err := r.KV.UpsertWorker(ctx, existing); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			// No entry change, but ensure pool membership is recorded (idempotent) —
			// self-heals busy workers that predate the pool all-set index.
			if err := r.KV.EnsurePoolMember(ctx, pod.Name, pool); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	w := &resumeapi.WorkerEntry{
		Pod:   pod.Name,
		Pool:  pool,
		PodIP: pod.Status.PodIP,
		State: resumeapi.WorkerIdle,
	}
	if gerr == nil {
		w.Version = existing.Version
	}
	if err := r.KV.UpsertWorker(ctx, w); err != nil {
		return ctrl.Result{}, err
	}
	log.V(1).Info("registered worker", "pod", pod.Name, "pool", pool, "ip", pod.Status.PodIP)
	return ctrl.Result{}, nil
}

func (r *WorkerDiscoveryReconciler) remove(ctx context.Context, pod, pool string) {
	if err := r.KV.RemoveWorker(ctx, pod, pool); err != nil {
		logf.Log.Error(err, "remove worker", "pod", pod)
	}
}

// removeByPod removes a worker entry when only the pod name is known (pod already
// deleted from the API). It reads the entry to learn the pool, then removes it.
func (r *WorkerDiscoveryReconciler) removeByPod(ctx context.Context, pod string) {
	w, err := r.KV.GetWorker(ctx, pod)
	if err != nil {
		return // already gone
	}
	r.remove(ctx, pod, w.Pool)
}

// PruneStaleWorkers removes KV worker entries whose pods no longer exist. This
// closes the gap where pod-delete events are MISSED — e.g. pods deleted while the
// operator was down/restarting (edge-triggered delete handling alone can't catch
// those). Level-triggered reconciliation against the live pod list is the durable
// fix (TDD §4.2 reconciliation). Returns the number pruned.
func (r *WorkerDiscoveryReconciler) PruneStaleWorkers(ctx context.Context) (int, error) {
	pods, err := r.KV.ListWorkerPods(ctx)
	if err != nil {
		return 0, err
	}
	return r.pruneStaleWorkers(ctx, pods), nil
}

// pruneStaleWorkers is PruneStaleWorkers over a pre-fetched pod list, so the
// periodic loop can share one ListWorkerPods scan with ReclaimOrphanBindings.
func (r *WorkerDiscoveryReconciler) pruneStaleWorkers(ctx context.Context, pods []string) int {
	pruned := 0
	for _, podName := range pods {
		var pod corev1.Pod
		gerr := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: podName}, &pod)
		if apierrors.IsNotFound(gerr) {
			// pod gone: prune its KV entry (and idle-set membership).
			r.removeByPod(ctx, podName)
			pruned++
		}
	}
	return pruned
}

// reclaimReason classifies why a busy binding is anomalous, or "" if it is healthy
// (its session is live and bound to THIS pod). The pod is assumed to exist + be
// Ready (dead pods are handled by PruneStaleWorkers). See the reclaim rule in
// docs/sandboxd/PRD/DESIGN-worker-binding-reclaim.md §4.1.
func (r *WorkerDiscoveryReconciler) reclaimReason(ctx context.Context, w *resumeapi.WorkerEntry) string {
	if w.SID == "" {
		return "no-sid" // busy with no session id at all — never a valid claim
	}
	se, err := r.KV.GetSession(ctx, w.SID)
	if errors.Is(err, assign.ErrNotFound) {
		return "orphan" // session entry gone (deleted/GC'd without releasing the worker)
	}
	if err != nil {
		return "" // transient KV error: don't act this pass
	}
	switch {
	case se.State == resumeapi.StateSuspended:
		// A Suspended session holds no worker; the binding leaked (ReleaseWorker lost).
		return "suspended"
	case se.WorkerPod != "" && se.WorkerPod != w.Pod:
		// The session rebound to another pod; this pod's binding is a leftover.
		return "rebound"
	default:
		return "" // Running/Resuming/Suspending bound here (or not yet bound): keep.
	}
}

// ReclaimOrphanBindings returns busy workers whose binding is provably orphaned
// (no session), points at a Suspended session, or is a leftover of a rebind — but
// only after the anomaly has persisted past ReclaimGrace with an unchanged worker
// version (so an in-flight claim->bind is never reclaimed). It moves such workers
// busy->idle via ReleaseWorker. Returns the number reclaimed. No-op if
// ReclaimGrace <= 0. See docs/sandboxd/PRD/DESIGN-worker-binding-reclaim.md.
func (r *WorkerDiscoveryReconciler) ReclaimOrphanBindings(ctx context.Context) (int, error) {
	if r.ReclaimGrace <= 0 {
		return 0, nil
	}
	pods, err := r.KV.ListWorkerPods(ctx)
	if err != nil {
		return 0, err
	}
	return r.reclaimOrphanBindings(ctx, pods), nil
}

// reclaimOrphanBindings is ReclaimOrphanBindings over a pre-fetched pod list, so the
// periodic loop can share one ListWorkerPods scan with PruneStaleWorkers.
func (r *WorkerDiscoveryReconciler) reclaimOrphanBindings(ctx context.Context, pods []string) int {
	if r.ReclaimGrace <= 0 {
		return 0
	}
	now := r.now()
	log := logf.FromContext(ctx).WithName("worker-reclaim")

	seen := make(map[string]bool, len(pods))
	reclaimed := 0
	for _, pod := range pods {
		w, gerr := r.KV.GetWorker(ctx, pod)
		if gerr != nil || w.State != resumeapi.WorkerBusy {
			continue // idle or gone: not our concern (idle self-heals via the informer)
		}
		reason := r.reclaimReason(ctx, w)
		if reason == "" {
			delete(r.anomalous, pod) // healthy now: disarm
			continue
		}
		seen[pod] = true

		// Two-strike + version-stable gate: arm on first observation, act only once
		// the anomaly has survived a prior sweep, the version is unchanged, and the
		// grace has elapsed.
		mark, armed := r.anomalous[pod]
		if !armed || mark.version != w.Version {
			r.anomalous[pod] = anomalyMark{firstSeen: now, version: w.Version}
			continue
		}
		if now.Sub(mark.firstSeen) < r.ReclaimGrace {
			continue // still within grace
		}

		if err := r.KV.ReleaseWorker(ctx, w.Pod, w.Pool); err != nil {
			log.Error(err, "reclaim: release worker", "pod", w.Pod, "pool", w.Pool)
			continue // retry next sweep
		}
		delete(r.anomalous, pod)
		if r.Notify != nil {
			r.Notify.PoolChanged(w.Pool) // busy->idle: refresh WarmPool status now
		}
		metrics.WorkerReclaimedTotal.WithLabelValues(reason).Inc()
		log.Info("reclaimed orphaned worker binding", "pod", w.Pod, "pool", w.Pool,
			"sid", w.SID, "reason", reason, "agedSec", int(now.Sub(mark.firstSeen).Seconds()))
		reclaimed++
	}
	// Forget marks for pods no longer busy/anomalous so the map can't grow unbounded.
	for pod := range r.anomalous {
		if !seen[pod] {
			delete(r.anomalous, pod)
		}
	}
	return reclaimed
}

func (r *WorkerDiscoveryReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// StartPruneLoop runs the level-triggered KV self-heal sweeps periodically (manager
// Runnable): PruneStaleWorkers (drop entries for dead pods) and ReclaimOrphanBindings
// (return stuck-busy workers to idle). Both are leader-gated with the reconciler.
func (r *WorkerDiscoveryReconciler) StartPruneLoop(ctx context.Context, interval time.Duration) error {
	if interval == 0 {
		interval = 30 * time.Second
	}
	if r.anomalous == nil {
		r.anomalous = map[string]anomalyMark{}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	log := logf.FromContext(ctx).WithName("worker-prune")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			// One ListWorkerPods scan per tick, shared by both sweeps (they iterate
			// the same worker:* keyspace).
			pods, err := r.KV.ListWorkerPods(ctx)
			if err != nil {
				log.Error(err, "list worker pods")
				continue
			}
			if n := r.pruneStaleWorkers(ctx, pods); n > 0 {
				log.Info("pruned stale worker entries", "count", n)
			}
			if r.ReclaimGrace > 0 {
				if n := r.reclaimOrphanBindings(ctx, pods); n > 0 {
					log.Info("reclaimed orphaned worker bindings", "count", n)
				}
			}
		}
	}
}

func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// SetupWithManager wires the pod watch. The pod cache is scoped to worker pods
// at the WATCH/ListWatch level in main.go (cache.Options.ByObject label selector),
// so the API server only ever streams sandboxd worker pods to this operator —
// cluster-wide pod churn of other pods never reaches our informer. (A predicate
// here would filter only reconcile events, NOT the watch, so it is intentionally
// omitted in favor of the cache-level selector.)
//
// Future scale option (see TDD): watch EndpointSlices of a per-pool headless
// Service instead of Pods — fewer objects, readiness precomputed, targetRef gives
// the pod name + IP directly. Deferred to a later hardening phase.
func (r *WorkerDiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Named("worker-discovery").
		Complete(r)
}
