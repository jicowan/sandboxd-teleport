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
	"github.com/jicowan/aio-sandbox/controlplane/internal/resume"
)

// configPolicyForSession is the shared workload-config resolver used by the suspend
// and checkpoint paths (docs/sandboxd/PRD/PRD-arbitrary-image-sessions.md §13): the idle/checkpoint
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
				Image:                     "ghcr.io/example/pool-img:1",
				Ports:                     testPorts(),
				Health:                    testHealth(),
				Idle:                      corev1alpha1.IdlePolicy{TimeoutSeconds: idle, Action: "suspend"},
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
				Image:                     "docker.io/library/redis:7-alpine",
				Ports:                     testPorts(),
				Health:                    testHealth(),
				Idle:                      corev1alpha1.IdlePolicy{TimeoutSeconds: 90, Action: "suspend"},
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

// applyLifecycleOverride: a Session's spec.lifecycle overrides the template-resolved
// idle policy for BOTH timeout and action. The action override is what lets a ForkSet
// make ephemeral (reset) or durable (suspend) forks without a dedicated pool
// (docs/sandboxd/PRD/PRD-snapshot-fork.md §5.3/§5.5) — the regression that stranded fork workers
// because the sweeper ignored SessionLifecycle.IdleAction.
var _ = Describe("applyLifecycleOverride", func() {
	base := resume.IdlePolicy{TimeoutSeconds: 600, Action: "suspend"} // template default

	It("overrides action AND timeout when both set (ephemeral fork)", func() {
		got := applyLifecycleOverride(base, corev1alpha1.SessionLifecycle{
			IdleTimeoutSeconds: 60, IdleAction: "reset",
		})
		Expect(got.TimeoutSeconds).To(Equal(60))
		Expect(got.Action).To(Equal("reset"))
	})

	It("overrides only the action when timeout is unset", func() {
		got := applyLifecycleOverride(base, corev1alpha1.SessionLifecycle{IdleAction: "reset"})
		Expect(got.TimeoutSeconds).To(Equal(600), "timeout inherits the template")
		Expect(got.Action).To(Equal("reset"))
	})

	It("inherits the template when lifecycle is empty (classic session UNCHANGED)", func() {
		got := applyLifecycleOverride(base, corev1alpha1.SessionLifecycle{})
		Expect(got).To(Equal(base))
	})
})

