//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kata

import (
	"slices"
	"testing"
)

func TestVirtiofsdArgs(t *testing.T) {
	tests := []struct {
		name      string
		opts      VirtiofsdOptions
		wantCache string
	}{
		{
			name:      "RO lower defaults to cache=always",
			opts:      VirtiofsdOptions{SocketPath: "/run/vm/virtiofsd.sock", SharedDir: "/run/shared"},
			wantCache: "--cache=always",
		},
		{
			name: "writable durable share overrides the cache mode",
			opts: VirtiofsdOptions{
				SocketPath: "/run/vm/virtiofsd-durable.sock",
				SharedDir:  "/var/lib/ateom-gvisor/actors/uid/durable-dir",
				Cache:      "auto",
			},
			wantCache: "--cache=auto",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := virtiofsdArgs(tc.opts)
			if !slices.Contains(args, tc.wantCache) {
				t.Errorf("args %v do not contain %q", args, tc.wantCache)
			}
			for _, want := range []string{
				"--socket-path=" + tc.opts.SocketPath,
				"--shared-dir=" + tc.opts.SharedDir,
				// find-paths re-opens the guest's open files by path on
				// restore; both shares depend on it.
				"--migration-mode",
				"find-paths",
			} {
				if !slices.Contains(args, want) {
					t.Errorf("args %v do not contain %q", args, want)
				}
			}
		})
	}
}

// TestUpperWorkDirs pins the host overlay upper/work layout: sibling <cid>/fs and
// <cid>/work under the actor's upper base. This layout is load-bearing twice over —
// the kernel requires upperdir and workdir on the same fs with workdir NOT nested
// under upperdir, and it's also the snapshot tar's entry layout — so a change here
// breaks both every overlay mount and every existing snapshot.
func TestUpperWorkDirs(t *testing.T) {
	upper, work := UpperWorkDirs("/work/rootfs-upper/sbx1", "ctr9")
	if want := "/work/rootfs-upper/sbx1/ctr9/fs"; upper != want {
		t.Errorf("upper = %q, want %q", upper, want)
	}
	if want := "/work/rootfs-upper/sbx1/ctr9/work"; work != want {
		t.Errorf("work = %q, want %q", work, want)
	}
	// workdir must be a SIBLING of upperdir, not nested inside it (the kernel rejects
	// a workdir under upperdir).
	if filepathDir(upper) != filepathDir(work) {
		t.Errorf("upper and work must share a parent: %q vs %q", upper, work)
	}
	if hasPrefixPath(work, upper) {
		t.Errorf("workdir %q must not be nested under upperdir %q", work, upper)
	}
}

func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return p
}
func hasPrefixPath(p, prefix string) bool {
	return len(p) > len(prefix) && p[:len(prefix)] == prefix && p[len(prefix)] == '/'
}
