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

	corev1alpha1 "github.com/jicowan/aio-sandbox/controlplane/api/v1alpha1"
)

// configPolicyForSession is the shared workload-config resolver used by the suspend
// and checkpoint paths (docs/PRD-arbitrary-image-sessions.md §13): the idle/checkpoint
// policy follows image > appRef > pool's own SandboxTemplate. These specs pin that
// precedence so an appRef/generic-pool session resolves policy identically to a
// dedicated-pool session — and prove the classic and inline-image paths are UNCHANGED
// (regression guard for the generic-pool work).
var _ = Describe("configPolicyForSession precedence", func() {
	const ns = "default"

	// A dedicated pool: its SandboxTemplate pins an image + idle/checkpoint policy.
	setupDedicatedPool := func(ctx context.Context, pool, tmpl string, idle, ckpt int) {
		Expect(k8sClient.Create(ctx, &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: tmpl, Namespace: ns},
			Spec: corev1alpha1.SandboxTemplateSpec{
				Image: "ghcr.io/example/pool-img:1",
				Idle:  corev1alpha1.IdlePolicy{TimeoutSeconds: idle, Action: "suspend"},
				CheckpointIntervalSeconds: ckpt,
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: pool, Namespace: ns},
			Spec:       corev1alpha1.WarmPoolSpec{TemplateRef: corev1alpha1.LocalRef{Name: tmpl}, Replicas: 1},
		})).To(Succeed())
	}

	It("dedicated poolRef mode resolves policy from the pool's SandboxTemplate (UNCHANGED)", func() {
		ctx := context.Background()
		setupDedicatedPool(ctx, "cp-pool-classic", "cp-tmpl-classic", 600, 0)
		s := &corev1alpha1.Session{
			Spec: corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "cp-pool-classic"}},
		}
		pol, err := configPolicyForSession(ctx, k8sClient, ns, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(pol.IdleTimeoutSeconds).To(Equal(600))
		Expect(pol.IdleAction).To(Equal("suspend"))
	})

	It("inline image mode resolves to NO policy (config is the image)", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{
			Spec: corev1alpha1.SessionSpec{
				Image:   "ghcr.io/example/inline@sha256:abc",
				PoolRef: &corev1alpha1.LocalRef{Name: "cp-pool-classic"}, // capacity only
			},
		}
		pol, err := configPolicyForSession(ctx, k8sClient, ns, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(pol).To(Equal(sessionConfigPolicy{}), "inline image carries no template policy")
	})

	It("appRef mode resolves policy from the AppTemplate, DECOUPLED from the pool", func() {
		ctx := context.Background()
		setupDedicatedPool(ctx, "cp-pool-generic-cap", "cp-tmpl-pooldefault", 600, 0)
		// The app the session names (its idle=90 must win over the pool template's 600).
		Expect(k8sClient.Create(ctx, &corev1alpha1.AppTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cp-app-redis", Namespace: ns},
			Spec: corev1alpha1.AppTemplateSpec{
				Image: "docker.io/library/redis:7-alpine",
				Idle:  corev1alpha1.IdlePolicy{TimeoutSeconds: 90, Action: "suspend"},
				CheckpointIntervalSeconds: 30,
			},
		})).To(Succeed())
		s := &corev1alpha1.Session{
			Spec: corev1alpha1.SessionSpec{
				PoolRef: &corev1alpha1.LocalRef{Name: "cp-pool-generic-cap"}, // capacity
				AppRef:  &corev1alpha1.LocalRef{Name: "cp-app-redis"},        // workload/policy
			},
		}
		pol, err := configPolicyForSession(ctx, k8sClient, ns, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(pol.IdleTimeoutSeconds).To(Equal(90), "appRef policy must win over the pool's template")
		Expect(pol.CheckpointIntervalSeconds).To(Equal(30))
	})

	It("image wins over appRef when both are set (precedence)", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{
			Spec: corev1alpha1.SessionSpec{
				Image:   "ghcr.io/example/inline@sha256:def",
				AppRef:  &corev1alpha1.LocalRef{Name: "cp-app-redis"},
				PoolRef: &corev1alpha1.LocalRef{Name: "cp-pool-generic-cap"},
			},
		}
		pol, err := configPolicyForSession(ctx, k8sClient, ns, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(pol).To(Equal(sessionConfigPolicy{}), "image is the highest-precedence source")
	})

	It("no poolRef, no appRef, no image resolves to empty (no policy), not an error", func() {
		ctx := context.Background()
		s := &corev1alpha1.Session{Spec: corev1alpha1.SessionSpec{}}
		pol, err := configPolicyForSession(ctx, k8sClient, ns, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(pol).To(Equal(sessionConfigPolicy{}))
	})
})
