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
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
)

// fakeTerminateSuspender records SuspendForTerminate calls.
type fakeTerminateSuspender struct {
	mu    sync.Mutex
	calls []string // sids
	kv    *assign.Client
}

func (f *fakeTerminateSuspender) SuspendForTerminate(ctx context.Context, sid, pod, ip, pool string) error {
	f.mu.Lock()
	f.calls = append(f.calls, sid)
	f.mu.Unlock()
	// mimic the real one's KV side effect so the test can observe it.
	return f.kv.RemoveWorker(ctx, pod, pool)
}

var _ = Describe("WorkerDiscovery checkpoint-on-terminate", func() {
	const ns = "default"

	It("drives SuspendForTerminate when a busy worker's pod is Terminating", func() {
		ctx := context.Background()
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		defer mr.Close()
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))

		// A busy worker in KV bound to a session.
		Expect(kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{
			Pod: "wt-busy", Pool: "pool-t", PodIP: "10.0.0.9", State: resumeapi.WorkerBusy, SID: "s-term",
		})).To(Succeed())

		// A real pod carrying the pool label + a finalizer so a Delete parks it in
		// Terminating (DeletionTimestamp set) instead of vanishing.
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wt-busy", Namespace: ns,
				Labels:     map[string]string{LabelApp: LabelAppWorker, LabelPool: "pool-t"},
				Finalizers: []string{"test/hold"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandboxd", Image: "x"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pod)).To(Succeed()) // now Terminating

		fake := &fakeTerminateSuspender{kv: kv}
		r := &WorkerDiscoveryReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), KV: kv,
			Namespace: ns, TerminateSuspender: fake,
		}
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "wt-busy"}})
		Expect(err).NotTo(HaveOccurred())

		fake.mu.Lock()
		calls := append([]string(nil), fake.calls...)
		fake.mu.Unlock()
		Expect(calls).To(ConsistOf("s-term"), "busy terminating worker should trigger checkpoint-on-terminate")

		// release the finalizer so envtest can reap the pod
		latest := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "wt-busy"}, latest)).To(Succeed())
		latest.Finalizers = nil
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())
	})

	It("does not trigger for an idle terminating worker", func() {
		ctx := context.Background()
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		defer mr.Close()
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
		Expect(kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{
			Pod: "wt-idle", Pool: "pool-t", PodIP: "10.0.0.8", State: resumeapi.WorkerIdle,
		})).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wt-idle", Namespace: ns,
				Labels:     map[string]string{LabelApp: LabelAppWorker, LabelPool: "pool-t"},
				Finalizers: []string{"test/hold"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "sandboxd", Image: "x"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pod)).To(Succeed())

		fake := &fakeTerminateSuspender{kv: kv}
		r := &WorkerDiscoveryReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), KV: kv,
			Namespace: ns, TerminateSuspender: fake,
		}
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "wt-idle"}})
		Expect(err).NotTo(HaveOccurred())

		fake.mu.Lock()
		n := len(fake.calls)
		fake.mu.Unlock()
		Expect(n).To(Equal(0), "idle terminating worker should NOT checkpoint")

		latest := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "wt-idle"}, latest)).To(Succeed())
		latest.Finalizers = nil
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())
	})
})

