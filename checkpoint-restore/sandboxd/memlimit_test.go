//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const mi = int64(1) << 20

func TestSandboxMemLimit(t *testing.T) {
	def := memReserveConfig{floorBytes: defaultReserveBytes, pct: defaultReservePct} // 256Mi, 12%
	cases := []struct {
		name     string
		podLimit int64
		hasLimit bool
		cfg      memReserveConfig
		want     int64
	}{
		{"unlimited pod → uncapped", 0, false, def, 0},
		{"feature disabled → uncapped", 8192 * mi, true, memReserveConfig{0, 0}, 0},
		// 8Gi pod: 12% = 983.04Mi > 256Mi floor → reserve is the pct.
		{"large pod uses pct reserve", 8192 * mi, true, def, 8192*mi - 8192*mi*12/100},
		// 1Gi pod: 12% = 122.88Mi < 256Mi floor → reserve is the floor.
		{"small-ish pod uses floor reserve", 1024 * mi, true, def, 1024*mi - 256*mi},
		// 300Mi pod: floor 256Mi → limit 44Mi < 128Mi sanity floor → uncapped.
		{"tiny pod below sanity floor → uncapped", 300 * mi, true, def, 0},
		// Exactly at the boundary: limit == sandboxMemFloor (128Mi) is kept.
		{"limit exactly at sanity floor kept", 128*mi + 256*mi, true, def, 128 * mi},
		{"negative/garbage pod → uncapped", -1, true, def, 0},
		{"custom floor honored", 4096 * mi, true, memReserveConfig{512 * mi, 5}, 4096*mi - 512*mi},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sandboxMemLimit(c.podLimit, c.hasLimit, c.cfg); got != c.want {
				t.Fatalf("sandboxMemLimit(%d,%v,%+v) = %d, want %d", c.podLimit, c.hasLimit, c.cfg, got, c.want)
			}
		})
	}
}

func TestParseMemLimitEnv(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantN   int64
		wantHas bool
	}{
		{"empty → unlimited", "", 0, false},
		{"whitespace → unlimited", "  \n", 0, false},
		{"concrete bytes", "2147483648", 2147483648, true},
		{"bytes with whitespace", " 1073741824\n", 1073741824, true},
		{"garbage → unlimited", "not-a-number", 0, false},
		{"zero → unlimited", "0", 0, false},
		{"negative → unlimited", "-5", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, has := parseMemLimitEnv(c.in)
			if n != c.wantN || has != c.wantHas {
				t.Fatalf("parseMemLimitEnv(%q) = (%d,%v), want (%d,%v)", c.in, n, has, c.wantN, c.wantHas)
			}
		})
	}
}

func TestLoadMemReserveConfig(t *testing.T) {
	t.Setenv("SANDBOXD_AGENT_MEMORY_RESERVE", "")
	t.Setenv("SANDBOXD_AGENT_MEMORY_RESERVE_PCT", "")
	if c := loadMemReserveConfig(); c.floorBytes != defaultReserveBytes || c.pct != defaultReservePct {
		t.Fatalf("defaults not applied: %+v", c)
	}
	t.Setenv("SANDBOXD_AGENT_MEMORY_RESERVE", "536870912") // 512Mi
	t.Setenv("SANDBOXD_AGENT_MEMORY_RESERVE_PCT", "20")
	if c := loadMemReserveConfig(); c.floorBytes != 512*mi || c.pct != 20 {
		t.Fatalf("env not honored: %+v", c)
	}
	// pct out of range (>=100) is ignored → default retained.
	t.Setenv("SANDBOXD_AGENT_MEMORY_RESERVE_PCT", "150")
	if c := loadMemReserveConfig(); c.pct != defaultReservePct {
		t.Fatalf("out-of-range pct not rejected: %+v", c)
	}
	// explicit disable.
	t.Setenv("SANDBOXD_AGENT_MEMORY_RESERVE", "0")
	t.Setenv("SANDBOXD_AGENT_MEMORY_RESERVE_PCT", "0")
	if c := loadMemReserveConfig(); !c.disabled() {
		t.Fatalf("floor=0 pct=0 should disable: %+v", c)
	}
}

func TestReadOOMKillCount(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Realistic cgroup v2 memory.events layout.
	ev := write("events", "low 0\nhigh 0\nmax 3\noom 2\noom_kill 2\n")
	if n, ok := readOOMKillCount(ev); !ok || n != 2 {
		t.Fatalf("oom_kill = (%d,%v), want (2,true)", n, ok)
	}
	none := write("none", "low 0\nhigh 0\n")
	if n, ok := readOOMKillCount(none); ok || n != 0 {
		t.Fatalf("missing oom_kill should be (0,false), got (%d,%v)", n, ok)
	}
	if _, ok := readOOMKillCount(filepath.Join(dir, "nope")); ok {
		t.Fatal("missing file should be (_,false)")
	}
}

func TestSandboxOOMKills(t *testing.T) {
	root := t.TempDir()
	// Simulate a runsc per-container cgroup dir whose name contains the sandbox id.
	id := "sess-demo-abc123"
	cg := filepath.Join(root, "kubepods", "runsc-"+id+".scope")
	if err := os.MkdirAll(cg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cg, "memory.events"), []byte("oom_kill 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, ok := sandboxOOMKills(root, id); !ok || n != 1 {
		t.Fatalf("sandboxOOMKills = (%d,%v), want (1,true)", n, ok)
	}
	// A different id → not found.
	if n, ok := sandboxOOMKills(root, "other-id"); ok || n != 0 {
		t.Fatalf("unrelated id should be (0,false), got (%d,%v)", n, ok)
	}
	// Empty id → never matches.
	if _, ok := sandboxOOMKills(root, ""); ok {
		t.Fatal("empty id must not match")
	}
}

// TestOCISpecMemLimit asserts the OCI spec carries linux.resources.memory.limit
// iff a positive limit is passed, and omits linux.resources entirely otherwise.
func TestOCISpecMemLimit(t *testing.T) {
	get := func(spec map[string]any) (any, bool) {
		lx, _ := spec["linux"].(map[string]any)
		res, ok := lx["resources"].(map[string]any)
		if !ok {
			return nil, false
		}
		mem, _ := res["memory"].(map[string]any)
		v, ok := mem["limit"]
		return v, ok
	}

	// With a limit → present.
	spec := ociSpec([]string{"/bin/sh"}, nil, "/", 0, 0, "", 512*mi)
	if v, ok := get(spec); !ok || v.(int64) != 512*mi {
		t.Fatalf("expected memory.limit=%d, got %v (ok=%v)", 512*mi, v, ok)
	}

	// Zero → linux.resources omitted (byte-for-byte today's behavior).
	spec0 := ociSpec([]string{"/bin/sh"}, nil, "/", 0, 0, "", 0)
	if _, ok := get(spec0); ok {
		t.Fatal("expected no memory.limit when memLimitBytes=0")
	}
	if lx, _ := spec0["linux"].(map[string]any); lx["resources"] != nil {
		t.Fatal("expected linux.resources to be absent when uncapped")
	}

	// Sanity: still valid JSON (runsc reads it as config.json).
	if _, err := json.Marshal(spec); err != nil {
		t.Fatalf("spec not marshalable: %v", err)
	}
}
