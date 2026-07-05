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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
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

	// Terminating pod → treat as gone.
	if pod.DeletionTimestamp != nil {
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
