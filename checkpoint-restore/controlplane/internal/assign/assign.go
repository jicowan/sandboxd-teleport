/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package assign is the KV assignment table (TDD §4): the authoritative store of
// session and worker state, backed by Valkey. The operator is the SOLE writer;
// the router only reads. All state-changing writes are compare-and-set on a
// Version field, executed as atomic Lua so two writers can never diverge — this
// is the split-brain guard (PRD §8 risk 2).
package assign

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// nowMillis is the current unix time in ms as a string arg for Lua scripts.
func nowMillis() string { return strconv.FormatInt(time.Now().UnixMilli(), 10) }

// ErrVersionConflict is returned when a CAS write loses the version race; the
// caller should reload and retry.
var ErrVersionConflict = errors.New("assign: version conflict")

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("assign: not found")

// ErrNoCapacity is returned when a pool has no idle worker to claim.
var ErrNoCapacity = errors.New("assign: no idle worker in pool")

// Key helpers keep the layout in one place (TDD §4.1).
func sessionKey(sid string) string   { return "session:" + sid }
func workerKey(pod string) string    { return "worker:" + pod }
func poolIdleKey(pool string) string { return "pool:" + pool + ":idle" }
func poolAllKey(pool string) string  { return "pool:" + pool + ":all" }

// Secondary indexes (ZSETs) so the sweepers read only sessions that are DUE,
// turning the O(N)-per-15s scans into O(due) lookups (PRD-control-plane-scalability).
//
//	suspendDueKey:    member=sid, score=suspend deadline ms (lastActiveAt + idleTimeout)
//	checkpointDueKey: member=sid, score=next periodic-checkpoint deadline ms
//
// Maintained atomically inside the session write/stamp Lua scripts; a session is
// removed from both when it leaves Running (suspend/reset/delete).
const (
	suspendDueKey    = "idx:suspend:due"
	checkpointDueKey = "idx:checkpoint:due"
)

// Client is a thin wrapper over a Valkey/Redis connection with the assignment
// operations. Safe for concurrent use.
type Client struct {
	rdb *redis.Client
}

