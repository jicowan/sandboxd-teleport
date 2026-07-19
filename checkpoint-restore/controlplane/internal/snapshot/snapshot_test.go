package snapshot

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type fakeS3 struct {
	objects map[string]bool
	copies  int
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	out := &s3.ListObjectsV2Output{}
	prefix := aws.ToString(in.Prefix)
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			kk := k
			out.Contents = append(out.Contents, s3types.Object{Key: &kk})
		}
	}
	return out, nil
}

func (f *fakeS3) CopyObject(_ context.Context, in *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	f.objects[aws.ToString(in.Key)] = true
	f.copies++
	return &s3.CopyObjectOutput{}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	delete(f.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func TestPromoteCopiesToBasesPrefixLeavingSourceIntact(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{objects: map[string]bool{
		"sandboxes/sess-a/snap-1/checkpoint.img": true,
		"sandboxes/sess-a/snap-1/pages.img":      true,
		"sandboxes/sess-a/snap-1/config.json":    true,
	}}
	st := New(f, "bucket")

	res, err := st.Promote(ctx, "golden", "sandboxes/sess-a/snap-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.SnapshotURI != "bases/golden/snap-1" {
		t.Fatalf("dst prefix = %q, want bases/golden/snap-1", res.SnapshotURI)
	}
	if res.Objects != 3 {
		t.Fatalf("copied %d objects, want 3", res.Objects)
	}
	for _, k := range []string{
		"bases/golden/snap-1/checkpoint.img",
		"bases/golden/snap-1/pages.img",
		"bases/golden/snap-1/config.json",
	} {
		if !f.objects[k] {
			t.Fatalf("expected copied object %q", k)
		}
	}
	// Source untouched (copy-on-promote decoupling).
	if !f.objects["sandboxes/sess-a/snap-1/checkpoint.img"] {
		t.Fatal("source object was removed by promote")
	}
}

func TestPromoteEmptySourceErrors(t *testing.T) {
	st := New(&fakeS3{objects: map[string]bool{}}, "bucket")
	if _, err := st.Promote(context.Background(), "g", "sandboxes/none/snap-1"); err == nil {
		t.Fatal("expected error promoting an empty source prefix")
	}
}

func TestReclaimDeletesOnlyBasePrefix(t *testing.T) {
	ctx := context.Background()
	f := &fakeS3{objects: map[string]bool{
		"bases/golden/snap-1/checkpoint.img":     true,
		"bases/golden/snap-1/config.json":        true,
		"sandboxes/sess-a/snap-1/checkpoint.img": true, // must survive
	}}
	st := New(f, "bucket")
	if err := st.Reclaim(ctx, "golden"); err != nil {
		t.Fatal(err)
	}
	if f.objects["bases/golden/snap-1/checkpoint.img"] || f.objects["bases/golden/snap-1/config.json"] {
		t.Fatal("base objects not reclaimed")
	}
	if !f.objects["sandboxes/sess-a/snap-1/checkpoint.img"] {
		t.Fatal("reclaim wrongly deleted a sandboxes/ object")
	}
}
