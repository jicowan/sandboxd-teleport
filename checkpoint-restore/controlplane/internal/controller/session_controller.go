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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
)

// sessionFinalizer guards KV cleanup on Session CR deletion: it holds the object in
// the apiserver until the reconciler has released the session's worker binding and
// deleted its authoritative Valkey entry (docs/PRD-snapshot-fork.md — CR-delete
// cleanup gap). Without it, deleting a Session (or a ForkSet, which cascades to its
// child Sessions) strands the worker as busy and the pool never scales back down.
const sessionFinalizer = "core.sandboxd.io/session-kv-cleanup"

// SuspendController performs an on-demand checkpoint+suspend of one session. It is
// satisfied by resume.Suspender.SuspendNow; kept as a narrow interface so the
// reconciler is unit-testable without a live worker/KV. SuspendNow is idempotent by
// state (a no-op + nil if the session isn't Running).
type SuspendController interface {
	SuspendNow(ctx context.Context, sid string) error
	// ReleaseForDelete tears down the session's KV footprint (release its worker to
	// the idle pool + delete the session entry) when the CR is being deleted. It does
	// NOT checkpoint — a deleted session is discarded. Idempotent: a missing entry is
	// success. Satisfied by resume.Suspender.ReleaseForDelete.
	ReleaseForDelete(ctx context.Context, sid string) error
}

// SessionReconciler reconciles the on-demand suspend request on a Session
// (docs/PRD-on-demand-suspend.md). It is the ONLY controller that watches Session
// (Sessions are otherwise lazy-created + status-mirrored). It is deliberately
// minimal: it acts only on the edge-triggered suspend request and touches nothing
// else about the Session, so it never fights reactive resume or the durability
// mirror.
type SessionReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Suspend SuspendController
	// Release tears down a session's KV footprint on CR deletion (finalizer path).
	// Same implementation as Suspend (resume.Suspender); a distinct field keeps the
	// two concerns explicit and the reconciler unit-testable.
	Release SuspendController
}

// +kubebuilder:rbac:groups=core.sandboxd.io,resources=sessions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=sessions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=sessions/finalizers,verbs=update

// Reconcile handles two concerns on a Session:
//   - Deletion: a finalizer ensures the session's worker binding + KV entry are
//     released before the CR disappears (so a delete never strands a busy worker).
//   - The edge-triggered spec.suspendRequest: when it differs from
//     status.lastSuspendHandled, checkpoint+suspend once, then advance the watermark.
func (r *SessionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var s corev1alpha1.Session
	if err := r.Get(ctx, req.NamespacedName, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- Deletion path: release the KV footprint, then drop the finalizer. ---
	if !s.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&s, sessionFinalizer) {
			if err := r.Release.ReleaseForDelete(ctx, s.Name); err != nil {
				// Keep the finalizer and requeue so the worker is eventually freed;
				// never let a KV/worker error orphan the binding silently.
				log.Error(err, "release session KV on delete failed; will retry", "session", s.Name)
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&s, sessionFinalizer)
			if err := r.Update(ctx, &s); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			log.Info("released session KV footprint on delete", "session", s.Name)
		}
		return ctrl.Result{}, nil
	}

	// --- Live path: ensure the finalizer is present so a future delete cleans up. ---
	if controllerutil.AddFinalizer(&s, sessionFinalizer) {
		if err := r.Update(ctx, &s); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		// Re-fetch on the next reconcile with the finalizer persisted; continue this
		// pass against the in-memory copy for the suspend logic below.
	}

	reqTok := s.Spec.SuspendRequest
	// No request, or already handled -> nothing to do. (Empty token is never a
	// request; the watermark also starts empty, so an unset field is a clean no-op.)
	if reqTok == "" || reqTok == s.Status.LastSuspendHandled {
		return ctrl.Result{}, nil
	}

	// Perform exactly one checkpoint+suspend. SuspendNow is idempotent by state: if
	// the session isn't Running (already Suspended/Absent, or reactively resumed and
	// then gone again), it no-ops and returns nil, and we still advance the watermark
	// so the one-shot request is considered satisfied at this token.
	if err := r.Suspend.SuspendNow(ctx, s.Name); err != nil {
		// Leave the watermark unchanged so the request is retried; surface a condition.
		r.setSuspendCond(ctx, &s, metav1.ConditionFalse, "SuspendFailed", err.Error())
		return ctrl.Result{}, err // requeue with backoff
	}

	// Success: advance the watermark to the handled token. Advancing it is what makes
	// the request one-shot — reactive resume may bring the session back to Running,
	// and it will NOT be re-suspended because the token now matches.
	s.Status.LastSuspendHandled = reqTok
	r.setSuspendCond(ctx, &s, metav1.ConditionTrue, "Suspended", "checkpoint+suspend completed")
	if err := r.Status().Update(ctx, &s); err != nil {
		if apierrors.IsConflict(err) {
			// Concurrent status write (e.g. the durability mirror). Requeue and
			// recompute against the latest object; the suspend already happened
			// (idempotent), so the next pass just re-advances the watermark.
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	log.Info("on-demand suspend handled", "session", s.Name, "token", reqTok)
	return ctrl.Result{}, nil
}

// setSuspendCond sets a SuspendRequest condition for human/kubectl visibility (the
// watermark is the machine signal). Best-effort in-memory mutation; persisted by the
// caller's Status().Update, or standalone on the failure path.
func (r *SessionReconciler) setSuspendCond(ctx context.Context, s *corev1alpha1.Session, status metav1.ConditionStatus, reason, msg string) {
	cond := metav1.Condition{
		Type:               "SuspendRequest",
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: s.Generation,
		LastTransitionTime: metav1.Now(),
	}
	replaced := false
	for i := range s.Status.Conditions {
		if s.Status.Conditions[i].Type == "SuspendRequest" {
			if s.Status.Conditions[i].Status == status && s.Status.Conditions[i].Reason == reason {
				cond.LastTransitionTime = s.Status.Conditions[i].LastTransitionTime
			}
			s.Status.Conditions[i] = cond
			replaced = true
			break
		}
	}
	if !replaced {
		s.Status.Conditions = append(s.Status.Conditions, cond)
	}
	// On the failure path there is no follow-up Status().Update, so persist here.
	if status == metav1.ConditionFalse {
		_ = r.Status().Update(ctx, s)
	}
}

// SetupWithManager registers the reconciler on Session.
func (r *SessionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Session{}).
		Named("session").
		Complete(r)
}
