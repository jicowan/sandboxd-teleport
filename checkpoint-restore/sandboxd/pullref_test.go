//go:build linux

package main

import "testing"

// TestPullRef covers digest-pinned ref construction for restore (#8): a valid
// sha256 digest yields <repo>@<digest> with any tag stripped (without corrupting a
// registry-host port); anything else falls back to the image unchanged.
func TestPullRef(t *testing.T) {
	const dig = "sha256:abc123"
	cases := []struct {
		name   string
		image  string
		digest string
		want   string
	}{
		{"empty digest → tag pull (fallback)", "docker.io/library/redis:7-alpine", "", "docker.io/library/redis:7-alpine"},
		{"non-sha256 digest → fallback", "redis:7", "deadbeef", "redis:7"},
		{"tag stripped, digest appended", "docker.io/library/redis:7-alpine", dig, "docker.io/library/redis@" + dig},
		{"no tag → digest appended", "docker.io/library/redis", dig, "docker.io/library/redis@" + dig},
		{"short ref with tag", "redis:7", dig, "redis@" + dig},
		{"short ref no tag", "redis", dig, "redis@" + dig},
		{"registry host WITH PORT + tag (port not mistaken for tag)", "localhost:5000/redis:7", dig, "localhost:5000/redis@" + dig},
		{"registry host WITH PORT no tag", "localhost:5000/redis", dig, "localhost:5000/redis@" + dig},
		{"already digest-pinned → unchanged", "redis@sha256:old", dig, "redis@sha256:old"},
		{"empty image → unchanged", "", dig, ""},
		{"ghcr with tag", "ghcr.io/agent-infra/sandbox:latest", dig, "ghcr.io/agent-infra/sandbox@" + dig},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pullRef(c.image, c.digest); got != c.want {
				t.Fatalf("pullRef(%q, %q) = %q, want %q", c.image, c.digest, got, c.want)
			}
		})
	}
}
