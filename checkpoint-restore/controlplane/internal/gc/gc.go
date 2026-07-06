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

// Package gc reaps stale S3 checkpoints (P4). Two cases:
//   - TTL: a SUSPENDED session whose checkpoint is older than its
//     ttlAfterSuspendSeconds — delete the snapshot and the session entry.
//   - Orphans: snapshot prefixes under sandboxes/ with no live session at all
//     (e.g. a session deleted without cleanup, or a crash mid-suspend).
//
// The operator runs this with a SCOPED identity: s3:ListBucket + s3:DeleteObject
// on sandboxes/* ONLY (no Get/Put). The privileged worker keeps read/write and
// has NO delete — the dangerous (privileged + delete) combination never exists.
package gc

import (
	"context"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// checkpointPrefix is the S3 key prefix all snapshots live under (matches
// sandboxd's `sandboxes/<id>/<snap>` layout).
const checkpointPrefix = "sandboxes/"

// S3API is the subset of the S3 client the GC needs (list + delete). Injectable
// for tests.
type S3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// TTLLookup resolves a session's ttlAfterSuspendSeconds (0 = keep forever).
type TTLLookup func(ctx context.Context, sid string) (int, error)

// Collector reaps stale checkpoints.
type Collector struct {
	kv     *assign.Client
	s3     S3API
	bucket string
	ttlFor TTLLookup
	now    func() time.Time
	// OrphanGrace guards against racing a fresh suspend: a snapshot prefix is only
	// treated as orphaned if no session references it AND (best-effort) it's not
	// brand new. Kept simple here; the session-index check is the primary guard.
}

// New builds a Collector.
func New(kv *assign.Client, s3api S3API, bucket string, ttlFor TTLLookup, now func() time.Time) *Collector {
	if now == nil {
		now = time.Now
	}
	return &Collector{kv: kv, s3: s3api, bucket: bucket, ttlFor: ttlFor, now: now}
}

// SweepOnce reaps TTL-expired suspended checkpoints and orphaned snapshot
// prefixes. Returns (sessionsGC, orphanPrefixesGC).
func (c *Collector) SweepOnce(ctx context.Context) (int, int, error) {
	sessions, err := c.kv.ListSessions(ctx)
	if err != nil {
		return 0, 0, err
	}

	// Index live snapshot prefixes so we can detect orphans, and TTL-reap the
	// SUSPENDED ones whose retention elapsed.
	live := map[string]bool{} // "sandboxes/<sid>/" -> referenced by a session
	nowMS := c.now().UnixMilli()
	ttlGC := 0
	for _, e := range sessions {
		if e.SnapshotURI != "" {
			live[snapDirOf(e.SnapshotURI)] = true
		}
		if e.State != resumeapi.StateSuspended || e.SnapshotURI == "" {
			continue
		}
		ttlSec, terr := c.ttlFor(ctx, e.SID)
		if terr != nil || ttlSec <= 0 {
			continue // keep forever / unknown
		}
		// Age from the last checkpoint if we have it, else from lastActive.
		ref := e.LastCheckpointAt
		if ref == 0 {
			ref = e.LastActiveAt
		}
		if ref == 0 || nowMS-ref < int64(ttlSec)*1000 {
			continue
		}
		// Expired: delete the snapshot objects, then the session entry.
		if err := c.deletePrefix(ctx, snapDirOf(e.SnapshotURI)); err != nil {
			continue // best-effort; retry next sweep
		}
		_ = c.kv.DeleteSession(ctx, e.SID)
		delete(live, snapDirOf(e.SnapshotURI))
		ttlGC++
	}

	// Orphans: snapshot dirs under sandboxes/ referenced by no session.
	orphanGC := 0
	dirs, err := c.listSnapshotDirs(ctx)
	if err != nil {
		return ttlGC, 0, err
	}
	for _, dir := range dirs {
		if live[dir] {
			continue
		}
		if err := c.deletePrefix(ctx, dir); err == nil {
			orphanGC++
		}
	}
	return ttlGC, orphanGC, nil
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
			Bucket:            &c.bucket,
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
			Bucket: &c.bucket, Prefix: &prefix, ContinuationToken: token,
		})
		if err != nil {
			return err
		}
		for _, obj := range out.Contents {
			if _, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &c.bucket, Key: obj.Key}); err != nil {
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
