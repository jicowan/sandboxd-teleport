//go:build linux

package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// onDiskBytes returns the actual allocated size of a file (st_blocks*512), so a test
// can assert the decoded file stayed SPARSE (holes not materialized).
func onDiskBytes(t *testing.T, path string) int64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Blocks * 512
}

// makeSparse writes a file of logical size `size` with data only at the given
// (offset,len) extents (the rest are holes), and returns its expected byte content.
func makeSparse(t *testing.T, path string, size int64, extents [][2]int64) []byte {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, size)
	for i, e := range extents {
		off, ln := e[0], e[1]
		buf := bytes.Repeat([]byte{byte('A' + i)}, int(ln))
		if _, err := f.WriteAt(buf, off); err != nil {
			t.Fatal(err)
		}
		copy(want[off:off+ln], buf)
	}
	return want
}

func TestSparseZstdRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img")

	const size = 8 << 20 // 8MiB logical
	// two data extents separated by a big hole, plus a trailing hole.
	extents := [][2]int64{{0, 4096}, {5 << 20, 1 << 20}}
	want := makeSparse(t, src, size, extents)

	// --- encode ---
	sf, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	var enc bytes.Buffer
	logical, dataBytes, err := writeSparseZstd(&enc, sf)
	sf.Close()
	if err != nil {
		t.Fatalf("writeSparseZstd: %v", err)
	}
	if logical != size {
		t.Errorf("logical = %d, want %d", logical, size)
	}
	if want, got := int64(4096+(1<<20)), dataBytes; got != want {
		t.Errorf("dataBytes = %d, want %d (only the extents, not the holes)", got, want)
	}
	// The compressed+sparse encoding must be far smaller than the logical size (holes
	// dropped; the repeated-byte extents compress hard).
	if int64(enc.Len()) >= size {
		t.Errorf("encoded %d bytes >= logical %d — not smaller", enc.Len(), size)
	}

	// --- magic dispatch ---
	br := bufio.NewReader(&enc)
	peek, _ := br.Peek(sparseMagicLen)
	if !hasSparseMagic(peek) {
		t.Fatalf("hasSparseMagic = false on freshly-encoded stream")
	}
	if _, err := br.Discard(sparseMagicLen); err != nil { // consume the magic
		t.Fatal(err)
	}

	// --- decode ---
	dstPath := filepath.Join(dir, "dst.img")
	df, err := os.Create(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	rl, err := readSparseZstd(df, br)
	df.Close()
	if err != nil {
		t.Fatalf("readSparseZstd: %v", err)
	}
	if rl != size {
		t.Errorf("decoded logical = %d, want %d", rl, size)
	}

	// bytes identical to the original (holes read back as zeros)
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("decoded content != original (len got=%d want=%d)", len(got), len(want))
	}

	// decoded file stayed sparse: on-disk allocation must be far below the logical
	// size (only the ~1MiB+4KiB of extents, rounded to blocks, is allocated).
	if od := onDiskBytes(t, dstPath); od >= size {
		t.Errorf("decoded file not sparse: on-disk %d >= logical %d", od, size)
	}
}

func TestHasSparseMagicRejectsPlain(t *testing.T) {
	if hasSparseMagic([]byte("not-magic-at-all")) {
		t.Error("hasSparseMagic true on non-magic bytes")
	}
	if hasSparseMagic([]byte("short")) {
		t.Error("hasSparseMagic true on too-short input")
	}
	if !hasSparseMagic([]byte(sparseMagic + "trailing")) {
		t.Error("hasSparseMagic false on valid magic prefix")
	}
}
