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

// fakeReaper records Reap calls and simulates operator-owned CR deletion.
type fakeReaper struct {
	owned   map[string]bool // sid -> operator-owned (deletable)
	dryRun  bool
	reaped  []string // sids actually deleted
	touched []string // all sids Reap was called for
}

func (r *fakeReaper) Reap(_ context.Context, sid string) (bool, error) {
	r.touched = append(r.touched, sid)
	if !r.owned[sid] {
		return false, nil // user-owned: tombstone, not deleted
	}
	if r.dryRun {
		return true, nil // would delete
	}
	r.reaped = append(r.reaped, sid)
	return true, nil
}

// fakeCRs is a static gc.CRLister.
type fakeCRs struct{ items []CRSession }

func (c *fakeCRs) ListSessions(_ context.Context) ([]CRSession, error) { return c.items, nil }

const now = int64(1_000_000_000_000)

func TestGCReapsTTLExpiredAndOrphans(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)

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
		"sandboxes/a/snap-1/checkpoint.img":      true,
		"sandboxes/a/snap-1/config.json":         true,
		"sandboxes/b/snap-1/checkpoint.img":      true,
		"sandboxes/orphan/snap-1/checkpoint.img": true, // no session -> orphan
	}}
	reaper := &fakeReaper{owned: map[string]bool{"a": true}}
	col := New(kv, s3f, Config{
		Bucket: "bucket",
		TTLFor: func(_ context.Context, _ string) (int, error) { return 30, nil },
		Reaper: reaper,
		Now:    fixedNow(now),
	})

	r, err := col.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.TTL != 1 {
		t.Fatalf("want 1 TTL reap (session a), got %d", r.TTL)
	}
	if r.OrphanS3 != 1 {
		t.Fatalf("want 1 orphan-S3 reap, got %d", r.OrphanS3)
	}
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
	if len(reaper.reaped) != 1 || reaper.reaped[0] != "a" {
		t.Fatalf("want CR 'a' reaped, got %v", reaper.reaped)
	}
}

func TestGCDefaultTTLApplies(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	// No per-session TTL (returns 0), but a default of 30s -> reap the 100s-old one.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "a", State: resumeapi.StateSuspended, SnapshotURI: "sandboxes/a/snap-1",
		LastCheckpointAt: now - 100_000,
	})
	s3f := &fakeS3{objects: map[string]bool{"sandboxes/a/snap-1/checkpoint.img": true}}
	col := New(kv, s3f, Config{
		Bucket:            "bucket",
		TTLFor:            func(_ context.Context, _ string) (int, error) { return 0, nil },
		DefaultTTLSeconds: 30,
		Now:               fixedNow(now),
	})
	r, err := col.SweepOnce(ctx)
	if err != nil || r.TTL != 1 {
		t.Fatalf("default TTL should reap: TTL=%d err=%v", r.TTL, err)
	}
}

func TestGCKeepsWhenTTLZero(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "a", State: resumeapi.StateSuspended, SnapshotURI: "sandboxes/a/snap-1",
		LastCheckpointAt: now - 1_000_000,
	})
	s3f := &fakeS3{objects: map[string]bool{"sandboxes/a/snap-1/checkpoint.img": true}}
	col := New(kv, s3f, Config{
		Bucket: "bucket",
		TTLFor: func(_ context.Context, _ string) (int, error) { return 0, nil },
		// DefaultTTLSeconds 0 => keep forever
		Now: fixedNow(now),
	})
	r, err := col.SweepOnce(ctx)
	if err != nil || r.TTL != 0 || r.OrphanS3 != 0 {
		t.Fatalf("ttl=0 should keep everything: %+v err=%v", r, err)
	}
	if !s3f.objects["sandboxes/a/snap-1/checkpoint.img"] {
		t.Fatal("checkpoint deleted despite TTL=0")
	}
}

func TestGCAbandonedReap(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	// Zombie-Running: bound to a worker that doesn't exist, idle 2h, grace 1h -> reap.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "zombie", State: resumeapi.StateRunning, WorkerPod: "dead-pod",
		LastActiveAt: now - 2*3600*1000,
	})
	// Live-Running: worker exists and holds it -> keep.
	kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{Pod: "live-pod", Pool: "p", State: resumeapi.WorkerBusy, SID: "live"})
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "live", State: resumeapi.StateRunning, WorkerPod: "live-pod",
		LastActiveAt: now - 2*3600*1000,
	})
	// Recently-active zombie within grace -> keep.
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "fresh", State: resumeapi.StateRunning, WorkerPod: "gone",
		LastActiveAt: now - 60*1000, // 1m ago
	})

	reaper := &fakeReaper{owned: map[string]bool{"zombie": true}}
	col := New(kv, &fakeS3{objects: map[string]bool{}}, Config{
		Bucket:                "bucket",
		AbandonedGraceSeconds: 3600,
		Reaper:                reaper,
		Now:                   fixedNow(now),
	})
	r, err := col.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Abandoned != 1 {
		t.Fatalf("want 1 abandoned reap, got %d", r.Abandoned)
	}
	if _, err := kv.GetSession(ctx, "zombie"); err != assign.ErrNotFound {
		t.Fatal("zombie KV entry not deleted")
	}
	if _, err := kv.GetSession(ctx, "live"); err != nil {
		t.Fatal("live session wrongly reaped")
	}
	if _, err := kv.GetSession(ctx, "fresh"); err != nil {
		t.Fatal("within-grace session wrongly reaped")
	}
	if len(reaper.reaped) != 1 || reaper.reaped[0] != "zombie" {
		t.Fatalf("want CR 'zombie' reaped, got %v", reaper.reaped)
	}
}

