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
