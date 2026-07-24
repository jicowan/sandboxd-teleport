package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3Store moves checkpoint images between the worker and S3. Auth is the AWS
// default chain -> EKS Pod Identity (the worker's ServiceAccount), no static keys.
// The manager up/downloader transfer each object in CONCURRENT multipart chunks —
// the checkpoint's memory image (pages.img) is the large payload and dominates
// suspend/restore latency, so intra-object parallelism is the real win (vs. the old
// single-stream PutObject/GetObject per file).
//
// NOTE: feature/s3/manager is marked deprecated in favor of feature/s3/transfermanager,
// but that successor isn't yet a resolvable dependency here (not in the module graph).
// manager is stable and fully functional; migrate to transfermanager when it's vendored.
// (The worker package has no golangci/staticcheck gate — only `go build` — so this
// deprecation is informational, not a CI failure.)
type s3Store struct {
	cl     *s3.Client
	up     *manager.Uploader
	down   *manager.Downloader
	bucket string
}

func newS3(ctx context.Context, bucket string) (*s3Store, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	cl := s3.NewFromConfig(cfg)
	return &s3Store{
		cl:     cl,
		up:     manager.NewUploader(cl),   // default 5MiB parts, 5-way concurrency
		down:   manager.NewDownloader(cl), // default 5MiB parts, 5-way concurrency
		bucket: bucket,
	}, nil
}

// uploadDir uploads every file in localDir to s3://bucket/<prefix>/<file>.
func (s *s3Store) uploadDir(ctx context.Context, localDir, prefix string) error {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(localDir, e.Name()))
		if err != nil {
			return err
		}
		key := strings.TrimSuffix(prefix, "/") + "/" + e.Name()
		_, err = s.up.Upload(ctx, &s3.PutObjectInput{
			Bucket: &s.bucket, Key: &key, Body: f,
		})
		f.Close()
		if err != nil {
			return fmt.Errorf("put %s: %w", key, err)
		}
	}
	return nil
}

// downloadPrefix downloads all objects under s3://bucket/<prefix>/ into localDir.
func (s *s3Store) downloadPrefix(ctx context.Context, prefix, localDir string) error {
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return err
	}
	p := strings.TrimSuffix(prefix, "/") + "/"
	// Paginate: ListObjectsV2 returns at most 1000 keys per page, so a single call
	// silently truncates a large snapshot (partial restore). Walk every page.
	pager := s3.NewListObjectsV2Paginator(s.cl, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &p})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, obj := range page.Contents {
			if err := s.downloadOne(ctx, *obj.Key, localDir); err != nil {
				return err
			}
		}
	}
	return nil
}

// downloadOne fetches a single object into localDir (named by its key's basename)
// via the manager Downloader (concurrent ranged GETs for a large object). The dest
// *os.File is an io.WriterAt, which the Downloader writes chunks into in parallel.
func (s *s3Store) downloadOne(ctx context.Context, key, localDir string) error {
	name := key[strings.LastIndex(key, "/")+1:]
	f, err := os.Create(filepath.Join(localDir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := s.down.Download(ctx, f, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key}); err != nil {
		return fmt.Errorf("get %s: %w", key, err)
	}
	return nil
}
