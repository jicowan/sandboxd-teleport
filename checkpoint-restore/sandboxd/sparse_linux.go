//go:build linux

// Sparse-aware, zstd-compressed checkpoint object codec.
//
// A checkpoint's memory image (gVisor pages.img, microVM clh-memory-ranges) is a
// SPARSE file: most of a guest's address space is unallocated/zero, so the on-disk
// footprint is a fraction of the logical size (measured live: a 2GiB microVM guest
// with a ~154MiB resident set). The worker's S3 transfer was hole-blind — it streamed
// the whole dense logical file — so a small working set shipped + stored as the full
// guest RAM, dominating suspend/restore latency and S3 cost (see
// docs/sandboxd/PRD/PRD-sparse-checkpoint-s3-transfer.md).
//
// This codec walks the file's populated extents with SEEK_DATA/SEEK_HOLE (holes are
// never read) and feeds ONLY those extents to a zstd stream, so compression scans the
// resident set, not the logical image. On read it recreates a sparse file (holes
// between extents are never written), truncated to the exact logical size.
//
// zstd at SpeedFastest is chosen over lz4 deliberately: S3 (not CPU) is the transfer
// bottleneck, so compression RATIO drives wall-clock, and zstd-fastest matches/beats
// lz4's ratio at comparable compress speed while both decompress far faster than S3
// delivers; klauspost/compress/zstd is also already a dependency. See the PRD.
//
// PROVENANCE: format + SEEK_DATA/SEEK_HOLE + zstd framing adapted from Agent
// Substrate's cmd/atelet/internal/ategcs/sparsezstd.go (Apache-2.0, Copyright 2026
// Google LLC). Adapted to sandboxd's S3 seam (s3.go) with a sandboxd-specific magic
// and an explicit magic-dispatch reader for backward compatibility with the dense
// (pre-codec) objects already in S3.
//
// Licensed under the Apache License, Version 2.0.

package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

// sparseMagic marks the sparse-extent+zstd object format. It is 8 bytes and
// deliberately NOT a valid zstd frame magic, so a reader can dispatch on the first 8
// bytes: this format vs a plain (dense, uncompressed) object written before the codec
// existed. Version-neutral; the format version follows it.
const sparseMagic = "SBXDSPRS"

// sparseVersion is the on-wire format version, a little-endian uint32 in the clear
// right after sparseMagic. Bump on any incompatible layout change so a reader rejects
// objects it can't parse instead of misreading them.
const sparseVersion uint32 = 1

// sparseEndOffset is the end-of-stream sentinel: an extent-frame offset of -1 marks
// the end of the frames (a real offset is always >= 0). An end marker keeps the format
// streamable — the writer needn't know the extent count up front.
const sparseEndOffset int64 = -1

// sparseMagicLen is the byte length a reader must peek to dispatch on the format.
const sparseMagicLen = len(sparseMagic)

