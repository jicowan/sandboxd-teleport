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
	"github.com/jicowan/aio-sandbox/controlplane/internal/snapshot"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// baseSnapshotFinalizer ensures a base's bases/<id>/ S3 objects are reclaimed when
// its CR is deleted — including a pinned base deleted directly (the reaper only
// reclaims S3 for bases IT deletes on unpinned+unreferenced). Without it, deleting
// a promoted base CR orphans its S3 objects (docs/sandboxd/PRD/PRD-snapshot-fork.md §5.4).
const baseSnapshotFinalizer = "sandboxd.io/base-snapshot-s3"

// snapDirOf returns the "<...>/<sid>/snap-N" directory of a snapshot URI (strips a
// trailing slash). A base is promoted from this dir.
func snapDirOf(uri string) string {
	if uri == "" {
		return ""
	}
	if uri[len(uri)-1] == '/' {
		return uri[:len(uri)-1]
	}
	return uri
}

// BaseSnapshotReconciler promotes a suspended session's snapshot into a fork-stable
// base (copy-on-promote) and records the restore identity (docs/sandboxd/PRD/PRD-snapshot-fork.md
// §5.1). It does NOT reclaim bases — that is the GC base reaper (§5.4), gated on
// pinned==false + refCount==0.
type BaseSnapshotReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// KV is the assignment table; the source session's snapshot + restore identity
	// are read from it (authoritative), falling back to Session.status.
	KV *assign.Client
	// Store performs the S3 copy-on-promote. nil disables promotion (the base stays
	// Pending) — useful in envtest without S3.
	Store *snapshot.Store
}

// +kubebuilder:rbac:groups=core.sandboxd.io,resources=basesnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=basesnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.sandboxd.io,resources=basesnapshots/finalizers,verbs=update

// Reconcile promotes the source session's snapshot once; subsequent passes are
// no-ops (a base, once Ready, is immutable — its snapshotURI never changes).
func (r *BaseSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var base corev1alpha1.BaseSnapshot
	if err := r.Get(ctx, req.NamespacedName, &base); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: reclaim the base's S3 objects, then drop the finalizer. This covers
	// a base deleted directly (incl. pinned) — the reaper only S3-reclaims bases it
	// deletes itself. Idempotent: Reclaim over an already-empty prefix is a no-op.
	if !base.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&base, baseSnapshotFinalizer) {
			if r.Store != nil {
				if err := r.Store.Reclaim(ctx, base.Name); err != nil {
					log.Error(err, "reclaim base S3 on delete", "base", base.Name)
					return ctrl.Result{}, err // retry; keep the finalizer until S3 is clear
				}
			}
			controllerutil.RemoveFinalizer(&base, baseSnapshotFinalizer)
			if err := r.Update(ctx, &base); err != nil {
				// Benign 409 (CR changed under us on delete) — requeue quietly, don't
				// log an ERROR + stack trace (issue #4).
				if apierrors.IsConflict(err) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, err
			}
			log.Info("reclaimed base snapshot on delete", "base", base.Name)
		}
		return ctrl.Result{}, nil
	}

	// Ensure the finalizer is present on a live base (so a future delete reclaims S3).
	// Return-and-requeue after adding it so promotion runs in a fresh reconcile
	// against a stable object — avoids racing this metadata write's own watch event
	// with the promote status update (which would conflict).
	if !controllerutil.ContainsFinalizer(&base, baseSnapshotFinalizer) {
		controllerutil.AddFinalizer(&base, baseSnapshotFinalizer)
		if err := r.Update(ctx, &base); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Once promoted, a base is immutable — nothing to do. (Reclaim is the GC reaper.)
	if base.Status.Ready {
		return ctrl.Result{}, nil
	}

	// Resolve the source session's snapshot + restore identity (KV authoritative).
	src, err := r.resolveSource(ctx, base.Namespace, base.Spec.SourceSessionRef.Name)
	if err != nil {
		r.fail(ctx, &base, "SourceUnresolved", err.Error())
		return ctrl.Result{}, nil
	}

	if r.Store == nil {
		// No S3 configured (envtest): record the resolved identity but stay Pending;
		// the base is not forkable until copy-on-promote runs.
		base.Status.Phase = corev1alpha1.BaseSnapshotPending
		applySourceIdentity(&base.Status, src)
		r.setReady(ctx, &base, metav1.ConditionFalse, "PromotePending", "S3 store not configured")
		_ = r.Status().Update(ctx, &base)
		return ctrl.Result{}, nil
	}

	// Copy-on-promote: server-side copy sandboxes/<src>/snap-… -> bases/<name>/….
	res, err := r.Store.Promote(ctx, base.Name, snapDirOf(src.SnapshotURI))
	if err != nil {
		r.fail(ctx, &base, "PromoteFailed", err.Error())
		return ctrl.Result{}, err // retry
	}

	applySourceIdentity(&base.Status, src)
	base.Status.SnapshotURI = res.SnapshotURI
	base.Status.Ready = true
	base.Status.Phase = corev1alpha1.BaseSnapshotReady
	r.setReady(ctx, &base, metav1.ConditionTrue, "Promoted",
		fmt.Sprintf("copied %d objects to %s", res.Objects, res.SnapshotURI))
	if err := r.Status().Update(ctx, &base); err != nil {
		if apierrors.IsConflict(err) {
			// A concurrent reconcile (e.g. the finalizer-add write's watch event)
			// modified the object between our read and this write. Expected and
			// self-healing: requeue and recompute against the latest object. The
			// promote itself already happened (S3 copy is idempotent), so the next
			// pass just re-writes status.
			return ctrl.Result{Requeue: true}, nil
		}
		log.Error(err, "update basesnapshot status")
		return ctrl.Result{}, err
	}
	log.Info("promoted base snapshot", "base", base.Name, "snapshotURI", res.SnapshotURI, "objects", res.Objects)
	return ctrl.Result{}, nil
}

