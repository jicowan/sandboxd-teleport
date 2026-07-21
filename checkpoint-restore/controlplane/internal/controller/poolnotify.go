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

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
)

// PoolNotifier bridges the resume/suspend paths' assign.PoolNotifier interface to
// a controller-runtime channel event source: PoolChanged(pool) emits a
// GenericEvent for that WarmPool, causing the WarmPool controller to reconcile it
// immediately and refresh idle/busy status (near-real-time, no polling).
//
// It satisfies assign.PoolNotifier. Sends are non-blocking: if the buffered
// channel is full the event is dropped — safe because reconciliation is
// level-triggered (it re-reads fresh KV), so a coalesced/dropped nudge only
// delays the next refresh until another event or resync.
type PoolNotifier struct {
	ch        chan event.GenericEvent
	namespace string
}

// NewPoolNotifier returns a notifier and the channel to feed the WarmPool
// controller (via SetupWithManager's PoolEvents). namespace is where WarmPools
// live. buf sizes the channel (e.g. 256).
func NewPoolNotifier(namespace string, buf int) *PoolNotifier {
	if buf <= 0 {
		buf = 256
	}
	return &PoolNotifier{ch: make(chan event.GenericEvent, buf), namespace: namespace}
}

// Events returns the channel to wire into WarmPoolReconciler.PoolEvents.
func (n *PoolNotifier) Events() <-chan event.GenericEvent { return n.ch }

// PoolChanged implements assign.PoolNotifier: enqueue a reconcile of the pool.
func (n *PoolNotifier) PoolChanged(pool string) {
	if pool == "" {
		return
	}
	obj := &corev1alpha1.WarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: pool, Namespace: n.namespace},
	}
	select {
	case n.ch <- event.GenericEvent{Object: obj}:
	default: // full: drop (level-triggered reconcile will catch up)
	}
}
