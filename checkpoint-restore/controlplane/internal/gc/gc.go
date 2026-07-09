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

// Package gc reaps a dead session's WHOLE footprint — S3 checkpoint, Valkey
// assignment entry (+ due-indexes), and the Session CR — across every way a
// session goes dead (PRD-session-garbage-collection). Four passes:
//
//   - TTL:          a SUSPENDED session whose checkpoint is older than its
//     ttlAfterSuspendSeconds — delete the snapshot, the KV entry,
//     and (if operator-owned) the Session CR.
//   - Abandoned:    a non-suspended KV entry whose bound worker is gone / no
//     longer holds it, idle past a grace — delete the KV entry and
//     the CR (classes B/C: zombie-Running pointing at a dead worker).
//   - Orphan-S3:    snapshot prefixes under sandboxes/ referenced by no session.
//   - Orphan-CR:    an operator-owned Session CR with no KV entry, Absent/empty
//     phase, older than a grace — delete the CR (class D).
//
// The operator runs this with a SCOPED S3 identity: s3:ListBucket + s3:DeleteObject
// on sandboxes/* ONLY (no Get/Put). The privileged worker keeps read/write and has
// NO delete — the dangerous (privileged + delete) combination never exists. CR
// deletes use the operator's own RBAC (which already grants delete on sessions).
//
// DryRun (default when armed) performs no mutation — it only classifies and
// records what it WOULD reap (metrics + logs), so the classification can be
// validated against a live fleet before arming.
package gc

