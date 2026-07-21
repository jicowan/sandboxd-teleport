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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
	"github.com/jicowan/aio-sandbox/controlplane/internal/assign"
	"github.com/jicowan/aio-sandbox/shared/resumeapi"
	"github.com/jicowan/aio-sandbox/shared/sbxapi"
)

var _ = Describe("Session durability (etcd-as-truth, Valkey-as-cache)", func() {
	const ns = "default"

	It("round-trips a KV entry through Session.status losslessly", func() {
		e := &resumeapi.SessionEntry{
			SID: "s-rt", State: resumeapi.StateSuspended, Pool: "p1",
			WorkerPod: "w1", WorkerPodIP: "10.0.0.5", Image: "img:1",
			SnapshotURI: "sandboxes/s-rt/snap-1", IAMRoleARN: "arn:aws:iam::1:role/r",
			Ports:        []sbxapi.PortMap{{Container: 8080, Host: 8080}},
			Health:       &sbxapi.Health{Probe: "http", ProbePort: 8080, ProbePath: "/h", RestartPolicy: "restore"},
			LastActiveAt: 1_700_000_000_000,
		}
		var st corev1alpha1.SessionStatus
		applyEntryToStatus(e, &st)
		back := statusToEntry("s-rt", &st)

		Expect(back.State).To(Equal(e.State))
		Expect(back.Pool).To(Equal(e.Pool))
		Expect(back.WorkerPod).To(Equal(e.WorkerPod))
		Expect(back.WorkerPodIP).To(Equal(e.WorkerPodIP))
		Expect(back.Image).To(Equal(e.Image))
		Expect(back.SnapshotURI).To(Equal(e.SnapshotURI))
		Expect(back.IAMRoleARN).To(Equal(e.IAMRoleARN))
		Expect(back.Ports).To(Equal(e.Ports))
		Expect(back.Health).To(Equal(e.Health))
		Expect(back.LastActiveAt).To(Equal(e.LastActiveAt))
		Expect(back.Version).To(Equal(int64(0)), "rebuilt entry writes cleanly into a wiped cache")
	})

	It("mirrors a KV entry into Session.status and rebuilds the cache after a wipe", func() {
		ctx := context.Background()
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		defer mr.Close()
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))

		// A Session CR must exist for the mirror to write status onto (planFor owns
		// creation in prod; here we create it directly).
		sess := &corev1alpha1.Session{
			ObjectMeta: metav1.ObjectMeta{Name: "s-dur", Namespace: ns},
			Spec:       corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "p1"}},
		}
		Expect(k8sClient.Create(ctx, sess)).To(Succeed())

		mirror := NewSessionMirror(k8sClient, ns)
		entry := &resumeapi.SessionEntry{
			SID: "s-dur", State: resumeapi.StateSuspended, Pool: "p1",
			Image: "img:2", SnapshotURI: "sandboxes/s-dur/snap-9",
			Ports: []sbxapi.PortMap{{Container: 6379, Host: 6379}},
		}
		mirror.Mirror(ctx, entry)

		// status is now durable in etcd
		var got corev1alpha1.Session
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "s-dur"}, &got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(resumeapi.StateSuspended))
		Expect(got.Status.SnapshotURI).To(Equal("sandboxes/s-dur/snap-9"))
		Expect(got.Status.Pool).To(Equal("p1"))

		// Simulate a Valkey wipe: nothing in KV. Rebuild from Session CRs.
		if _, err := kv.GetSession(ctx, "s-dur"); err == nil {
			// ensure empty
			_ = kv.DeleteSession(ctx, "s-dur")
		}
		reb := NewSessionRebuilder(k8sClient, kv, ns)
		n, err := reb.RebuildOnce(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeNumerically(">=", 1))

		restored, err := kv.GetSession(ctx, "s-dur")
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.State).To(Equal(resumeapi.StateSuspended))
		Expect(restored.SnapshotURI).To(Equal("sandboxes/s-dur/snap-9"))
		Expect(restored.Pool).To(Equal("p1"))
		Expect(restored.Ports).To(Equal([]sbxapi.PortMap{{Container: 6379, Host: 6379}}))
	})

	It("rebuild leaves an already-present KV entry untouched (KV wins in normal op)", func() {
		ctx := context.Background()
		mr, err := miniredis.Run()
		Expect(err).NotTo(HaveOccurred())
		defer mr.Close()
		kv := assign.NewFromRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))

		sess := &corev1alpha1.Session{
			ObjectMeta: metav1.ObjectMeta{Name: "s-live", Namespace: ns},
			Spec:       corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "p1"}},
		}
		Expect(k8sClient.Create(ctx, sess)).To(Succeed())
		NewSessionMirror(k8sClient, ns).Mirror(ctx, &resumeapi.SessionEntry{
			SID: "s-live", State: resumeapi.StateSuspended, Pool: "p1", SnapshotURI: "old",
		})

		// KV holds a FRESHER Running entry (as in normal operation)
		Expect(kv.PutSessionCAS(ctx, &resumeapi.SessionEntry{
			SID: "s-live", State: resumeapi.StateRunning, Pool: "p1", WorkerPodIP: "10.0.0.9",
		})).To(Succeed())

		reb := NewSessionRebuilder(k8sClient, kv, ns)
		_, err = reb.RebuildOnce(ctx)
		Expect(err).NotTo(HaveOccurred())

		cur, err := kv.GetSession(ctx, "s-live")
		Expect(err).NotTo(HaveOccurred())
		Expect(cur.State).To(Equal(resumeapi.StateRunning), "rebuild must not clobber a live KV entry")
	})
})
