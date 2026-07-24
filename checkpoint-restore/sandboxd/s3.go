package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3Store moves checkpoint images between the worker and S3. Auth is the AWS
// default chain -> EKS Pod Identity (the worker's ServiceAccount), no static keys.
type s3Store struct {
	cl     *s3.Client
	bucket string
}

func newS3(ctx context.Context, bucket string) (*s3Store, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &s3Store{cl: s3.NewFromConfig(cfg), bucket: bucket}, nil
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
		_, err = s.cl.PutObject(ctx, &s3.PutObjectInput{
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

// downloadOne fetches a single object into localDir (named by its key's basename).
func (s *s3Store) downloadOne(ctx context.Context, key, localDir string) error {
	out, err := s.cl.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return fmt.Errorf("get %s: %w", key, err)
	}
	defer out.Body.Close()
	name := key[strings.LastIndex(key, "/")+1:]
	f, err := os.Create(filepath.Join(localDir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.ReadFrom(out.Body); err != nil {
		return err
	}
	return nil
}