import (
	"context"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/controlplane/internal/metrics"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// checkpointPrefix is the S3 key prefix all snapshots live under (matches
// sandboxd's `sandboxes/<id>/<snap>` layout).
const checkpointPrefix = "sandboxes/"

// Reap-class labels (also used as the metrics `class` label value).
const (
	classTTL       = "ttl"
	classAbandoned = "abandoned"
	classOrphanS3  = "orphan-s3"
	classOrphanCR  = "orphan-cr"
)

// S3API is the subset of the S3 client the GC needs (list + delete). Injectable
// for tests.
type S3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// TTLLookup resolves a session's ttlAfterSuspendSeconds (0 = fall back to the
// collector's default; the default 0 = keep forever).
type TTLLookup func(ctx context.Context, sid string) (int, error)

// SessionReaper deletes (operator-owned) or tombstones (user-owned) a Session CR.
// Satisfied by controller.SessionReaper. Returns whether a CR was deleted.
type SessionReaper interface {
	Reap(ctx context.Context, sid string) (bool, error)
}

// CRSession is the minimal view of a Session CR the orphan-CR pass needs.
type CRSession struct {
	SID              string
	Phase            string
	OperatorOwned    bool
	CreatedUnixMilli int64
}

// CRLister enumerates Session CRs so the orphan-CR pass can find operator-owned
// CRs with no live KV entry. Satisfied by a controller-layer adapter over the
// cached client.
type CRLister interface {
	ListSessions(ctx context.Context) ([]CRSession, error)
}

// Config bundles the collector's dependencies + policy knobs.
type Config struct {
	Bucket                string
	TTLFor                TTLLookup     // per-session ttlAfterSuspendSeconds
	DefaultTTLSeconds     int           // applied when the per-session TTL is 0; 0 = keep forever
	AbandonedGraceSeconds int           // how long a session must look dead before reaping (0 = off)
	DryRun                bool          // true = classify + record, never mutate
	Reaper                SessionReaper // nil = don't touch Session CRs (S3+KV only)
	CRs                   CRLister      // nil = skip the orphan-CR pass
	Now                   func() time.Time
}

// Collector reaps dead-session footprint.
type Collector struct {
	kv  *assign.Client
	s3  S3API
	cfg Config
}

// New builds a Collector.
func New(kv *assign.Client, s3api S3API, cfg Config) *Collector {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Collector{kv: kv, s3: s3api, cfg: cfg}
}

// SweepResult reports what a sweep reaped (or, in dry-run, would reap) per class.
type SweepResult struct {
	TTL       int // suspended sessions past retention
	Abandoned int // non-suspended sessions whose worker is gone
	OrphanS3  int // snapshot prefixes with no session
	OrphanCR  int // operator-owned CRs with no KV entry
	DryRun    bool
}

// Total is the sum across classes (convenience for logging).
func (r SweepResult) Total() int { return r.TTL + r.Abandoned + r.OrphanS3 + r.OrphanCR }

// SweepOnce runs all four reap passes.
func (c *Collector) SweepOnce(ctx context.Context) (SweepResult, error) {
	res := SweepResult{DryRun: c.cfg.DryRun}
	sessions, err := c.kv.ListSessions(ctx)
	if err != nil {
		return res, err
	}
	nowMS := c.cfg.Now().UnixMilli()

	// accounted tracks snapshot dirs we've either kept (a live session references
	// them) or already handled in the TTL pass, so the orphan-S3 pass doesn't
	// double-count them (matters in dry-run, where the TTL pass doesn't actually
	// delete the objects).
	accounted := map[string]bool{}

	for _, e := range sessions {
		switch {
		case e.State == resumeapi.StateSuspended && e.SnapshotURI != "":
			if c.ttlExpired(ctx, e, nowMS) {
				c.reapSession(ctx, e, classTTL, snapDirOf(e.SnapshotURI))
				accounted[snapDirOf(e.SnapshotURI)] = true
				res.TTL++
				continue
			}
			// kept: reference its snapshot so orphan-S3 leaves it alone.
			accounted[snapDirOf(e.SnapshotURI)] = true
		case e.State != resumeapi.StateSuspended && c.abandoned(ctx, e, nowMS):
			// Zombie: worker gone / no longer holds it, idle past grace. Usually no
			// snapshot; if one exists, don't account it so orphan-S3 reclaims it.
			c.reapSession(ctx, e, classAbandoned, "")
			res.Abandoned++
		default:
			if e.SnapshotURI != "" {
				accounted[snapDirOf(e.SnapshotURI)] = true
			}
		}
	}

	// Orphan-S3: snapshot dirs under sandboxes/ referenced by no kept session.
	dirs, err := c.listSnapshotDirs(ctx)
	if err != nil {
		return res, err
	}
	for _, dir := range dirs {
		if accounted[dir] {
			continue
		}
		if c.cfg.DryRun {
			metrics.GCReapedTotal.WithLabelValues("s3", classOrphanS3).Add(0)
			res.OrphanS3++
			continue
		}
		if err := c.deletePrefix(ctx, dir); err == nil {
			metrics.GCReapedTotal.WithLabelValues("s3", classOrphanS3).Inc()
			res.OrphanS3++
		}
	}

	// Orphan-CR: operator-owned Session CRs with no KV entry (class D).
	res.OrphanCR = c.sweepOrphanCRs(ctx, nowMS)

	c.recordCandidates(res)
	return res, nil
}

// ttlExpired reports whether a suspended session's checkpoint is older than its
// effective retention (per-session TTL, else the collector default; 0 = keep).
func (c *Collector) ttlExpired(ctx context.Context, e *resumeapi.SessionEntry, nowMS int64) bool {
	ttlSec := 0
	if c.cfg.TTLFor != nil {
		if v, err := c.cfg.TTLFor(ctx, e.SID); err == nil {
			ttlSec = v
		}
	}
	if ttlSec <= 0 {
		ttlSec = c.cfg.DefaultTTLSeconds
	}
	if ttlSec <= 0 {
		return false // keep forever
	}
	ref := e.LastCheckpointAt
	if ref == 0 {
		ref = e.LastActiveAt
	}
	if ref == 0 {
		return false // no age reference; keep
	}
	return nowMS-ref >= int64(ttlSec)*1000
}

// abandoned reports whether a non-suspended session's worker is gone / no longer
// holds it AND it has been idle past the abandoned grace. Conservative: requires a
// known lastActiveAt (a session with no activity timestamp can't be aged, so it's
// left for a later sweep) and a configured grace.
func (c *Collector) abandoned(ctx context.Context, e *resumeapi.SessionEntry, nowMS int64) bool {
	if c.cfg.AbandonedGraceSeconds <= 0 {
		return false
	}
	if c.workerHolds(ctx, e.WorkerPod, e.SID) {
		return false // a live worker still holds it — not abandoned
	}
	if e.LastActiveAt == 0 {
		return false // unknown age; don't risk reaping a just-created session
	}
	return nowMS-e.LastActiveAt >= int64(c.cfg.AbandonedGraceSeconds)*1000
}

// workerHolds mirrors the router/resume workerHolds() fence: the worker:<pod>
// entry exists, is busy, and is bound to THIS session.
func (c *Collector) workerHolds(ctx context.Context, pod, sid string) bool {
	if pod == "" {
		return false
	}
	w, err := c.kv.GetWorker(ctx, pod)
	if err != nil {
		return false
	}
	return w.State == resumeapi.WorkerBusy && w.SID == sid
}

// reapSession removes a session's KV entry (+ indexes via DeleteSession), its CR
// (via the reaper), and — for the TTL class — its S3 snapshot prefix. Honors
// dry-run (records intent, mutates nothing). snapDir is the S3 prefix to delete
// for TTL reaps ("" for abandoned, which has no snapshot to remove here).
func (c *Collector) reapSession(ctx context.Context, e *resumeapi.SessionEntry, class, snapDir string) {
	if c.cfg.DryRun {
		if snapDir != "" {
			metrics.GCReapedTotal.WithLabelValues("s3", class).Add(0)
		}
		metrics.GCReapedTotal.WithLabelValues("kv", class).Add(0)
		if c.cfg.Reaper != nil {
			_, _ = c.cfg.Reaper.Reap(ctx, e.SID) // reaper is dry-run aware; logs intent
		}
		return
	}
	if snapDir != "" {
		if err := c.deletePrefix(ctx, snapDir); err != nil {
			return // best-effort; retry next sweep (leave KV+CR so we retry the pair)
		}
		metrics.GCReapedTotal.WithLabelValues("s3", class).Inc()
	}
	if err := c.kv.DeleteSession(ctx, e.SID); err == nil {
		metrics.GCReapedTotal.WithLabelValues("kv", class).Inc()
	}
	if c.cfg.Reaper != nil {
		if deleted, err := c.cfg.Reaper.Reap(ctx, e.SID); err == nil && deleted {
			metrics.GCReapedTotal.WithLabelValues("cr", class).Inc()
		}
	}
}

// sweepOrphanCRs deletes operator-owned Session CRs that have no KV entry and a
// dead phase (Absent/empty), older than the abandoned grace. Returns the count.
func (c *Collector) sweepOrphanCRs(ctx context.Context, nowMS int64) int {
	if c.cfg.CRs == nil || c.cfg.Reaper == nil || c.cfg.AbandonedGraceSeconds <= 0 {
		return 0
	}
	crs, err := c.cfg.CRs.ListSessions(ctx)
	if err != nil {
		return 0
	}
	graceMS := int64(c.cfg.AbandonedGraceSeconds) * 1000
	n := 0
	for _, cr := range crs {
		if !cr.OperatorOwned {
			continue // never delete user-declared CRs
		}
		if cr.Phase != "" && cr.Phase != resumeapi.StateAbsent {
			continue // durable state present — the KV passes own it
		}
		// A live/in-flight session has a KV entry (planFor creates the CR, resume
		// writes KV). Skip anything still in KV.
		if _, err := c.kv.GetSession(ctx, cr.SID); err == nil {
			continue
		}
		// Grace off the CR's creation time so we never race a just-created CR whose
		// resume hasn't written KV yet.
		if cr.CreatedUnixMilli != 0 && nowMS-cr.CreatedUnixMilli < graceMS {
			continue
		}
		if c.cfg.DryRun {
			metrics.GCReapedTotal.WithLabelValues("cr", classOrphanCR).Add(0)
			_, _ = c.cfg.Reaper.Reap(ctx, cr.SID) // dry-run aware
			n++
			continue
		}
		if deleted, err := c.cfg.Reaper.Reap(ctx, cr.SID); err == nil && deleted {
			metrics.GCReapedTotal.WithLabelValues("cr", classOrphanCR).Inc()
			n++
		}
	}
	return n
}

// recordCandidates publishes the per-class counts as gauges so a dry-run deploy
// still shows what the armed GC would remove.
func (c *Collector) recordCandidates(r SweepResult) {
	metrics.GCCandidates.WithLabelValues(classTTL).Set(float64(r.TTL))
	metrics.GCCandidates.WithLabelValues(classAbandoned).Set(float64(r.Abandoned))
	metrics.GCCandidates.WithLabelValues(classOrphanS3).Set(float64(r.OrphanS3))
	metrics.GCCandidates.WithLabelValues(classOrphanCR).Set(float64(r.OrphanCR))
}

// snapDirOf returns the "sandboxes/<sid>/" directory for a snapshot URI like
// "sandboxes/<sid>/snap-123" (or a trailing-slash form).
func snapDirOf(uri string) string {
	u := strings.TrimSuffix(uri, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[:i+1]
	}
	return u + "/"
}

// listSnapshotDirs lists the immediate <sid>/ dirs under sandboxes/ via a
// delimited list (CommonPrefixes) — cheap, one page per ~1000 sessions.
func (c *Collector) listSnapshotDirs(ctx context.Context) ([]string, error) {
	var dirs []string
	var token *string
	for {
		out, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            &c.cfg.Bucket,
			Prefix:            aws.String(checkpointPrefix),
			Delimiter:         aws.String("/"),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, cp := range out.CommonPrefixes {
			if cp.Prefix != nil {
				dirs = append(dirs, *cp.Prefix)
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return dirs, nil
}

// deletePrefix deletes every object under prefix (all snap files for a session).
func (c *Collector) deletePrefix(ctx context.Context, prefix string) error {
	var token *string
	for {
		out, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: &c.cfg.Bucket, Prefix: &prefix, ContinuationToken: token,
		})
		if err != nil {
			return err
		}
		for _, obj := range out.Contents {
			if _, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &c.cfg.Bucket, Key: obj.Key}); err != nil {
				return err
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return nil
}
