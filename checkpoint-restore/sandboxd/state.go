package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

func bgCtx() context.Context { return context.Background() }

// opCtx returns a background context with a generous deadline for long S3
// operations. It is deliberately NOT tied to the HTTP request context: a client
// timeout/disconnect must not cancel an in-flight checkpoint upload or restore
// download (we hit "context canceled" mid-790MB download otherwise).
// opCtx returns a background-rooted context bounded at 15m for a long S3 op
// (checkpoint/restore upload/download), decoupled from the HTTP request so a client
// disconnect can't cancel a multi-hundred-MB transfer mid-flight. The caller MUST
// `defer cancel()` so the timer is released as soon as the op finishes (seconds),
// rather than lingering for the full 15m.
func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Minute)
}

// imgDir is the per-sandbox checkpoint image staging dir.
func (s *server) imgDir(id string) string { return filepath.Join(s.work, "img", id) }

// moveSpecFromImg moves the downloaded config.json out of the image dir into the
// bundle (shared by restore + restart-restore).
func (s *server) moveSpecFromImg(sb *sandbox) bool {
	return moveConfigJSON(s.imgDir(sb.ID), sb.Bundle)
}

// moveConfigJSON moves imgDir/config.json -> bundle/config.json (runsc treats a
// stray config.json in the image dir as an image file, so the saved spec must live
// in the bundle). Returns true if a config.json was present and moved. Shared by the
// /restore handler and the supervisor restart-restore.
func moveConfigJSON(imgDir, bundle string) bool {
	src := filepath.Join(imgDir, "config.json")
	b, err := os.ReadFile(src)
	if err != nil {
		return false
	}
	os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o644)
	os.Remove(src)
	return true
}

// ---- runtime health state (not persisted) ----

func (s *server) healthState(id string) *healthState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hs[id] == nil {
		s.hs[id] = &healthState{}
	}
	// return a copy-ish pointer; callers mutate then putHealthState
	return s.hs[id]
}
func (s *server) putHealthState(id string, h *healthState) {
	s.mu.Lock()
	s.hs[id] = h
	s.mu.Unlock()
}

// Persistence: each sandbox's metadata is written to <work>/meta/<id>.json so
// that a sandboxd restart can reconcile the in-memory map with the runsc state
// (otherwise a pod restart orphans running sandboxes with no metadata).

func (s *server) metaPath(id string) string { return filepath.Join(s.work, "meta", id+".json") }

func (s *server) put(sb *sandbox) {
	s.mu.Lock()
	s.sb[sb.ID] = sb
	s.mu.Unlock()
	// best-effort persist
	os.MkdirAll(filepath.Join(s.work, "meta"), 0o755)
	if b, err := json.MarshalIndent(sb, "", "  "); err == nil {
		os.WriteFile(s.metaPath(sb.ID), b, 0o644)
	}
}

func (s *server) get(id string) *sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sb[id]
}

func (s *server) forget(id string) {
	s.mu.Lock()
	delete(s.sb, id)
	s.mu.Unlock()
	os.Remove(s.metaPath(id))
}

func (s *server) list() []*sandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*sandbox, 0, len(s.sb))
	for _, v := range s.sb {
		out = append(out, v)
	}
	return out
}

// reconcile rebuilds the in-memory map from persisted metadata on startup, and
// drops any metadata whose runsc container no longer exists.
func (s *server) reconcile() {
	dir := filepath.Join(s.work, "meta")
	entries, _ := os.ReadDir(dir)
	loaded, dropped := 0, 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var sb sandbox
		if json.Unmarshal(b, &sb) != nil || sb.ID == "" {
			continue
		}
		// keep metadata even if the container is gone IF it has a snapshot (it can
		// still be restored). Drop only if it has neither a live container nor a snapshot.
		if st, err := s.rt.state(sb.ID); err == nil && st != "" {
			s.mu.Lock()
			s.sb[sb.ID] = &sb
			s.mu.Unlock()
			// Re-register the credential-vending role for a live sandbox that survived
			// a sandboxd process restart (the vendor's in-memory map is empty on boot;
			// the sandbox's baked-in AWS env still points at the vendor).
			if sb.IAMRoleARN != "" && s.cred != nil {
				s.cred.register(sb.ID, sb.IAMRoleARN)
			}
			loaded++
		} else if sb.Snapshot != "" {
			s.mu.Lock()
			s.sb[sb.ID] = &sb
			s.mu.Unlock()
			loaded++
		} else {
			os.Remove(filepath.Join(dir, e.Name()))
			dropped++
		}
	}
	log.Printf("reconcile: loaded %d sandbox(es), dropped %d stale meta", loaded, dropped)
}

// gcLoop periodically removes on-disk artifacts (bundles, image dirs) for
// sandboxes that are no longer tracked or whose container is gone and which have
// been checkpointed (state is safe in S3). Prevents the node-fill we hit.
func (s *server) gcLoop(interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		s.gcOnce()
	}
}

func (s *server) gcOnce() {
	tracked := map[string]bool{}
	for _, sb := range s.list() {
		tracked[sb.ID] = true
	}
	// bundles/ and img/ dirs whose id is untracked AND has no live runsc container
	for _, base := range []string{"bundles", "img"} {
		dir := filepath.Join(s.work, base)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			id := e.Name()
			if tracked[id] {
				continue
			}
			if st, err := s.rt.state(id); err == nil && st != "" {
				continue // still a live container; leave it
			}
			p := filepath.Join(dir, id)
			if err := os.RemoveAll(p); err == nil {
				log.Printf("gc: removed orphaned %s", p)
			}
		}
	}
}

// cleanupBundle removes a sandbox's on-disk bundle + image dir (best-effort),
// used on failed create/restore so we don't leave half-built state around. Also
// unmounts + removes the containerd snapshot backing the rootfs (keyed by id).
func (s *server) cleanupArtifacts(id string) {
	teardownRootfsContainerd(filepath.Join(s.work, "bundles", id, "rootfs"), snapshotKey(id))
	os.RemoveAll(filepath.Join(s.work, "bundles", id))
	os.RemoveAll(filepath.Join(s.work, "img", id))
}

// snapshotKey namespaces our snapshots in the shared containerd store so they
// don't collide with kubelet's or other workers' (all share the k8s.io ns).
func snapshotKey(id string) string { return "sandboxd-" + id }
