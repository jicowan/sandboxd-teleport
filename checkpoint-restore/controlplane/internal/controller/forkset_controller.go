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
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// Materializer proactively drives a session to Running (Eager activation), i.e.
// the operator's resume workflow. Best-effort and idempotent (resume is
// singleflight + CAS): a second call for an already-Running session is a no-op.
// nil disables Eager materialization (children then materialize lazily on first
// client contact, exactly like any Absent session).
type Materializer func(ctx context.Context, sid, subject, pool string) error

// ForkSetReconciler fans a ForkSet out into N child Session CRs and rolls their
// readiness up into status (docs/PRD-snapshot-fork.md §5.2). This increment
// implements the IMAGE source (baseRef unset): children are plain pool-backed
// Sessions that cold-start (/run) via the normal resume path. The snapshot source
// (baseRef set) is surfaced as a not-yet-implemented condition.
type ForkSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// KV is the assignment table, read to determine child readiness (a child is
	// "ready" when its KV entry is Running). Optional: nil (envtest) skips readiness
	// so ready stays 0 and phase stays Progressing.
	KV *assign.Client
	// Materialize drives Eager children to Running. Optional (nil = lazy only).
	Materialize Materializer
	// inflight dedups Eager materialization: a child SID present here has a resume
	// goroutine in progress, so the 5s requeue loop must NOT fire another (each
	// resume claims an idle worker — re-firing a slow restore is a worker-claim
	// storm). Cleared when the goroutine returns.
	inflight sync.Map // sid -> struct{}
}

// +kubebuilder:rbac:groups=core.sandboxd.io,resources=forksets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=forksets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=forksets/finalizers,verbs=update

