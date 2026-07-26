package main

import (
	"bufio"
	"bytes"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Worker-side memory reserve — OOM-isolate the sandboxd agent.
//
// A worker pod runs the sandboxd Go agent + runsc (sentry/gofer) ALONGSIDE the
// sandbox, all under one pod cgroup. In gVisor the guest's RAM lives inside the
// sentry process, so a runaway guest grows until the POD cgroup hits its limit —
// at which point the kernel OOM killer may pick the sandboxd AGENT as the victim,
// taking down the whole worker. We cap the sandbox's own memory cgroup a bit below
// the pod limit so the SANDBOX's cgroup OOM-kills first, sparing the agent.
//
// The limit is derived entirely worker-side from the worker's OWN pod cgroup —
// no CRD/API/KV/wire surface. See docs/sandboxd/PRD/PRD-worker-memory-reserve.md.

const (
	defaultReserveBytes int64 = 256 << 20 // 256Mi floor
	defaultReservePct   int64 = 12         // % of pod limit
	// sandboxMemFloor: if the computed sandbox limit would be below this, skip
	// (emit no limit) rather than strangle the workload — a pod too small for a
	// meaningful reserve just runs uncapped, as today.
	sandboxMemFloor int64 = 128 << 20 // 128Mi
)

// memReserveConfig holds the (env-tunable) reserve parameters, read once at startup.
type memReserveConfig struct {
	floorBytes int64 // SANDBOXD_AGENT_MEMORY_RESERVE (bytes), default 256Mi
	pct        int64 // SANDBOXD_AGENT_MEMORY_RESERVE_PCT, default 12
}

func loadMemReserveConfig() memReserveConfig {
	c := memReserveConfig{floorBytes: defaultReserveBytes, pct: defaultReservePct}
	if v := os.Getenv("SANDBOXD_AGENT_MEMORY_RESERVE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			c.floorBytes = n
		}
	}
	if v := os.Getenv("SANDBOXD_AGENT_MEMORY_RESERVE_PCT"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 && n < 100 {
			c.pct = n
		}
	}
	return c
}

// disabled reports whether the operator explicitly turned the feature off
// (floor == 0 AND pct == 0 => no reserve => never cap).
func (c memReserveConfig) disabled() bool { return c.floorBytes == 0 && c.pct == 0 }

// podMemoryLimit returns the worker POD's own memory limit in bytes, or
// (0, false) when the pod has no limit set.
//
// The value comes from the downward API env var SANDBOXD_POD_MEM_LIMIT, which the
// operator injects (via resourceFieldRef: limits.memory) ONLY when the
// SandboxTemplate sets an explicit memory limit — so env-present means a real pod
// limit and env-absent means uncapped. We do NOT read /sys/fs/cgroup/memory.max:
// the worker is a PRIVILEGED pod with no cgroup namespace, so its /sys/fs/cgroup is
// not its own leaf cgroup and memory.max there reads "max" even when a pod limit is
// set (verified live). And we must NOT let the downward API fall back to node
// allocatable when no limit is set (it does) — hence the operator gates injection
// on a limit existing. This limit comes from SandboxTemplate.spec.resources and is
// pool-constant regardless of Karpenter instance type or pod density (PRD §5.1).
func podMemoryLimit() (int64, bool) {
	return parseMemLimitEnv(os.Getenv("SANDBOXD_POD_MEM_LIMIT"))
}

func parseMemLimitEnv(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false // no limit injected → uncapped
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// sandboxMemLimit computes the per-sandbox memory limit from the pod limit and the
// reserve config. Returns 0 when no limit should be set (unlimited pod, feature
// disabled, or a pod too small to carve a meaningful reserve). Pure + table-tested.
func sandboxMemLimit(podLimit int64, hasLimit bool, c memReserveConfig) int64 {
	if !hasLimit || podLimit <= 0 || c.disabled() {
		return 0
	}
	reserve := c.floorBytes
	if pctReserve := podLimit * c.pct / 100; pctReserve > reserve {
		reserve = pctReserve
	}
	limit := podLimit - reserve
	if limit < sandboxMemFloor {
		return 0 // reserve would consume most of the pod → leave uncapped
	}
	return limit
}

// computeSandboxMemLimit is the runtime entry point: read the pod cgroup, compute
// the sandbox limit, and log the decision once per bundle build. Returns the limit
// in bytes (0 = omit).
func computeSandboxMemLimit(c memReserveConfig) int64 {
	if c.disabled() {
		return 0
	}
	podLimit, hasLimit := podMemoryLimit()
	if !hasLimit {
		// The dangerous config under Karpenter: a limit-less pod can burst into
		// shared node memory that density makes unpredictable, with no per-sandbox
		// cap. We do NOT guess against node memory (density-dependent, unsafe);
		// recommend a limit so the anchor exists.
		log.Printf("WARN: worker pod has no memory limit (SANDBOXD_POD_MEM_LIMIT unset); agent OOM-protection is OFF. Set a memory limit on SandboxTemplate.spec.resources to enable it (see PRD-worker-memory-reserve.md).")
		return 0
	}
	limit := sandboxMemLimit(podLimit, hasLimit, c)
	if limit == 0 {
		log.Printf("WARN: pod memory limit %d too small for a meaningful agent reserve; sandbox left uncapped", podLimit)
		return 0
	}
	log.Printf("per-sandbox memory limit %d bytes (pod limit %d, reserve %d) — agent OOM-protection ON", limit, podLimit, podLimit-limit)
	return limit
}

// sandboxOOMKills reports how many times the OOM killer has fired inside the
// sandbox's OWN cgroup (cgroup v2 memory.events `oom_kill` counter), so a sandbox
// that died because it blew past its per-sandbox limit is DIAGNOSABLE as an OOM
// rather than a mysterious workload exit. Best-effort and side-effect-free: we do
// NOT set linux.cgroupsPath in the OCI spec (that would ride through checkpoint/
// restore and isn't proven teleport-safe), so we can't know the exact path — we
// scan cgroupRoot for a cgroup directory whose name contains the sandbox id and sum
// its oom_kill counters. Returns (count, true) if any matching cgroup was found.
func sandboxOOMKills(cgroupRoot, id string) (int64, bool) {
	if id == "" {
		return 0, false
	}
	var total int64
	var found bool
	filepath.WalkDir(cgroupRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // best-effort: skip unreadable subtrees
		}
		if !strings.Contains(d.Name(), id) {
			return nil
		}
		if n, ok := readOOMKillCount(filepath.Join(path, "memory.events")); ok {
			total += n
			found = true
		}
		return nil
	})
	return total, found
}

// readOOMKillCount parses the `oom_kill N` line out of a cgroup v2 memory.events
// file. Returns (0,false) if the file/line is absent.
func readOOMKillCount(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[0] == "oom_kill" {
			if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}