// sourceIdentity is the restore identity resolved from the source session.
type sourceIdentity struct {
	SnapshotURI  string
	Image        string
	Digest       string
	Ports        []corev1alpha1.PortMap
	Health       *corev1alpha1.Health
	IAMRoleARN   string
	RunscVersion string
}

// resolveSource reads the source session's current snapshot + restore identity. The
// source MUST be Suspended with a snapshotURI (that's the promotable state). KV is
// authoritative; Session.status is the fallback (e.g. after a Valkey wipe).
func (r *BaseSnapshotReconciler) resolveSource(ctx context.Context, ns, sid string) (sourceIdentity, error) {
	if r.KV != nil {
		if e, err := r.KV.GetSession(ctx, sid); err == nil {
			if e.State != resumeapi.StateSuspended || e.SnapshotURI == "" {
				return sourceIdentity{}, fmt.Errorf("source session %q not Suspended-with-snapshot (state=%q)", sid, e.State)
			}
			return sourceIdentity{
				SnapshotURI: e.SnapshotURI,
				Image:       e.Image,
				Digest:      e.Digest,
				Ports:       portsToCRD(e.Ports),
				Health:      healthToCRD(e.Health),
				IAMRoleARN:  e.IAMRoleARN,
			}, nil
		}
	}
	// Fallback: durable Session.status mirror.
	var s corev1alpha1.Session
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: sid}, &s); err != nil {
		return sourceIdentity{}, fmt.Errorf("source session %q: %w", sid, err)
	}
	if s.Status.Phase != resumeapi.StateSuspended || s.Status.SnapshotURI == "" {
		return sourceIdentity{}, fmt.Errorf("source session %q not Suspended-with-snapshot (phase=%q)", sid, s.Status.Phase)
	}
	return sourceIdentity{
		SnapshotURI: s.Status.SnapshotURI,
		Image:       s.Status.Image,
		Digest:      s.Status.Digest,
		Ports:       s.Status.Ports,
		Health:      s.Status.Health,
		IAMRoleARN:  s.Status.IAMRoleARN,
	}, nil
}

func applySourceIdentity(st *corev1alpha1.BaseSnapshotStatus, src sourceIdentity) {
	st.Image = src.Image
	st.Digest = src.Digest
	st.Ports = src.Ports
	st.Health = src.Health
	st.IAMRoleARN = src.IAMRoleARN
	st.RunscVersion = src.RunscVersion
}

func (r *BaseSnapshotReconciler) fail(ctx context.Context, base *corev1alpha1.BaseSnapshot, reason, msg string) {
	base.Status.Phase = corev1alpha1.BaseSnapshotFailed
	base.Status.Ready = false
	r.setReady(ctx, base, metav1.ConditionFalse, reason, msg)
	_ = r.Status().Update(ctx, base)
}

func (r *BaseSnapshotReconciler) setReady(ctx context.Context, base *corev1alpha1.BaseSnapshot, status metav1.ConditionStatus, reason, msg string) {
	upsertCondition(&base.Status.Conditions, "Ready", status, reason, msg, base.Generation)
}

// SetupWithManager registers the reconciler.
func (r *BaseSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.BaseSnapshot{}).
		Named("basesnapshot").
		Complete(r)
}