// Reconcile drives a ForkSet toward N child Sessions and refreshes status.
func (r *ForkSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var fs corev1alpha1.ForkSet
	if err := r.Get(ctx, req.NamespacedName, &fs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Source selection is baseRef XOR appRef XOR neither (dedicated-pool image).
	if fs.Spec.BaseRef != nil && fs.Spec.AppRef != nil {
		r.setReady(ctx, &fs, metav1.ConditionFalse, "SourceConflict",
			"baseRef and appRef are mutually exclusive")
		fs.Status.Phase = corev1alpha1.ForkSetProgressing
		fs.Status.Desired = fs.Spec.Count
		_ = r.Status().Update(ctx, &fs)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	// appRef source: the AppTemplate must exist before we fan out (fail fast with a
	// clear condition rather than N children erroring at resume). The child admission
	// (resolveWorkloadSource) additionally enforces that the pool is generic.
	if fs.Spec.AppRef != nil {
		var app corev1alpha1.AppTemplate
		if err := r.Get(ctx, types.NamespacedName{Namespace: fs.Namespace, Name: fs.Spec.AppRef.Name}, &app); err != nil {
			r.setReady(ctx, &fs, metav1.ConditionFalse, "AppUnresolved",
				fmt.Sprintf("appRef %q: %v", fs.Spec.AppRef.Name, err))
			fs.Status.Phase = corev1alpha1.ForkSetProgressing
			fs.Status.Desired = fs.Spec.Count
			_ = r.Status().Update(ctx, &fs)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	// Resolve the source. baseRef set => snapshot source: the BaseSnapshot must be
	// Ready before we can seed children from it. baseRef unset => image source.
	var base *corev1alpha1.BaseSnapshot
	if fs.Spec.BaseRef != nil {
		var b corev1alpha1.BaseSnapshot
		if err := r.Get(ctx, types.NamespacedName{Namespace: fs.Namespace, Name: fs.Spec.BaseRef.Name}, &b); err != nil {
			r.setReady(ctx, &fs, metav1.ConditionFalse, "BaseUnresolved",
				fmt.Sprintf("baseRef %q: %v", fs.Spec.BaseRef.Name, err))
			fs.Status.Phase = corev1alpha1.ForkSetProgressing
			fs.Status.Desired = fs.Spec.Count
			_ = r.Status().Update(ctx, &fs)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		if !b.Status.Ready {
			r.setReady(ctx, &fs, metav1.ConditionFalse, "BaseNotReady",
				fmt.Sprintf("baseRef %q not Ready (copy-on-promote pending)", fs.Spec.BaseRef.Name))
			fs.Status.Phase = corev1alpha1.ForkSetProgressing
			fs.Status.Desired = fs.Spec.Count
			_ = r.Status().Update(ctx, &fs)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		// Snapshot-source cross-object check: the target pool must resolve to a
		// SandboxTemplate. A bogus pool otherwise fails opaquely later (the seeded
		// children never place). Fail fast with a clear condition instead.
		//
		// NOTE: the PRD also wants a runsc-VERSION match (base vs pool), but that is
		// not computable from the CRDs today — base.status.runscVersion is never
		// populated (session state carries no runsc) and a pool exposes no runsc
		// (it's implicit in the worker image). The worker's /restore 409s a genuine
		// runsc mismatch as the backstop; threading runsc through
		// checkpoint→session→base is future work.
		if _, terr := templateForPool(ctx, r.Client, fs.Namespace, fs.Spec.Pool); terr != nil {
			r.setReady(ctx, &fs, metav1.ConditionFalse, "PoolUnresolved",
				fmt.Sprintf("pool %q does not resolve to a SandboxTemplate: %v", fs.Spec.Pool, terr))
			fs.Status.Phase = corev1alpha1.ForkSetProgressing
			fs.Status.Desired = fs.Spec.Count
			_ = r.Status().Update(ctx, &fs)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		base = &b
	}

	desired := childNames(&fs)

	// Create any missing child Sessions (idempotent). Image source: a plain
	// pool-backed Session, born Absent, cold-starting on first contact. Snapshot
	// source: additionally seed the child's KV entry Suspended+snapshotURI so the
	// existing resume path /restores it from the base.
	for _, name := range desired {
		if err := r.ensureChild(ctx, &fs, name, base); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Reap children beyond the current count (scale-down). Deleting the child
	// Session CR is enough here: session GC's abandoned pass reaps its KV entry.
	if err := r.reapExtraChildren(ctx, &fs, desired); err != nil {
		log.Error(err, "reap extra fork children")
	}

	// Eager activation: proactively materialize any child not yet Running. Fired as
	// best-effort background work so a slow cold start doesn't block the reconcile;
	// readiness is observed from KV on the next pass.
	ready := r.materializeAndCountReady(ctx, &fs, desired)

	// Roll up status.
	fs.Status.Desired = fs.Spec.Count
	fs.Status.Ready = ready
	fs.Status.Forks = desired
	if ready >= fs.Spec.Count {
		fs.Status.Phase = corev1alpha1.ForkSetReady
		r.setReady(ctx, &fs, metav1.ConditionTrue, "AllForksReady",
			fmt.Sprintf("%d/%d forks Running", ready, fs.Spec.Count))
	} else {
		fs.Status.Phase = corev1alpha1.ForkSetProgressing
		r.setReady(ctx, &fs, metav1.ConditionFalse, "Progressing",
			fmt.Sprintf("%d/%d forks Running", ready, fs.Spec.Count))
	}
	if err := r.Status().Update(ctx, &fs); err != nil {
		if apierrors.IsConflict(err) {
			// A concurrent reconcile (requeue / owned-Session event) updated the
			// ForkSet between our Get and this write. Expected and self-healing:
			// just requeue and recompute against the latest object — not an error.
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		log.Error(err, "update forkset status")
	}

	// Requeue while progressing so KV-observed readiness converges without waiting
	// for an external event (child Sessions don't emit events on KV transitions).
	if fs.Status.Phase != corev1alpha1.ForkSetReady {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// childNames returns the deterministic child session ids for a ForkSet:
// sess-fork-<prefix>-<n>, prefix defaulting to the ForkSet name.
func childNames(fs *corev1alpha1.ForkSet) []string {
	prefix := fs.Spec.NamePrefix
	if prefix == "" {
		prefix = fs.Name
	}
	names := make([]string, 0, fs.Spec.Count)
	for i := int32(0); i < fs.Spec.Count; i++ {
		names = append(names, fmt.Sprintf("sess-fork-%s-%d", prefix, i))
	}
	return names
}

// ensureChild creates the child Session if absent (owned by the ForkSet), and for
// the snapshot source seeds its KV entry Suspended+snapshotURI so the existing
// resume path restores it from the base. base is nil for the image source.
func (r *ForkSetReconciler) ensureChild(ctx context.Context, fs *corev1alpha1.ForkSet, name string, base *corev1alpha1.BaseSnapshot) error {
	var existing corev1alpha1.Session
	err := r.Get(ctx, types.NamespacedName{Namespace: fs.Namespace, Name: name}, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	created := apierrors.IsNotFound(err)
	if created {
		forkFrom := &corev1alpha1.ForkProvenance{} // image source: no base
		if base != nil {
			forkFrom = &corev1alpha1.ForkProvenance{
				BaseRef:     &corev1alpha1.LocalRef{Name: base.Name},
				SnapshotURI: base.Status.SnapshotURI,
			}
		}
		labels := map[string]string{
			LabelCreatedBy: CreatedByOperator, // operator-owned → GC may reap
			LabelForkSet:   fs.Name,
		}
		if base != nil {
			labels[LabelForkBase] = base.Name // lets the base reaper derive refCount
		}
		// appRef source: stamp it onto the child so the resume path runs that
		// AppTemplate on the (generic) pool — exactly like a standalone appRef Session.
		// Only for the image path (base == nil); a snapshot child restores its image
		// from the base and needs no appRef.
		var appRef *corev1alpha1.LocalRef
		if base == nil && fs.Spec.AppRef != nil {
			appRef = &corev1alpha1.LocalRef{Name: fs.Spec.AppRef.Name}
		}
		child := &corev1alpha1.Session{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: fs.Namespace,
				Labels:    labels,
			},
			Spec: corev1alpha1.SessionSpec{
				PoolRef:   &corev1alpha1.LocalRef{Name: fs.Spec.Pool},
				AppRef:    appRef,
				Subject:   fs.Spec.Subject,
				IAM:       fs.Spec.IAM,
				Lifecycle: fs.Spec.Lifecycle,
				ForkFrom:  forkFrom,
			},
		}
		if err := controllerutil.SetControllerReference(fs, child, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, child); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create fork child %q: %w", name, err)
		}
	}

	// Snapshot source: seed the child's KV entry as Suspended pointing at the base,
	// so the resume path takes the /restore branch (resume.go RESTORE branch keys on
	// State==Suspended && SnapshotURI!=""). Idempotent: skip if an entry already
	// exists (a materialized fork has advanced past Suspended and must not be reset).
	if base != nil && r.KV != nil {
		if _, gerr := r.KV.GetSession(ctx, name); gerr == assign.ErrNotFound {
			iamRole := base.Status.IAMRoleARN
			if fs.Spec.IAM != nil && fs.Spec.IAM.RoleARN != "" {
				iamRole = fs.Spec.IAM.RoleARN // ForkSet iam overrides the base's
			}
			seed := &resumeapi.SessionEntry{
				SID:                name,
				State:              resumeapi.StateSuspended,
				Pool:               fs.Spec.Pool,
				Image:              base.Status.Image,
				SnapshotURI:        base.Status.SnapshotURI,
				Ports:              portsFromCRD(base.Status.Ports),
				Health:             healthFromCRD(base.Status.Health),
				IAMRoleARN:         iamRole,
				IdleTimeoutSeconds: fs.Spec.Lifecycle.IdleTimeoutSeconds,
			}
			if err := r.KV.PutSessionCAS(ctx, seed); err != nil && err != assign.ErrVersionConflict {
				return fmt.Errorf("seed fork child KV %q: %w", name, err)
			}
		}
	}
	return nil
}

// reapExtraChildren deletes owned child Sessions whose name is no longer in the
// desired set (scale-down). Session GC reaps each child's KV entry.
func (r *ForkSetReconciler) reapExtraChildren(ctx context.Context, fs *corev1alpha1.ForkSet, desired []string) error {
	want := map[string]bool{}
	for _, n := range desired {
		want[n] = true
	}
	var children corev1alpha1.SessionList
	if err := r.List(ctx, &children, client.InNamespace(fs.Namespace),
		client.MatchingLabels{LabelForkSet: fs.Name}); err != nil {
		return err
	}
	for i := range children.Items {
		c := &children.Items[i]
		if want[c.Name] {
			continue
		}
		if err := r.Delete(ctx, c); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// materializeAndCountReady fires Eager materialization for not-yet-Running children
// and returns the number of children currently Running (read from KV). Without KV
// (envtest) it returns 0.
func (r *ForkSetReconciler) materializeAndCountReady(ctx context.Context, fs *corev1alpha1.ForkSet, desired []string) int32 {
	var ready int32
	for _, sid := range desired {
		running := false
		if r.KV != nil {
			if e, err := r.KV.GetSession(ctx, sid); err == nil && e.State == resumeapi.StateRunning {
				running = true
			}
		}
		if running {
			ready++
			continue
		}
		if fs.Spec.Activation == corev1alpha1.ActivationEager && r.Materialize != nil {
			// Dedup: skip if a resume for this child is already in flight, else the
			// 5s requeue loop fires a fresh resume (and a fresh worker claim) every
			// pass while a slow restore is still running — a worker-claim storm.
			if _, busy := r.inflight.LoadOrStore(sid, struct{}{}); busy {
				continue
			}
			// Best-effort, non-blocking: a slow cold start must not stall reconcile.
			go func(id string) {
				defer r.inflight.Delete(id)
				mctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				_ = r.Materialize(mctx, id, fs.Spec.Subject, fs.Spec.Pool)
			}(sid)
		}
	}
	return ready
}

// setReady sets the Ready condition on the ForkSet (best-effort; mirrors the
// WarmPool pattern).
func (r *ForkSetReconciler) setReady(ctx context.Context, fs *corev1alpha1.ForkSet, status metav1.ConditionStatus, reason, msg string) {
	upsertCondition(&fs.Status.Conditions, "Ready", status, reason, msg, fs.Generation)
}

// SetupWithManager registers the reconciler; it owns the child Sessions.
func (r *ForkSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.ForkSet{}).
		Owns(&corev1alpha1.Session{}).
		Named("forkset").
		Complete(r)
}
