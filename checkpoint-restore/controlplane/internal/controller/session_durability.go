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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
	"github.com/jicowan/aio-sandbox/shared/sbxapi"
)

// SessionMirror durably mirrors the KV assignment entry into Session.status
// (etcd), so the Valkey cache can be rebuilt after a restart
// (PRD-durable-assignment-state). It implements resume.SessionMirror. All writes
// are best-effort: a failure is logged, never propagated — KV remains the
// authoritative write path in normal operation, and the mirror self-corrects on
// the next transition or the periodic rebuild.
type SessionMirror struct {
	client    client.Client
	namespace string
}

// NewSessionMirror builds a mirror over the operator's cached client. namespace is
// where Session CRs live (the resume namespace).
func NewSessionMirror(c client.Client, namespace string) *SessionMirror {
	return &SessionMirror{client: c, namespace: namespace}
}

// Mirror writes the session entry's durable fields into Session.status. If the
// Session CR doesn't exist yet (e.g. an in-flight lazy create raced), it is
// skipped — planFor creates the CR, and the next transition mirrors it.
func (m *SessionMirror) Mirror(ctx context.Context, e *resumeapi.SessionEntry) {
	if e == nil || e.SID == "" {
		return
	}
	log := logf.FromContext(ctx)
	var s corev1alpha1.Session
	if err := m.client.Get(ctx, types.NamespacedName{Namespace: m.namespace, Name: e.SID}, &s); err != nil {
		if !apierrors.IsNotFound(err) {
			log.V(1).Info("session mirror: get failed", "sid", e.SID, "err", err)
		}
		return // absent: planFor owns creation; a later transition will mirror
	}
	applyEntryToStatus(e, &s.Status)
	if err := m.client.Status().Update(ctx, &s); err != nil {
		log.V(1).Info("session mirror: status update failed", "sid", e.SID, "err", err)
	}
}

// Delete clears the durable record when a session is discarded (reset). We clear
// status back to Absent rather than deleting the CR — the CR may be operator- or
// user-owned (lazy-created vs. declared), and an Absent phase is the honest
// durable state. GC of the CR itself is a separate concern.
func (m *SessionMirror) Delete(ctx context.Context, sid string) {
	if sid == "" {
		return
	}
	log := logf.FromContext(ctx)
	var s corev1alpha1.Session
	if err := m.client.Get(ctx, types.NamespacedName{Namespace: m.namespace, Name: sid}, &s); err != nil {
		return
	}
	s.Status = corev1alpha1.SessionStatus{Phase: resumeapi.StateAbsent, Conditions: s.Status.Conditions}
	if err := m.client.Status().Update(ctx, &s); err != nil {
		log.V(1).Info("session mirror: delete(status->Absent) failed", "sid", sid, "err", err)
	}
}

// SessionReaper removes the durable Session CR when GC decides a session is dead
// (PRD-session-garbage-collection §6.2). It is the CR-deleting counterpart to
// SessionMirror: where the mirror keeps status in sync and the mirror's Delete
// only tombstones to Absent (reset), the reaper actually deletes the object — but
// ONLY when the CR is operator-owned (lazy-created, carrying
// LabelCreatedBy=CreatedByOperator). A user-declared Session is never deleted by
// GC; the reaper tombstones it to Absent instead, leaving the object the user owns
// in place. All operations are best-effort and idempotent (a partial reap self-
// heals on the next GC sweep).
type SessionReaper struct {
	client    client.Client
	namespace string
	dryRun    bool
}

// NewSessionReaper builds a reaper. dryRun=true logs the intended action without
// mutating anything (validation before arming; PRD §7/§8).
func NewSessionReaper(c client.Client, namespace string, dryRun bool) *SessionReaper {
	return &SessionReaper{client: c, namespace: namespace, dryRun: dryRun}
}

// Reap removes (operator-owned) or tombstones (user-owned) the Session CR for sid.
// Returns (deleted, error): deleted=true when an operator-owned CR was (or, in dry-
// run, would be) deleted; false when the CR was tombstoned, absent, or user-owned.
// Missing CR is not an error (already gone).
func (r *SessionReaper) Reap(ctx context.Context, sid string) (bool, error) {
	if sid == "" {
		return false, nil
	}
	log := logf.FromContext(ctx).WithName("session-reaper")
	var s corev1alpha1.Session
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: sid}, &s); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil // already gone
		}
		return false, err
	}
	operatorOwned := s.Labels[LabelCreatedBy] == CreatedByOperator
	if !operatorOwned {
		// User-declared CR: never delete; tombstone to Absent so it stops looking live.
		if s.Status.Phase == resumeapi.StateAbsent {
			return false, nil
		}
		if r.dryRun {
			log.Info("dry-run: would tombstone user-owned Session to Absent", "sid", sid)
			return false, nil
		}
		s.Status = corev1alpha1.SessionStatus{Phase: resumeapi.StateAbsent, Conditions: s.Status.Conditions}
		if err := r.client.Status().Update(ctx, &s); err != nil {
			return false, err
		}
		return false, nil
	}
	if r.dryRun {
		log.Info("dry-run: would DELETE operator-owned Session CR", "sid", sid, "phase", s.Status.Phase)
		return true, nil
	}
	// Delete guarded by resourceVersion (Preconditions) so we never race a
	// concurrent update into deleting a CR that just came back to life.
	if err := r.client.Delete(ctx, &s, client.Preconditions{
		UID:             &s.UID,
		ResourceVersion: &s.ResourceVersion,
	}); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return false, nil // already gone / changed under us; next sweep re-evaluates
		}
		return false, err
	}
	log.Info("reaped operator-owned Session CR", "sid", sid, "phase", s.Status.Phase)
	return true, nil
}

