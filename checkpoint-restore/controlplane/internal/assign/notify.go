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

package assign

// PoolNotifier lets the resume/suspend paths signal that a pool's worker
// busy/idle state changed, so the WarmPool controller reconciles that pool
// immediately and refreshes its status (near-real-time, O(1) per change — no
// polling). Kept as a tiny interface so assign/resume don't depend on
// controller-runtime; the operator supplies a channel-backed implementation.
type PoolNotifier interface {
	// PoolChanged signals the named pool's assignment state changed. Must be
	// non-blocking and safe from any goroutine; coalescing/drops are fine (the
	// reconcile is level-triggered and re-reads fresh KV state).
	PoolChanged(pool string)
}

// NopNotifier is a no-op PoolNotifier (tests / when notification isn't wired).
type NopNotifier struct{}

// PoolChanged does nothing.
func (NopNotifier) PoolChanged(string) {}