// resolveWorkloadSource enforces the generic/dedicated admission at the operator's
// authoritative chokepoint (docs/sandboxd/PRD/PRD-arbitrary-image-sessions.md §13.3). These specs
// pin the accept/reject matrix so a dedicated pool stays single-image and a generic
// pool only runs foreign workloads — the Stage 2b guarantee.
var _ = Describe("resolveWorkloadSource generic/dedicated admission", func() {
	const ns = "default"

	// dedicated pool: SandboxTemplate pins an image.
	mkDedicated := func(ctx context.Context, pool, tmpl string) {
		Expect(k8sClient.Create(ctx, &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: tmpl, Namespace: ns},
			Spec:       corev1alpha1.SandboxTemplateSpec{Image: "ghcr.io/example/dedicated:1", Ports: testPorts(), Health: testHealth()},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: pool, Namespace: ns},
			Spec:       corev1alpha1.WarmPoolSpec{TemplateRef: corev1alpha1.LocalRef{Name: tmpl}, Replicas: 1},
		})).To(Succeed())
	}
	// generic pool: SandboxTemplate leaves image empty (worker-shape only).
	mkGeneric := func(ctx context.Context, pool, tmpl string) {
		Expect(k8sClient.Create(ctx, &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: tmpl, Namespace: ns},
			Spec:       corev1alpha1.SandboxTemplateSpec{}, // no image => generic
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1alpha1.WarmPool{
			ObjectMeta: metav1.ObjectMeta{Name: pool, Namespace: ns},
			Spec:       corev1alpha1.WarmPoolSpec{TemplateRef: corev1alpha1.LocalRef{Name: tmpl}, Replicas: 1},
		})).To(Succeed())
	}
	resolve := func(ctx context.Context, spec corev1alpha1.SessionSpec) (*resume.SessionPlan, error) {
		s := &corev1alpha1.Session{ObjectMeta: metav1.ObjectMeta{Name: "sess-adm", Namespace: ns}, Spec: spec}
		plan := &resume.SessionPlan{}
		if spec.PoolRef != nil {
			plan.Pool = spec.PoolRef.Name
		}
		err := resolveWorkloadSource(ctx, k8sClient, ns, s, plan)
		return plan, err
	}

	It("ACCEPT: poolRef-only on a DEDICATED pool → runs the pool's image", func() {
		ctx := context.Background()
		mkDedicated(ctx, "adm-ded", "adm-ded-tmpl")
		plan, err := resolve(ctx, corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "adm-ded"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.TemplateName).To(Equal("adm-ded-tmpl"))
		Expect(plan.AppName).To(BeEmpty())
	})

	It("REJECT: poolRef-only on a GENERIC pool → nothing to run", func() {
		ctx := context.Background()
		mkGeneric(ctx, "adm-gen1", "adm-gen1-tmpl")
		_, err := resolve(ctx, corev1alpha1.SessionSpec{PoolRef: &corev1alpha1.LocalRef{Name: "adm-gen1"}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("generic"))
	})

	It("ACCEPT: appRef on a GENERIC pool", func() {
		ctx := context.Background()
		mkGeneric(ctx, "adm-gen2", "adm-gen2-tmpl")
		Expect(k8sClient.Create(ctx, &corev1alpha1.AppTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "adm-app", Namespace: ns},
			Spec:       corev1alpha1.AppTemplateSpec{Image: "docker.io/library/redis:7-alpine", Ports: testPorts(), Health: testHealth()},
		})).To(Succeed())
		plan, err := resolve(ctx, corev1alpha1.SessionSpec{
			PoolRef: &corev1alpha1.LocalRef{Name: "adm-gen2"},
			AppRef:  &corev1alpha1.LocalRef{Name: "adm-app"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.AppName).To(Equal("adm-app"))
		Expect(plan.TemplateName).To(BeEmpty())
	})

	It("REJECT: appRef on a DEDICATED pool", func() {
		ctx := context.Background()
		mkDedicated(ctx, "adm-ded2", "adm-ded2-tmpl")
		_, err := resolve(ctx, corev1alpha1.SessionSpec{
			PoolRef: &corev1alpha1.LocalRef{Name: "adm-ded2"},
			AppRef:  &corev1alpha1.LocalRef{Name: "adm-app"},
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("dedicated"))
	})

	It("REJECT: appRef without poolRef (no capacity)", func() {
		ctx := context.Background()
		_, err := resolve(ctx, corev1alpha1.SessionSpec{AppRef: &corev1alpha1.LocalRef{Name: "adm-app"}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("requires poolRef"))
	})

	It("REJECT: inline image on a DEDICATED pool", func() {
		ctx := context.Background()
		mkDedicated(ctx, "adm-ded3", "adm-ded3-tmpl")
		_, err := resolve(ctx, corev1alpha1.SessionSpec{
			PoolRef: &corev1alpha1.LocalRef{Name: "adm-ded3"},
			Image:   "ghcr.io/example/inline@sha256:abc",
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("dedicated"))
	})

	It("ACCEPT: inline image on a GENERIC pool", func() {
		ctx := context.Background()
		mkGeneric(ctx, "adm-gen3", "adm-gen3-tmpl")
		plan, err := resolve(ctx, corev1alpha1.SessionSpec{
			PoolRef: &corev1alpha1.LocalRef{Name: "adm-gen3"},
			Image:   "ghcr.io/example/inline@sha256:def",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(plan.Image).To(Equal("ghcr.io/example/inline@sha256:def"))
	})
})
