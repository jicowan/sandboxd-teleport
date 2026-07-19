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

// Package snapshot implements copy-on-promote and reclaim for fork base snapshots
// (docs/PRD-snapshot-fork.md §5.1/§5.4). A base is an S3 server-side copy of a
// session's checkpoint into a fork-stable bases/<id>/ prefix, distinct from the
// per-session sandboxes/<sid>/ space the GC orphan pass sweeps — so a base's
// lifetime is decoupled from the origin session and structurally exempt from
// orphan reaping.
package snapshot

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// BasePrefix is the S3 key prefix all base snapshots live under. The GC orphan-S3
// pass sweeps sandboxes/ only and never this prefix (docs/PRD-snapshot-fork.md §5.4).
const BasePrefix = "bases/"

// S3API is the subset of the S3 client copy-on-promote + reclaim need. Injectable
// for tests.
type S3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	CopyObject(ctx context.Context, in *s3.CopyObjectInput, opts ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// Store performs base-snapshot S3 operations against one bucket.
type Store struct {
	s3     S3API
	bucket string
}

// New builds a Store.
func New(s3api S3API, bucket string) *Store { return &Store{s3: s3api, bucket: bucket} }

// PromoteResult reports where a base landed.
type PromoteResult struct {
	// SnapshotURI is the fork-stable prefix the base was copied to
	// (bases/<baseID>/<srcSnapName>/), suitable as a Session snapshotURI for restore.
	SnapshotURI string
	// Objects is the number of objects copied.
	Objects int
}

// Promote server-side-copies every object under srcPrefix (a session's snapshot
// dir, e.g. "sandboxes/<sid>/snap-123") into "bases/<baseID>/<srcSnapName>/",
// returning the destination prefix. Copy is O(objects) CopyObject calls (each a
// server-side copy — no data leaves S3). Idempotent: re-copying overwrites the same
// keys.
func (s *Store) Promote(ctx context.Context, baseID, srcPrefix string) (PromoteResult, error) {
	src := strings.TrimSuffix(srcPrefix, "/")
	if src == "" {
		return PromoteResult{}, fmt.Errorf("promote: empty source prefix")
	}
	// Preserve the source snapshot dir name (snap-<ts>) under the base so the
	// restore layout is identical to a normal session snapshot.
	snapName := src[strings.LastIndex(src, "/")+1:]
	dstPrefix := BasePrefix + baseID + "/" + snapName

	keys, err := s.listKeys(ctx, src+"/")
	if err != nil {
		return PromoteResult{}, err
	}
	if len(keys) == 0 {
		return PromoteResult{}, fmt.Errorf("promote: no objects under %q", src)
	}
	n := 0
	for _, key := range keys {
		name := key[strings.LastIndex(key, "/")+1:]
		dstKey := dstPrefix + "/" + name
		// CopySource must be URL-path form "<bucket>/<key>".
		if _, err := s.s3.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket:     &s.bucket,
			Key:        aws.String(dstKey),
			CopySource: aws.String(s.bucket + "/" + key),
		}); err != nil {
			return PromoteResult{}, fmt.Errorf("copy %s -> %s: %w", key, dstKey, err)
		}
		n++
	}
	return PromoteResult{SnapshotURI: dstPrefix, Objects: n}, nil
}

// Reclaim deletes every object under a base's bases/<baseID>/ prefix. Called by the
// base reaper only when the base is unpinned + unreferenced + past grace.
func (s *Store) Reclaim(ctx context.Context, baseID string) error {
	prefix := BasePrefix + baseID + "/"
	keys, err := s.listKeys(ctx, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		k := key
		if _, err := s.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &k}); err != nil {
			return fmt.Errorf("delete %s: %w", k, err)
		}
	}
	return nil
}

// listKeys returns all object keys under prefix (paginated).
func (s *Store) listKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := s.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: &s.bucket, Prefix: &prefix, ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, obj := range out.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}
	return keys, nil
}