func TestGCAbandonedOffWhenGraceZero(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "zombie", State: resumeapi.StateRunning, WorkerPod: "dead",
		LastActiveAt: now - 10*3600*1000,
	})
	col := New(kv, &fakeS3{objects: map[string]bool{}}, Config{
		Bucket: "bucket", AbandonedGraceSeconds: 0, // off
		Reaper: &fakeReaper{owned: map[string]bool{"zombie": true}},
		Now:    fixedNow(now),
	})
	r, _ := col.SweepOnce(ctx)
	if r.Abandoned != 0 {
		t.Fatalf("grace=0 should disable abandoned pass, got %d", r.Abandoned)
	}
	if _, err := kv.GetSession(ctx, "zombie"); err != nil {
		t.Fatal("zombie reaped despite grace=0")
	}
}

func TestGCOrphanCRReap(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t) // empty: no KV entries
	reaper := &fakeReaper{owned: map[string]bool{"orphan-op": true}}
	crs := &fakeCRs{items: []CRSession{
		// operator-owned, empty phase, no KV, old -> reap
		{SID: "orphan-op", Phase: "", OperatorOwned: true, CreatedUnixMilli: now - 10*3600*1000},
		// user-owned -> never delete
		{SID: "orphan-user", Phase: "", OperatorOwned: false, CreatedUnixMilli: now - 10*3600*1000},
		// operator-owned but freshly created (within grace) -> keep
		{SID: "fresh-op", Phase: "", OperatorOwned: true, CreatedUnixMilli: now - 60*1000},
		// operator-owned but has a durable phase -> KV passes own it, skip
		{SID: "suspended-op", Phase: resumeapi.StateSuspended, OperatorOwned: true, CreatedUnixMilli: now - 10*3600*1000},
	}}
	col := New(kv, &fakeS3{objects: map[string]bool{}}, Config{
		Bucket: "bucket", AbandonedGraceSeconds: 3600,
		Reaper: reaper, CRs: crs, Now: fixedNow(now),
	})
	r, err := col.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.OrphanCR != 1 {
		t.Fatalf("want 1 orphan-CR reap, got %d", r.OrphanCR)
	}
	if len(reaper.reaped) != 1 || reaper.reaped[0] != "orphan-op" {
		t.Fatalf("want only 'orphan-op' reaped, got %v", reaper.reaped)
	}
}

func TestGCDryRunMutatesNothing(t *testing.T) {
	ctx := context.Background()
	kv := testKV(t)
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "a", State: resumeapi.StateSuspended, SnapshotURI: "sandboxes/a/snap-1",
		LastCheckpointAt: now - 100_000,
	})
	kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
		SID: "zombie", State: resumeapi.StateRunning, WorkerPod: "dead",
		LastActiveAt: now - 2*3600*1000,
	})
	s3f := &fakeS3{objects: map[string]bool{
		"sandboxes/a/snap-1/checkpoint.img":      true,
		"sandboxes/orphan/snap-1/checkpoint.img": true,
	}}
	reaper := &fakeReaper{owned: map[string]bool{"a": true, "zombie": true}, dryRun: true}
	col := New(kv, s3f, Config{
		Bucket:                "bucket",
		TTLFor:                func(_ context.Context, _ string) (int, error) { return 30, nil },
		AbandonedGraceSeconds: 3600,
		DryRun:                true,
		Reaper:                reaper,
		Now:                   fixedNow(now),
	})
	r, err := col.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Classified as candidates...
	if r.TTL != 1 || r.Abandoned != 1 || r.OrphanS3 != 1 {
		t.Fatalf("dry-run should still classify: %+v", r)
	}
	if !r.DryRun {
		t.Fatal("result should be marked DryRun")
	}
	// ...but nothing mutated.
	if !s3f.objects["sandboxes/a/snap-1/checkpoint.img"] {
		t.Fatal("dry-run deleted S3 object")
	}
	if len(s3f.deleted) != 0 {
		t.Fatalf("dry-run issued S3 deletes: %v", s3f.deleted)
	}
	if _, err := kv.GetSession(ctx, "a"); err != nil {
		t.Fatal("dry-run deleted KV entry a")
	}
	if _, err := kv.GetSession(ctx, "zombie"); err != nil {
		t.Fatal("dry-run deleted KV entry zombie")
	}
	if len(reaper.reaped) != 0 {
		t.Fatalf("dry-run deleted CRs: %v", reaper.reaped)
	}
}
