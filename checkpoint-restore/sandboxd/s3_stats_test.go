//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestSparseSavings(t *testing.T) {
	cases := []struct {
		name string
		st   uploadStats
		want string // substrings that must appear
	}{
		{"microvm-43x", uploadStats{LogicalBytes: 2048 << 20, TransferredBytes: 47900160 /* ~45.7MiB */}, "ratio=44.8x"},
		{"gvisor-1x", uploadStats{LogicalBytes: 163 << 20, TransferredBytes: 161 << 20}, "ratio=1.0x"},
		{"zero-transferred-no-divzero", uploadStats{LogicalBytes: 10, TransferredBytes: 0}, "ratio=0.0x"},
	}
	for _, c := range cases {
		got := sparseSavings(c.st)
		if !strings.Contains(got, "logical=") || !strings.Contains(got, "transferred=") {
			t.Errorf("%s: %q missing logical/transferred fields", c.name, got)
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q does not contain %q", c.name, got, c.want)
		}
	}
}

// TestCountingWriter verifies the byte tally used to report transferred (compressed)
// size matches what actually passed through.
func TestCountingWriter(t *testing.T) {
	var sink strings.Builder
	cw := &countingWriter{w: &sink}
	payloads := []string{"", "abc", strings.Repeat("x", 4096)}
	var want int64
	for _, p := range payloads {
		n, err := cw.Write([]byte(p))
		if err != nil {
			t.Fatal(err)
		}
		if n != len(p) {
			t.Errorf("Write returned %d, want %d", n, len(p))
		}
		want += int64(len(p))
	}
	if cw.n != want {
		t.Errorf("countingWriter.n = %d, want %d", cw.n, want)
	}
	if int64(sink.Len()) != want {
		t.Errorf("underlying writer got %d bytes, want %d", sink.Len(), want)
	}
}