// New dials addr (host:port) and returns a Client. The caller owns Close.
func New(addr string) *Client {
	return &Client{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

// NewFromRedis wraps an existing redis client (useful for tests / miniredis).
func NewFromRedis(rdb *redis.Client) *Client { return &Client{rdb: rdb} }

// Ping verifies connectivity.
func (c *Client) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

// Close releases the connection.
func (c *Client) Close() error { return c.rdb.Close() }

// ---- sessions ----

// GetSession returns the session entry, or ErrNotFound.
func (c *Client) GetSession(ctx context.Context, sid string) (*resumeapi.SessionEntry, error) {
	b, err := c.rdb.Get(ctx, sessionKey(sid)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var e resumeapi.SessionEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// casSessionScript sets session:<sid> only if the stored Version matches
// expectVersion (or the key is absent and expectVersion==0). On success it stores
// the new value with Version incremented and returns the new version. On mismatch
// it returns -1.
//
// KEYS[1]=sessionKey KEYS[2]=suspendDueKey KEYS[3]=checkpointDueKey
// ARGV[1]=expectVersion ARGV[2]=newValueJSON ARGV[3]=nowMillis
// On success it also maintains the due-indexes: a Running session with a positive
// idleTimeoutSeconds is scored in suspendDueKey at lastActiveAt+timeout; one with a
// positive checkpointIntervalSeconds is scored in checkpointDueKey at
// last(checkpoint|active)+interval. A non-Running session is removed from both.
var casSessionScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
local expect = tonumber(ARGV[1])
local curver = 0
if cur then
  local ok, obj = pcall(cjson.decode, cur)
  if ok and obj.version then curver = obj.version end
end
if curver ~= expect then
  return -1
end
local newobj = cjson.decode(ARGV[2])
newobj.version = expect + 1
redis.call('SET', KEYS[1], cjson.encode(newobj))
local sid = newobj.sid
local now = tonumber(ARGV[3])
if newobj.state == 'Running' then
  local la = tonumber(newobj.lastActiveAt) or 0
  local ito = tonumber(newobj.idleTimeoutSeconds) or 0
  if la > 0 and ito > 0 then
    redis.call('ZADD', KEYS[2], la + ito*1000, sid)
  else
    redis.call('ZREM', KEYS[2], sid)
  end
  local ci = tonumber(newobj.checkpointIntervalSeconds) or 0
  if ci > 0 then
    local base = tonumber(newobj.lastCheckpointAt) or 0
    if base == 0 then base = la end
    if base == 0 then base = now end
    redis.call('ZADD', KEYS[3], base + ci*1000, sid)
  else
    redis.call('ZREM', KEYS[3], sid)
  end
else
  redis.call('ZREM', KEYS[2], sid)
  redis.call('ZREM', KEYS[3], sid)
end
return newobj.version
`)

// PutSessionCAS writes e only if the current stored version equals e.Version
// (0 for a create). On success e.Version is advanced to the new value. Returns
// ErrVersionConflict on mismatch.
func (c *Client) PutSessionCAS(ctx context.Context, e *resumeapi.SessionEntry) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return err
	}
	res, err := casSessionScript.Run(ctx, c.rdb,
		[]string{sessionKey(e.SID), suspendDueKey, checkpointDueKey},
		e.Version, string(payload), nowMillis()).Int64()
	if err != nil {
		return err
	}
	if res < 0 {
		return ErrVersionConflict
	}
	e.Version = res
	return nil
}

// DeleteSession removes a session entry and its index membership (reset/GC).
func (c *Client) DeleteSession(ctx context.Context, sid string) error {
	if err := c.rdb.Del(ctx, sessionKey(sid)).Err(); err != nil {
		return err
	}
	c.rdb.ZRem(ctx, suspendDueKey, sid)
	c.rdb.ZRem(ctx, checkpointDueKey, sid)
	return nil
}

// stampActiveScript sets only the lastActiveAt field on an existing session entry
// WITHOUT touching version — activity stamping (O3) is advisory metadata, not a
// state transition, so it must not contend with the operator's CAS writes. No-op
// if the session key is absent. It also slides the session's suspend-due score
// forward (lastActiveAt+idleTimeout) so the indexed sweeper sees the fresh
// deadline — O(log N), still hot-path cheap.
// KEYS[1]=sessionKey KEYS[2]=suspendDueKey  ARGV[1]=unixMillis
var stampActiveScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if not cur then return 0 end
local obj = cjson.decode(cur)
local now = tonumber(ARGV[1])
obj.lastActiveAt = now
redis.call('SET', KEYS[1], cjson.encode(obj))
local ito = tonumber(obj.idleTimeoutSeconds) or 0
if obj.state == 'Running' and ito > 0 then
  redis.call('ZADD', KEYS[2], now + ito*1000, obj.sid)
end
return 1
`)

// StampActive updates a session's lastActiveAt (router-observed request activity,
// O3). Advisory: does not bump version, so it never conflicts with resume CAS. It
// also slides the suspend-due index forward so idle detection stays accurate
// without a full-table scan.
func (c *Client) StampActive(ctx context.Context, sid string, unixMillis int64) error {
	return stampActiveScript.Run(ctx, c.rdb, []string{sessionKey(sid), suspendDueKey}, unixMillis).Err()
}

// SuspendDue returns the sessions whose suspend deadline is at/before nowMillis —
// i.e. those idle past their timeout — via the suspend:due index. O(due) instead
// of O(N): the sweeper reads only sessions that are actually due, not the whole
// table (PRD-control-plane-scalability §5.3).
func (c *Client) SuspendDue(ctx context.Context, nowMillis int64) ([]*resumeapi.SessionEntry, error) {
	return c.dueEntries(ctx, suspendDueKey, nowMillis)
}

// CheckpointDue returns the sessions whose next periodic-checkpoint deadline is
// at/before nowMillis, via the checkpoint:due index. Empty when no session opts in.
func (c *Client) CheckpointDue(ctx context.Context, nowMillis int64) ([]*resumeapi.SessionEntry, error) {
	return c.dueEntries(ctx, checkpointDueKey, nowMillis)
}

// dueEntries reads sids scored <= nowMillis from a due-index ZSET and loads their
// entries. Stale index members (entry gone) are pruned opportunistically.
func (c *Client) dueEntries(ctx context.Context, zkey string, nowMillis int64) ([]*resumeapi.SessionEntry, error) {
	sids, err := c.rdb.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key: zkey, Start: "-inf", Stop: strconv.FormatInt(nowMillis, 10), ByScore: true,
	}).Result()
	if err != nil {
		return nil, err
	}
	if len(sids) == 0 {
		return nil, nil
	}
	// Batch-load all due entries with one MGET instead of N sequential GETs (this runs
	// on both the suspend and checkpoint sweepers every ~30s). Order matches sids.
	keys := make([]string, len(sids))
	for i, sid := range sids {
		keys[i] = sessionKey(sid)
	}
	vals, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*resumeapi.SessionEntry, 0, len(sids))
	var stale []string
	for i, v := range vals {
		if v == nil { // entry gone: stale index member
			stale = append(stale, sids[i])
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		var e resumeapi.SessionEntry
		if json.Unmarshal([]byte(s), &e) == nil {
			out = append(out, &e)
		}
	}
	if len(stale) > 0 {
		c.rdb.ZRem(ctx, zkey, stale) // prune stale index members in one call
	}
	return out, nil
}

// ListSessions scans and returns all session entries. O(N) over sessions; used by
// the operator's startup rebuild + GC, NOT the hot sweep path (which uses the
// due-indexes above).
func (c *Client) ListSessions(ctx context.Context) ([]*resumeapi.SessionEntry, error) {
	var out []*resumeapi.SessionEntry
	var cursor uint64
	for {
		keys, cur, err := c.rdb.Scan(ctx, cursor, "session:*", 100).Result()
		if err != nil {
			return nil, err
		}
		// Batch-load each SCAN page with one MGET instead of a GET per key.
		if len(keys) > 0 {
			vals, gerr := c.rdb.MGet(ctx, keys...).Result()
			if gerr != nil {
				return nil, gerr
			}
			for _, v := range vals {
				s, ok := v.(string)
				if !ok {
					continue // key vanished between SCAN and MGET
				}
				var e resumeapi.SessionEntry
				if json.Unmarshal([]byte(s), &e) == nil {
					out = append(out, &e)
				}
			}
		}
		cursor = cur
		if cursor == 0 {
			break
		}
	}
	return out, nil
}

// ---- workers (written only by the operator's discovery informer, TDD §4.3) ----

// GetWorker returns the worker entry, or ErrNotFound.
func (c *Client) GetWorker(ctx context.Context, pod string) (*resumeapi.WorkerEntry, error) {
	b, err := c.rdb.Get(ctx, workerKey(pod)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var w resumeapi.WorkerEntry
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// upsertWorkerScript writes worker:<pod>, keeps the pool idle-set in sync with the
// worker's state, and records pool membership in the pool "all" set so per-pool
// counts are O(1) (SCARD) instead of a worker:* scan.
//
// CRITICAL — it must NEVER demote a busy worker back to idle. This is the
// discovery-side write (worker_discovery.go re-registers a pod as idle on every pod
// event: readiness flap, pod-deletion-cost patch, informer resync). Those events can
// fire on a pod in the window between an atomic claim (SPOP + SET busy) and the
// session actually starting on it. A blind SET+SADD there would overwrite the busy
// binding and RE-ADD the pod to the idle set, letting a second claim SPOP the same
// worker → two sessions on one worker (the second wedges in Resuming). So: when the
// incoming state is idle but the STORED entry is already busy (claimed), preserve the
// stored (busy) entry and leave it OUT of the idle set. A legitimate busy→idle
// transition goes through releaseWorkerScript / a suspend, never through discovery.
//
// KEYS[1]=workerKey KEYS[2]=poolIdleKey KEYS[3]=poolAllKey
// ARGV[1]=valueJSON ARGV[2]=pod ARGV[3]=state
var upsertWorkerScript = redis.NewScript(`
redis.call('SADD', KEYS[3], ARGV[2])          -- pool membership: always idempotent
if ARGV[3] == 'idle' then
  local cur = redis.call('GET', KEYS[1])
  if cur then
    local w = cjson.decode(cur)
    if w.state == 'busy' then
      return 0                                 -- claimed: keep busy, stay out of idle set
    end
  end
  redis.call('SET', KEYS[1], ARGV[1])
  redis.call('SADD', KEYS[2], ARGV[2])
else
  redis.call('SET', KEYS[1], ARGV[1])
  redis.call('SREM', KEYS[2], ARGV[2])
end
return 1
`)

// UpsertWorker writes/updates a worker entry and keeps the pool idle-set + all-set
// in sync. The operator bumps Version on each write. Registering a worker as idle is
// a no-op on the stored state when the worker is already busy (claimed): discovery
// must never resurrect a claimed worker into the idle set (see upsertWorkerScript).
func (c *Client) UpsertWorker(ctx context.Context, w *resumeapi.WorkerEntry) error {
	w.Version++
	payload, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return upsertWorkerScript.Run(ctx, c.rdb,
		[]string{workerKey(w.Pod), poolIdleKey(w.Pool), poolAllKey(w.Pool)},
		string(payload), w.Pod, w.State).Err()
}

// removeWorkerScript deletes worker:<pod> and removes it from both the idle set and
// the pool all-set.
// KEYS[1]=workerKey KEYS[2]=poolIdleKey KEYS[3]=poolAllKey  ARGV[1]=pod
var removeWorkerScript = redis.NewScript(`
redis.call('DEL', KEYS[1])
redis.call('SREM', KEYS[2], ARGV[1])
redis.call('SREM', KEYS[3], ARGV[1])
return 1
`)

// RemoveWorker deletes a worker entry (pod deleted/NotReady).
func (c *Client) RemoveWorker(ctx context.Context, pod, pool string) error {
	return removeWorkerScript.Run(ctx, c.rdb,
		[]string{workerKey(pod), poolIdleKey(pool), poolAllKey(pool)}, pod).Err()
}

// IdleWorkers returns the pod names currently idle in a pool.
func (c *Client) IdleWorkers(ctx context.Context, pool string) ([]string, error) {
	return c.rdb.SMembers(ctx, poolIdleKey(pool)).Result()
}

// EnsurePoolMember records pod in the pool all-set without rewriting the worker
// entry — an idempotent SADD used to self-heal membership for workers that predate
// the all-set index (no version churn).
func (c *Client) EnsurePoolMember(ctx context.Context, pod, pool string) error {
	return c.rdb.SAdd(ctx, poolAllKey(pool), pod).Err()
}

// claimWorkerScript atomically pops an idle worker from the pool idle set, marks
// its worker record busy+bound to the session, and returns the worker JSON. It
// enforces both CAS invariants (TDD §4.2) in one transaction: a worker leaves the
// idle set exactly once, so two concurrent claims cannot grab the same worker.
// Returns "" (empty) when the pool has no idle worker.
//
// KEYS[1]=poolIdleKey  ARGV[1]=sid
var claimWorkerScript = redis.NewScript(`
local pod = redis.call('SPOP', KEYS[1])
if not pod then
  return ''
end
local wkey = 'worker:' .. pod
local cur = redis.call('GET', wkey)
if not cur then
  return ''            -- stale idle-set entry; caller retries
end
local w = cjson.decode(cur)
w.state = 'busy'
w.sid = ARGV[1]
w.version = (w.version or 0) + 1
redis.call('SET', wkey, cjson.encode(w))
return cjson.encode(w)
`)

// ClaimIdleWorker atomically claims one idle worker in pool for sid, marking it
// busy. Returns ErrNoCapacity if none are idle. The claim is durable in KV; the
// caller then drives /run|/restore and binds the session.
func (c *Client) ClaimIdleWorker(ctx context.Context, pool, sid string) (*resumeapi.WorkerEntry, error) {
	res, err := claimWorkerScript.Run(ctx, c.rdb, []string{poolIdleKey(pool)}, sid).Text()
	if err != nil {
		return nil, err
	}
	if res == "" {
		return nil, ErrNoCapacity
	}
	var w resumeapi.WorkerEntry
	if err := json.Unmarshal([]byte(res), &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// releaseWorkerScript marks a worker idle again and re-adds it to the pool idle
// set (used to roll back a failed claim). KEYS[1]=workerKey KEYS[2]=poolIdleKey
// ARGV[1]=pod
//
// The SADD to the idle set is GUARDED by the worker entry still existing: if the
// worker:<pod> key is gone (its pod was deleted / pruned, e.g. a failed fork
// materialization racing pool scale-in), re-adding it to the idle set would create a
// PHANTOM idle member with no backing entry. That member is invisible to
// PruneStaleWorkers (which scans worker:* keys), lets ClaimIdleWorker SPOP a dead
// pod, and skews CountWorkers so busy = total(all) - idle goes NEGATIVE. So: no
// worker entry -> also remove any stale idle membership and do not re-add.
var releaseWorkerScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur then
  local w = cjson.decode(cur)
  w.state = 'idle'
  w.sid = ''
  w.version = (w.version or 0) + 1
  redis.call('SET', KEYS[1], cjson.encode(w))
  redis.call('SADD', KEYS[2], ARGV[1])
else
  redis.call('SREM', KEYS[2], ARGV[1])   -- worker gone: don't resurrect a phantom idle member
end
return 1
`)

// ReleaseWorker returns a claimed worker to the idle pool (rollback on a failed
// resume, before the pod-informer would otherwise repair it).
func (c *Client) ReleaseWorker(ctx context.Context, pod, pool string) error {
	return releaseWorkerScript.Run(ctx, c.rdb,
		[]string{workerKey(pod), poolIdleKey(pool)}, pod).Err()
}

// ListWorkerPods returns the pod names of all worker entries currently in KV
// (across all pools). Used by the reconciliation sweep to prune entries whose
// pods no longer exist.
func (c *Client) ListWorkerPods(ctx context.Context) ([]string, error) {
	var pods []string
	var cursor uint64
	for {
		keys, cur, err := c.rdb.Scan(ctx, cursor, "worker:*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			pods = append(pods, k[len("worker:"):])
		}
		cursor = cur
		if cursor == 0 {
			break
		}
	}
	return pods, nil
}

// PoolWorkers returns every WorkerEntry belonging to a pool by scanning the
// worker:* keys. Used to set per-pod scale-in deletion cost (idle vs busy).
// O(N) but N is small (pool size).
// PoolWorkers returns every WorkerEntry in a pool by reading the pool's all-set
// (O(pool size) GETs) instead of scanning the whole worker:* keyspace
// (PRD-control-plane-scalability §5.5). Stale members (entry gone) are pruned.
func (c *Client) PoolWorkers(ctx context.Context, pool string) ([]*resumeapi.WorkerEntry, error) {
	pods, err := c.rdb.SMembers(ctx, poolAllKey(pool)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*resumeapi.WorkerEntry, 0, len(pods))
	for _, pod := range pods {
		w, gerr := c.GetWorker(ctx, pod)
		if errors.Is(gerr, ErrNotFound) {
			c.rdb.SRem(ctx, poolAllKey(pool), pod) // stale membership: prune
			continue
		}
		if gerr == nil {
			out = append(out, w)
		}
	}
	return out, nil
}

// CountWorkers returns (idle, total) worker counts for a pool by scanning the
// worker:* keys. Used for WarmPool status; O(N) but N is small (pool size).
// CountWorkers returns (idle, total) for a pool. Both are O(1) SCARDs on the pool's
// idle-set and all-set — no worker:* scan (PRD-control-plane-scalability §5.5).
func (c *Client) CountWorkers(ctx context.Context, pool string) (idle, total int, err error) {
	i, err := c.rdb.SCard(ctx, poolIdleKey(pool)).Result()
	if err != nil {
		return 0, 0, err
	}
	t, err := c.rdb.SCard(ctx, poolAllKey(pool)).Result()
	if err != nil {
		return 0, 0, err
	}
	return int(i), int(t), nil
}
