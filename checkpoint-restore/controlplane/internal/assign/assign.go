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
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

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
// KEYS[1]=sessionKey  ARGV[1]=expectVersion  ARGV[2]=newValueJSON(with version placeholder)
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
		[]string{sessionKey(e.SID)}, e.Version, string(payload)).Int64()
	if err != nil {
		return err
	}
	if res < 0 {
		return ErrVersionConflict
	}
	e.Version = res
	return nil
}

// DeleteSession removes a session entry (used on reset/GC).
func (c *Client) DeleteSession(ctx context.Context, sid string) error {
	return c.rdb.Del(ctx, sessionKey(sid)).Err()
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

// upsertWorkerScript writes worker:<pod> and adds/removes it from the pool idle
// set atomically based on state, so the idle index never disagrees with the
// worker record.
//
// KEYS[1]=workerKey KEYS[2]=poolIdleKey  ARGV[1]=valueJSON ARGV[2]=pod ARGV[3]=state
var upsertWorkerScript = redis.NewScript(`
redis.call('SET', KEYS[1], ARGV[1])
if ARGV[3] == 'idle' then
  redis.call('SADD', KEYS[2], ARGV[2])
else
  redis.call('SREM', KEYS[2], ARGV[2])
end
return 1
`)

// UpsertWorker writes/updates a worker entry and keeps the pool idle-set in sync.
// The operator bumps Version on each write.
func (c *Client) UpsertWorker(ctx context.Context, w *resumeapi.WorkerEntry) error {
	w.Version++
	payload, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return upsertWorkerScript.Run(ctx, c.rdb,
		[]string{workerKey(w.Pod), poolIdleKey(w.Pool)},
		string(payload), w.Pod, w.State).Err()
}

// removeWorkerScript deletes worker:<pod> and removes it from the idle set.
var removeWorkerScript = redis.NewScript(`
redis.call('DEL', KEYS[1])
redis.call('SREM', KEYS[2], ARGV[1])
return 1
`)

// RemoveWorker deletes a worker entry (pod deleted/NotReady).
func (c *Client) RemoveWorker(ctx context.Context, pod, pool string) error {
	return removeWorkerScript.Run(ctx, c.rdb,
		[]string{workerKey(pod), poolIdleKey(pool)}, pod).Err()
}

// IdleWorkers returns the pod names currently idle in a pool.
func (c *Client) IdleWorkers(ctx context.Context, pool string) ([]string, error) {
	return c.rdb.SMembers(ctx, poolIdleKey(pool)).Result()
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
var releaseWorkerScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur then
  local w = cjson.decode(cur)
  w.state = 'idle'
  w.sid = ''
  w.version = (w.version or 0) + 1
  redis.call('SET', KEYS[1], cjson.encode(w))
end
redis.call('SADD', KEYS[2], ARGV[1])
return 1
`)

// ReleaseWorker returns a claimed worker to the idle pool (rollback on a failed
// resume, before the pod-informer would otherwise repair it).
func (c *Client) ReleaseWorker(ctx context.Context, pod, pool string) error {
	return releaseWorkerScript.Run(ctx, c.rdb,
		[]string{workerKey(pod), poolIdleKey(pool)}, pod).Err()
}

// CountWorkers returns (idle, total) worker counts for a pool by scanning the
// worker:* keys. Used for WarmPool status; O(N) but N is small (pool size).
func (c *Client) CountWorkers(ctx context.Context, pool string) (idle, total int, err error) {
	idleMembers, err := c.IdleWorkers(ctx, pool)
	if err != nil {
		return 0, 0, err
	}
	idle = len(idleMembers)
	var cursor uint64
	for {
		keys, cur, serr := c.rdb.Scan(ctx, cursor, "worker:*", 100).Result()
		if serr != nil {
			return 0, 0, serr
		}
		for _, k := range keys {
			w, gerr := c.GetWorker(ctx, k[len("worker:"):])
			if gerr == nil && w.Pool == pool {
				total++
			}
		}
		cursor = cur
		if cursor == 0 {
			break
		}
	}
	return idle, total, nil
}

// fmtAddr is a tiny helper so callers can build host:port without importing net.
func Addr(host string, port int) string { return fmt.Sprintf("%s:%d", host, port) }