// writeSparseZstd encodes a sparse file src to dst in the sparse-extent+zstd format:
//
//	magic[8] | version:u32 | zstd( totalSize:i64 | (off:i64, len:i64, data[len])* | -1:i64 )
//
// magic+version are in the clear so a reader can dispatch; everything after is a
// single zstd stream of the extent metadata interleaved with only the populated
// extents' data (holes are neither read nor compressed), ending with the sentinel.
// Extents are discovered incrementally (SEEK_DATA/SEEK_HOLE), so the format is
// streamable — no extent count up front. Returns the logical size and the populated
// (pre-compression) byte count. All integers little-endian.
func writeSparseZstd(dst io.Writer, src *os.File) (logical, dataBytes int64, err error) {
	fi, err := src.Stat()
	if err != nil {
		return 0, 0, err
	}
	size := fi.Size()

	bw := bufio.NewWriter(dst)
	if _, err := bw.WriteString(sparseMagic); err != nil {
		return 0, 0, err
	}
	if err := binary.Write(bw, binary.LittleEndian, sparseVersion); err != nil {
		return 0, 0, err
	}
	if err := bw.Flush(); err != nil {
		return 0, 0, err
	}

	zw, err := zstd.NewWriter(dst,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)))
	if err != nil {
		return 0, 0, err
	}
	// fail closes the encoder before returning err (Close flushes/frees state).
	fail := func(e error) (int64, int64, error) {
		zw.Close()
		return 0, 0, e
	}
	if err := binary.Write(zw, binary.LittleEndian, size); err != nil {
		return fail(err)
	}

	fd := int(src.Fd())
	off := int64(0)
	for off < size {
		ds, serr := unix.Seek(fd, off, unix.SEEK_DATA)
		if serr != nil {
			if serr == unix.ENXIO { // no more data: the rest is a hole
				break
			}
			return fail(fmt.Errorf("SEEK_DATA: %w", serr))
		}
		de, serr := unix.Seek(fd, ds, unix.SEEK_HOLE)
		if serr != nil {
			return fail(fmt.Errorf("SEEK_HOLE: %w", serr))
		}
		length := de - ds
		if err := binary.Write(zw, binary.LittleEndian, ds); err != nil {
			return fail(err)
		}
		if err := binary.Write(zw, binary.LittleEndian, length); err != nil {
			return fail(err)
		}
		if _, err := src.Seek(ds, io.SeekStart); err != nil {
			return fail(err)
		}
		n, cerr := io.CopyN(zw, src, length)
		dataBytes += n
		if cerr != nil {
			return fail(fmt.Errorf("reading extent @%d+%d: %w", ds, length, cerr))
		}
		off = de
	}
	if err := binary.Write(zw, binary.LittleEndian, sparseEndOffset); err != nil {
		return fail(err)
	}
	if err := zw.Close(); err != nil {
		return 0, 0, err
	}
	return size, dataBytes, nil
}

// readSparseZstd decodes the sparse-extent+zstd format into dst, which becomes a
// sparse file (holes between extents are never written). src must be positioned just
// AFTER the magic (the caller peeks + dispatches on it). dst is truncated to the
// logical size so trailing holes + the exact size are represented.
func readSparseZstd(dst *os.File, src io.Reader) (logical int64, err error) {
	var ver uint32
	if err := binary.Read(src, binary.LittleEndian, &ver); err != nil {
		return 0, fmt.Errorf("reading sparse format version: %w", err)
	}
	if ver != sparseVersion {
		return 0, fmt.Errorf("unsupported sparse object format version %d (this build supports %d)", ver, sparseVersion)
	}

	zr, err := zstd.NewReader(src, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return 0, err
	}
	defer zr.Close()

	var size int64
	if err := binary.Read(zr, binary.LittleEndian, &size); err != nil {
		return 0, fmt.Errorf("reading totalSize: %w", err)
	}
	if size < 0 {
		return 0, fmt.Errorf("negative totalSize %d", size)
	}
	if err := dst.Truncate(size); err != nil {
		return 0, err
	}

	// Replay the extent frames: (off i64, len i64, data[len]) until the terminator
	// frame whose offset is sparseEndOffset (-1).
	for {
		var off int64
		if err := binary.Read(zr, binary.LittleEndian, &off); err != nil {
			return 0, fmt.Errorf("reading extent offset: %w", err)
		}
		if off == sparseEndOffset {
			break
		}
		var length int64
		if err := binary.Read(zr, binary.LittleEndian, &length); err != nil {
			return 0, fmt.Errorf("reading extent length: %w", err)
		}
		// Validate against the declared size (the stream is untrusted S3 content): an
		// out-of-range extent would seek/write past the file or overflow off+length.
		// size-off is safe because off <= size is checked first.
		if off < 0 || length < 0 || off > size || length > size-off {
			return 0, fmt.Errorf("sparse extent out of range (off=%d len=%d size=%d)", off, length, size)
		}
		if _, err := dst.Seek(off, io.SeekStart); err != nil {
			return 0, err
		}
		if _, err := io.CopyN(dst, zr, length); err != nil {
			return 0, fmt.Errorf("writing extent @%d+%d: %w", off, length, err)
		}
	}
	return size, nil
}

// hasSparseMagic reports whether the first sparseMagicLen bytes of peek are the
// sparse-object magic. Callers peek that many bytes (via bufio) and dispatch:
// true -> readSparseZstd (after consuming the magic); false -> the object predates
// the codec (dense/uncompressed) and is copied through verbatim.
func hasSparseMagic(peek []byte) bool {
	return len(peek) >= sparseMagicLen && string(peek[:sparseMagicLen]) == sparseMagic
}
