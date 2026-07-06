package gc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/redis/go-redis/v9"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

func testKV(t *testing.T) *assign.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	return assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
}

// fakeS3 stores keys in a set; supports delimited list (dirs), prefix list, delete.
type fakeS3 struct {
	objects map[string]bool
	deleted []string
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	out := &s3.ListObjectsV2Output{}
	prefix := aws.ToString(in.Prefix)
	if in.Delimiter != nil { // list immediate sub-dirs
		seen := map[string]bool{}
		for k := range f.objects {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			rest := k[len(prefix):]
			if i := strings.Index(rest, "/"); i >= 0 {
				dir := prefix + rest[:i+1]
				if !seen[dir] {
					seen[dir] = true
					out.CommonPrefixes = append(out.CommonPrefixes, s3types.CommonPrefix{Prefix: aws.String(dir)})
				}
			}
		}
		return out, nil
	}
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			kk := k
			out.Contents = append(out.Contents, s3types.Object{Key: &kk})
		}
	}
	return out, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	k := aws.ToString(in.Key)
	delete(f.objects, k)
	f.deleted = append(f.deleted, k)
	return &s3.DeleteObjectOutput{}, nil
}

func fixedNow(ms int64) func() time.Time { return func() time.Time { return time.UnixMilli(ms) } }

func TestGCReapsTTLExpiredAndOrphans(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	now := int64(1_000_000_000_000)

	// Session A: SUSPENDED, checkpoint 100s old, TTL 30s -> reap.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "a", State: resumeapi.StateSuspended, SnapshotURI: "sandboxes/a/snap-1",
		LastCheckpointAt: now - 100_000,
	})
	// Session B: SUSPENDED but TTL not elapsed -> keep.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "b", State: resumeapi.StateSuspended, SnapshotURI: "sandboxes/b/snap-1",
		LastCheckpointAt: now - 5_000,
	})

	s3f := &fakeS3{objects: map[string]bool{
		"sandboxes/a/snap-1/checkpoint.img": true,
		"sandboxes/a/snap-1/config.json":    true,
		"sandboxes/b/snap-1/checkpoint.img": true,
		"sandboxes/orphan/snap-1/checkpoint.img": true, // no session -> orphan
	}}
	ttl := func(_ context.Context, sid string) (int, error) {
		return 30, nil // 30s for all
	}
	col := New(kv, s3f, "bucket", ttl, fixedNow(now))

	ttlN, orphanN, err := col.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ttlN != 1 {
		t.Fatalf("want 1 TTL reap (session a), got %d", ttlN)
	}
	if orphanN != 1 {
		t.Fatalf("want 1 orphan reap, got %d", orphanN)
	}
	// a's objects + session gone; b kept; orphan gone
	if s3f.objects["sandboxes/a/snap-1/checkpoint.img"] {
		t.Fatal("session a checkpoint not deleted")
	}
	if _, err := kv.GetSession(ctx, "a"); err != assign.ErrNotFound {
		t.Fatal("session a entry not deleted")
	}
	if !s3f.objects["sandboxes/b/snap-1/checkpoint.img"] {
		t.Fatal("session b checkpoint wrongly deleted (TTL not elapsed)")
	}
	if s3f.objects["sandboxes/orphan/snap-1/checkpoint.img"] {
		t.Fatal("orphan not deleted")
	}
}

func TestGCKeepsWhenTTLZero(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	now := int64(1_000_000_000_000)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "a", State: resumeapi.StateSuspended, SnapshotURI: "sandboxes/a/snap-1",
		LastCheckpointAt: now - 1_000_000,
	})
	s3f := &fakeS3{objects: map[string]bool{"sandboxes/a/snap-1/checkpoint.img": true}}
	col := New(kv, s3f, "bucket", func(_ context.Context, _ string) (int, error) { return 0, nil }, fixedNow(now))
	ttlN, orphanN, err := col.SweepOnce(ctx)
	if err != nil || ttlN != 0 || orphanN != 0 {
		t.Fatalf("ttl=0 should keep everything: ttlN=%d orphanN=%d err=%v", ttlN, orphanN, err)
	}
	if !s3f.objects["sandboxes/a/snap-1/checkpoint.img"] {
		t.Fatal("checkpoint deleted despite TTL=0")
	}
}