var _ = Describe("WorkerDiscovery reclaim orphaned bindings", func() {
	const pool = "pool-r"
	grace := 300 * time.Second

	// newReclaimer wires a reclaimer over a fresh miniredis with an injectable clock.
	// It does NOT touch envtest — reclaim reads only KV. Returns the reconciler, the
	// KV client, and a pointer to the mutable clock.
	newReclaimer := func(ctx context.Context) (*WorkerDiscoveryReconciler, *assign.Client, *time.Time, func()) {
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
		clk := time.Unix(1_000_000, 0)
		r := &WorkerDiscoveryReconciler{
			KV:           kv,
			Namespace:    "default",
			ReclaimGrace: grace,
			Now:          func() time.Time { return clk },
			anomalous:    map[string]anomalyMark{},
		}
		return r, kv, &clk, mr.Close
	}

	busy := func(ctx context.Context, kv *assign.Client, pod, sid string) {
		Expect(kv.UpsertWorker(ctx, &resumeapi.WorkerEntry{
			Pod: pod, Pool: pool, PodIP: "10.0.0.1", State: resumeapi.WorkerBusy, SID: sid,
		})).To(Succeed())
	}
	sess := func(ctx context.Context, kv *assign.Client, sid, state, workerPod string) {
		Expect(kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
			SID: sid, State: state, Pool: pool, WorkerPod: workerPod, Version: 0,
		})).To(Succeed())
	}
	isIdle := func(ctx context.Context, kv *assign.Client, pod string) bool {
		w, err := kv.GetWorker(ctx, pod)
		Expect(err).NotTo(HaveOccurred())
		return w.State == resumeapi.WorkerIdle && w.SID == ""
	}

	It("reclaims a busy worker whose session no longer exists (orphan) after grace + a second sweep", func() {
		ctx := context.Background()
		r, kv, clk, done := newReclaimer(ctx)
		defer done()
		busy(ctx, kv, "wr-orphan", "s-gone") // no session entry created

		// First sweep: only arms, never reclaims (two-strike).
		n, err := r.ReclaimOrphanBindings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0))
		Expect(isIdle(ctx, kv, "wr-orphan")).To(BeFalse())

		// Advance past grace; second sweep reclaims.
		*clk = clk.Add(grace + time.Second)
		n, err = r.ReclaimOrphanBindings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		Expect(isIdle(ctx, kv, "wr-orphan")).To(BeTrue())
		idle, err := kv.IdleWorkers(ctx, pool)
		Expect(err).NotTo(HaveOccurred())
		Expect(idle).To(ContainElement("wr-orphan"))
	})

	It("reclaims a busy worker bound to a Suspended session", func() {
		ctx := context.Background()
		r, kv, clk, done := newReclaimer(ctx)
		defer done()
		busy(ctx, kv, "wr-susp", "s-susp")
		sess(ctx, kv, "s-susp", resumeapi.StateSuspended, "")

		_, _ = r.ReclaimOrphanBindings(ctx) // arm
		*clk = clk.Add(grace + time.Second)
		n, err := r.ReclaimOrphanBindings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		Expect(isIdle(ctx, kv, "wr-susp")).To(BeTrue())
	})

	It("reclaims a busy worker whose session rebound to a different pod", func() {
		ctx := context.Background()
		r, kv, clk, done := newReclaimer(ctx)
		defer done()
		busy(ctx, kv, "wr-old", "s-move")
		sess(ctx, kv, "s-move", resumeapi.StateRunning, "wr-new") // bound elsewhere

		_, _ = r.ReclaimOrphanBindings(ctx) // arm
		*clk = clk.Add(grace + time.Second)
		n, err := r.ReclaimOrphanBindings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		Expect(isIdle(ctx, kv, "wr-old")).To(BeTrue())
	})

	It("does NOT reclaim a live Running session bound to this worker, ever", func() {
		ctx := context.Background()
		r, kv, clk, done := newReclaimer(ctx)
		defer done()
		busy(ctx, kv, "wr-live", "s-live")
		sess(ctx, kv, "s-live", resumeapi.StateRunning, "wr-live")

		_, _ = r.ReclaimOrphanBindings(ctx)
		*clk = clk.Add(grace * 10) // well past any grace
		n, err := r.ReclaimOrphanBindings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0))
		Expect(isIdle(ctx, kv, "wr-live")).To(BeFalse())
	})

	It("does NOT reclaim during the claim->bind window (session appears + version bumps before grace)", func() {
		ctx := context.Background()
		r, kv, clk, done := newReclaimer(ctx)
		defer done()
		// Simulate ClaimIdleWorker: busy, no session yet.
		busy(ctx, kv, "wr-claim", "s-inflight")
		n, err := r.ReclaimOrphanBindings(ctx) // arms (orphan)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0))

		// Resume lands within the deadline: session created (Resuming) + worker version
		// bumped (a real bind rewrites the worker entry). Advance a little (< grace).
		*clk = clk.Add(60 * time.Second)
		w, _ := kv.GetWorker(ctx, "wr-claim")
		Expect(kv.UpsertWorker(ctx, w)).To(Succeed()) // version++ (as bind does)
		sess(ctx, kv, "s-inflight", resumeapi.StateResuming, "wr-claim")

		// Even long after, it must never be reclaimed (now live + version changed).
		*clk = clk.Add(grace * 5)
		n, err = r.ReclaimOrphanBindings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0))
		Expect(isIdle(ctx, kv, "wr-claim")).To(BeFalse())
	})

	It("does NOT reclaim when a busy binding's version keeps changing (never version-stable across grace)", func() {
		ctx := context.Background()
		r, kv, clk, done := newReclaimer(ctx)
		defer done()
		busy(ctx, kv, "wr-churn", "s-churn") // orphan

		_, _ = r.ReclaimOrphanBindings(ctx) // arm at version v
		// Version changes (re-upsert) then grace elapses: the changed version re-arms,
		// so the two-strike + stable-version gate must NOT fire on this sweep.
		w, _ := kv.GetWorker(ctx, "wr-churn")
		Expect(kv.UpsertWorker(ctx, w)).To(Succeed()) // version++
		*clk = clk.Add(grace + time.Second)
		n, err := r.ReclaimOrphanBindings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0), "version changed since first-seen: re-arm, don't reclaim yet")
		Expect(isIdle(ctx, kv, "wr-churn")).To(BeFalse())
	})

	It("is disabled when ReclaimGrace is 0", func() {
		ctx := context.Background()
		r, kv, clk, done := newReclaimer(ctx)
		defer done()
		r.ReclaimGrace = 0
		busy(ctx, kv, "wr-off", "s-gone")
		*clk = clk.Add(time.Hour)
		n, err := r.ReclaimOrphanBindings(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(0))
		Expect(isIdle(ctx, kv, "wr-off")).To(BeFalse())
	})
})