// applyEntryToStatus maps a KV SessionEntry onto Session.status (the lossless
// durable mirror). lastActiveAt is mirrored coarsely (it rides transitions here,
// not every router stamp) to avoid write amplification.
func applyEntryToStatus(e *resumeapi.SessionEntry, st *corev1alpha1.SessionStatus) {
	st.Phase = e.State
	st.WorkerPodIP = e.WorkerPodIP
	st.WorkerPod = e.WorkerPod
	st.SnapshotURI = e.SnapshotURI
	st.Image = e.Image
	st.Digest = e.Digest
	st.Pool = e.Pool
	st.IAMRoleARN = e.IAMRoleARN
	st.Ports = portsToCRD(e.Ports)
	st.Health = healthToCRD(e.Health)
	if e.LastActiveAt > 0 {
		t := metav1.NewTime(time.UnixMilli(e.LastActiveAt))
		st.LastActiveAt = &t
	}
}

// statusToEntry is the inverse: reconstruct a KV SessionEntry from a durable
// Session.status (used by the rebuild). Version 0 so it writes cleanly into a
// wiped cache.
func statusToEntry(sid string, st *corev1alpha1.SessionStatus) *resumeapi.SessionEntry {
	e := &resumeapi.SessionEntry{
		SID:         sid,
		State:       st.Phase,
		Pool:        st.Pool,
		WorkerPodIP: st.WorkerPodIP,
		WorkerPod:   st.WorkerPod,
		Image:       st.Image,
		Digest:      st.Digest,
		SnapshotURI: st.SnapshotURI,
		IAMRoleARN:  st.IAMRoleARN,
		Ports:       portsFromCRD(st.Ports),
		Health:      healthFromCRD(st.Health),
		Version:     0,
	}
	if st.LastActiveAt != nil {
		e.LastActiveAt = st.LastActiveAt.Time.UnixMilli()
	}
	return e
}

func portsToCRD(in []sbxapi.PortMap) []corev1alpha1.PortMap {
	if len(in) == 0 {
		return nil
	}
	out := make([]corev1alpha1.PortMap, len(in))
	for i, p := range in {
		out[i] = corev1alpha1.PortMap{Container: p.Container, Host: p.Host}
	}
	return out
}

func healthToCRD(h *sbxapi.Health) *corev1alpha1.Health {
	if h == nil {
		return nil
	}
	return &corev1alpha1.Health{
		RestartPolicy: h.RestartPolicy,
		Probe:         h.Probe,
		ProbePort:     h.ProbePort,
		ProbePath:     h.ProbePath,
	}
}

func healthFromCRD(h *corev1alpha1.Health) *sbxapi.Health {
	if h == nil {
		return nil
	}
	return &sbxapi.Health{
		RestartPolicy: h.RestartPolicy,
		Probe:         h.Probe,
		ProbePort:     h.ProbePort,
		ProbePath:     h.ProbePath,
	}
}

// SessionRebuilder repopulates the Valkey session cache from the durable Session
// CRs on operator startup (and, if wired, periodically). It is the recovery half
// of PRD-durable-assignment-state: when Valkey is wiped, the operator reconstructs
// the assignment index from etcd so suspended sessions can still teleport-resume.
//
// Worker entries and the idle set are NOT rebuilt here — they self-heal via the
// pod-label informer + prune loop.
type SessionRebuilder struct {
	client    client.Client
	kv        *assign.Client
	namespace string
}

// NewSessionRebuilder builds the rebuilder.
func NewSessionRebuilder(c client.Client, kv *assign.Client, namespace string) *SessionRebuilder {
	return &SessionRebuilder{client: c, kv: kv, namespace: namespace}
}

// RebuildOnce lists Session CRs and writes any missing/stale session:<sid> KV
// entry from the durable status. Idempotent: a session already present with a
// non-zero version is left alone (KV is authoritative during normal operation);
// only absent (wiped) entries are repopulated. Returns the number rebuilt.
func (r *SessionRebuilder) RebuildOnce(ctx context.Context) (int, error) {
	var list corev1alpha1.SessionList
	if err := r.client.List(ctx, &list, client.InNamespace(r.namespace)); err != nil {
		return 0, err
	}
	log := logf.FromContext(ctx).WithName("session-rebuild")
	rebuilt := 0
	for i := range list.Items {
		s := &list.Items[i]
		phase := s.Status.Phase
		if phase == "" || phase == resumeapi.StateAbsent {
			continue // nothing durable to restore
		}
		// Present in KV already? leave it (KV wins in normal operation).
		if _, err := r.kv.GetSession(ctx, s.Name); err == nil {
			continue
		}
		e := statusToEntry(s.Name, &s.Status)
		if err := r.kv.PutSessionCAS(ctx, e); err != nil {
			log.Error(err, "rebuild: put session", "sid", s.Name)
			continue
		}
		rebuilt++
	}
	if rebuilt > 0 {
		log.Info("rebuilt session cache from Session CRs", "count", rebuilt)
	}
	return rebuilt, nil
}

// Start runs RebuildOnce once at startup (manager Runnable). A wiped Valkey is
// repopulated before the operator serves resumes in earnest; the write path
// (KV-authoritative) then takes over.
func (r *SessionRebuilder) Start(ctx context.Context) error {
	// Small settle delay so Valkey is reachable and the informer cache is warm.
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(2 * time.Second):
	}
	if _, err := r.RebuildOnce(ctx); err != nil {
		logf.FromContext(ctx).Error(err, "session cache rebuild failed (will rely on write-path/next reconcile)")
	}
	return nil
}
