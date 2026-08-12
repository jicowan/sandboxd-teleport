package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
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

// sparseSavings renders a compact human summary of the sparse-codec win for a log
// line, e.g. "logical=2048.0MiB transferred=47.9MiB ratio=42.8x". Ratio is the whole
// point of the codec (how much smaller the object is vs a hole-blind dense transfer);
// a ~1x on incompressible/dense data (gVisor) is expected and honest to show.
func sparseSavings(st uploadStats) string {
	ratio := 0.0
	if st.TransferredBytes > 0 {
		ratio = float64(st.LogicalBytes) / float64(st.TransferredBytes)
	}
	const mib = 1 << 20
	return fmt.Sprintf("logical=%.1fMiB transferred=%.1fMiB ratio=%.1fx",
		float64(st.LogicalBytes)/mib, float64(st.TransferredBytes)/mib, ratio)
}

// uploadStats reports what an upload actually moved, so the checkpoint/suspend
// handlers can log + surface the sparse-codec win (see PRD-sparse-checkpoint-s3
// §5.1). LogicalBytes is the sum of the files' logical sizes (what a hole-blind
// transfer would have shipped); TransferredBytes is the compressed object bytes
// actually PUT to S3 (holes dropped + zstd). Ratio = Logical/Transferred.
type uploadStats struct {
	LogicalBytes     int64
	TransferredBytes int64
}

// countingWriter tallies bytes written through it (the compressed object stream), so
// we can report the true transferred size without a second pass or an S3 HEAD.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// uploadDir uploads every file in localDir to s3://bucket/<prefix>/<file>, encoding
// each through the sparse-extent+zstd codec (sparse_linux.go): holes are dropped and
// the resident set is compressed, so a large sparse memory image ships as a fraction
// of its logical size. The codec streams into an io.Pipe whose reader is the upload
// Body, so the manager.Uploader still buffers into 5MiB parts and uploads them
// CONCURRENTLY (multipart) — the compression runs in the pipe-writer goroutine while
// the parts upload in parallel. A dense/incompressible file just yields a ~1:1 object.
// Returns the aggregate logical vs transferred (compressed) byte counts.
func (s *s3Store) uploadDir(ctx context.Context, localDir, prefix string) (uploadStats, error) {
	var st uploadStats
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return st, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		one, err := s.uploadOne(ctx, filepath.Join(localDir, e.Name()),
			strings.TrimSuffix(prefix, "/")+"/"+e.Name())
		if err != nil {
			return st, err
		}
		st.LogicalBytes += one.LogicalBytes
		st.TransferredBytes += one.TransferredBytes
	}
	return st, nil
}

// uploadOne sparse-zstd-encodes path and multipart-uploads it to key. The encode
// runs in a goroutine writing into an io.Pipe; the uploader reads the pipe as the
// object Body (buffering into concurrent parts). A pipe write error is surfaced by
// CloseWithError so the uploader's read fails and Upload returns it. Returns the
// file's logical size and the compressed bytes actually uploaded.
func (s *s3Store) uploadOne(ctx context.Context, path, key string) (uploadStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return uploadStats{}, err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	cw := &countingWriter{w: pw} // count the compressed object bytes as they stream out
	// logicalCh carries the codec's logical size out of the goroutine (the uploader
	// only sees the compressed stream). Buffered so the goroutine never blocks on it.
	logicalCh := make(chan int64, 1)
	go func() {
		logical, _, werr := writeSparseZstd(cw, f)
		logicalCh <- logical
		// Close the write end: nil => clean EOF for the reader; non-nil => the
		// uploader's read returns werr, so Upload fails with the real cause.
		pw.CloseWithError(werr)
	}()

	if _, err := s.up.Upload(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: &key, Body: pr,
	}); err != nil {
		pr.CloseWithError(err) // unblock the encoder goroutine if it's mid-write
		<-logicalCh            // drain so the goroutine can exit
		return uploadStats{}, fmt.Errorf("put %s: %w", key, err)
	}
	return uploadStats{LogicalBytes: <-logicalCh, TransferredBytes: cw.n}, nil
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
// and decodes it back to the on-disk file.
//
// The object is first downloaded to a sibling .dl temp file via the manager
// Downloader — CONCURRENT ranged GETs (the dest is an io.WriterAt written in
// parallel) — so download parallelism is preserved. The compressed object is small
// (holes dropped + zstd), so this ranged fetch is fast. Then we DECODE the temp file:
//   - sparse-zstd magic present  -> readSparseZstd rebuilds the sparse file (holes
//     never written), then the temp is removed.
//   - magic absent (a DENSE object written before this codec)  -> the temp file IS
//     the final content; rename it into place. This is the backward-compat path so
//     snapshots already in S3 still restore.
//
// zstd decode is sequential, so it can't be fed the parallel Downloader directly;
// downloading-then-decoding keeps the ranged-GET concurrency AND streams the (small)
// compressed bytes through the decoder in one local pass.
func (s *s3Store) downloadOne(ctx context.Context, key, localDir string) error {
	name := key[strings.LastIndex(key, "/")+1:]
	final := filepath.Join(localDir, name)
	tmp := final + ".dl"

	tf, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := s.down.Download(ctx, tf, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key}); err != nil {
		tf.Close()
		os.Remove(tmp)
		return fmt.Errorf("get %s: %w", key, err)
	}
	if _, err := tf.Seek(0, io.SeekStart); err != nil {
		tf.Close()
		os.Remove(tmp)
		return err
	}

	// Dispatch on the leading magic.
	br := bufio.NewReader(tf)
	peek, _ := br.Peek(sparseMagicLen)
	if !hasSparseMagic(peek) {
		// Legacy dense object: the temp file already holds the final content.
		tf.Close()
		return os.Rename(tmp, final)
	}
	if _, err := br.Discard(sparseMagicLen); err != nil { // consume the magic
		tf.Close()
		os.Remove(tmp)
		return err
	}

	df, err := os.Create(final)
	if err != nil {
		tf.Close()
		os.Remove(tmp)
		return err
	}
	_, derr := readSparseZstd(df, br)
	df.Close()
	tf.Close()
	os.Remove(tmp)
	if derr != nil {
		os.Remove(final)
		return fmt.Errorf("decode %s: %w", key, derr)
	}
	return nil
}
