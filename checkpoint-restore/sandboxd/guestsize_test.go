//go:build linux

package main

import "testing"

// TestDeriveGuestSize covers the guest vCPU/memory precedence (issue #38):
// explicit CH env / non-default kata config > pod resource LIMITS > built-in default.
func TestDeriveGuestSize(t *testing.T) {
	def := memReserveConfig{floorBytes: defaultReserveBytes, pct: defaultReservePct} // 256Mi, 12%
	const gib = int64(1) << 30

	cases := []struct {
		name string
		// inputs
		cfgMemMiB, cfgVCPUs             int
		envMemMiB, envVCPUs            int64
		podMemLimit, podCPULimit       int64
		reserve                        memReserveConfig
		// wants
		wantMem, wantVCPU int
	}{
		{
			name:      "nothing set → built-in default 2048/1",
			cfgMemMiB: 2048, cfgVCPUs: 1,
			wantMem: 2048, wantVCPU: 1,
		},
		{
			name:      "pod limits size the guest (mem = limit − reserve, vcpu = ceil)",
			cfgMemMiB: 2048, cfgVCPUs: 1,
			podMemLimit: 4 * gib, podCPULimit: 2, reserve: def,
			// 4Gi: 12% = 491.52Mi > 256Mi floor → reserve = pct. limit−reserve = 4096−491 = 3605Mi.
			wantMem: int((4*gib - 4*gib*12/100) >> 20), wantVCPU: 2,
		},
		{
			name:      "small pod uses floor reserve",
			cfgMemMiB: 2048, cfgVCPUs: 1,
			podMemLimit: 1 * gib, podCPULimit: 1, reserve: def,
			// 1Gi: 12% = 122Mi < 256Mi floor → reserve = floor. 1024−256 = 768Mi.
			wantMem: int((1*gib - 256*(int64(1)<<20)) >> 20), wantVCPU: 1,
		},
		{
			name:      "explicit CH env overrides pod limits",
			cfgMemMiB: 2048, cfgVCPUs: 1,
			envMemMiB: 512, envVCPUs: 3,
			podMemLimit: 8 * gib, podCPULimit: 4, reserve: def,
			wantMem: 512, wantVCPU: 3,
		},
		{
			name:      "non-default kata config overrides pod limits",
			cfgMemMiB: 6144, cfgVCPUs: 8, // set explicitly in the kata config
			podMemLimit: 2 * gib, podCPULimit: 1, reserve: def,
			wantMem: 6144, wantVCPU: 8,
		},
		{
			name:      "kata config sets only vcpus; mem still derives from limit",
			cfgMemMiB: 2048, cfgVCPUs: 4, // vcpus non-default, mem is the default
			podMemLimit: 2 * gib, podCPULimit: 1, reserve: def,
			// mem derives from the 2Gi limit (2048−256=1792); vcpus wins from config (4).
			wantMem: int((2*gib - 256*(int64(1)<<20)) >> 20), wantVCPU: 4,
		},
		{
			name:      "tiny pod (reserve eats it) → mem falls back to kata default",
			cfgMemMiB: 2048, cfgVCPUs: 1,
			podMemLimit: 300 * (int64(1) << 20), podCPULimit: 1, reserve: def,
			// sandboxMemLimit returns 0 (below sanity floor) → fall back to cfg default 2048.
			wantMem: 2048, wantVCPU: 1,
		},
		{
			name:      "mem reserve feature disabled → mem falls back to default (no cap to derive)",
			cfgMemMiB: 2048, cfgVCPUs: 1,
			podMemLimit: 8 * gib, podCPULimit: 2, reserve: memReserveConfig{0, 0},
			// disabled reserve → sandboxMemLimit returns 0 → fall back to default; vcpu still derives.
			wantMem: 2048, wantVCPU: 2,
		},
		{
			name:      "cpu limit unset but mem set → vcpu default, mem derived",
			cfgMemMiB: 2048, cfgVCPUs: 1,
			podMemLimit: 4 * gib, reserve: def,
			wantMem: int((4*gib - 4*gib*12/100) >> 20), wantVCPU: 1,
		},
		{
			name:      "env vcpu only; mem from default kata + no limit",
			cfgMemMiB: 2048, cfgVCPUs: 1,
			envVCPUs: 2,
			wantMem: 2048, wantVCPU: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mem, vcpu := deriveGuestSize(c.cfgMemMiB, c.cfgVCPUs, c.envMemMiB, c.envVCPUs, c.podMemLimit, c.podCPULimit, c.reserve)
			if mem != c.wantMem || vcpu != c.wantVCPU {
				t.Fatalf("deriveGuestSize() = (mem=%d, vcpu=%d), want (mem=%d, vcpu=%d)", mem, vcpu, c.wantMem, c.wantVCPU)
			}
		})
	}
}
