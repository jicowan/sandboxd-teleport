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

// Health CONSISTENCY CEL guardrail (issue #2). A probe is NOT required (portless
// batch/exec/headless workloads are valid and become Ready when their process runs);
// only an INCONSISTENT probe — tcp/http with no probePort — is refused at admission.
var _ = Describe("Health-probe consistency CEL", func() {
	const ns = "default"

	It("REJECTS an AppTemplate with a tcp/http probe but no probePort", func() {
		ctx := context.Background()
		app := &corev1alpha1.AppTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cel-app-bad", Namespace: ns},
			Spec: corev1alpha1.AppTemplateSpec{
				Image:  "docker.io/library/redis:7-alpine",
				Ports:  []corev1alpha1.PortMap{{Container: 8080, Host: 8080}},
				Health: &corev1alpha1.Health{Probe: "http"}, // no ProbePort -> inconsistent
			},
		}
		err := k8sClient.Create(ctx, app)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("probePort"))
	})

	It("ACCEPTS an AppTemplate with a tcp/http probe AND a probePort", func() {
		ctx := context.Background()
		app := &corev1alpha1.AppTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cel-app-good", Namespace: ns},
			Spec: corev1alpha1.AppTemplateSpec{
				Image:  "docker.io/library/redis:7-alpine",
				Ports:  []corev1alpha1.PortMap{{Container: 8080, Host: 8080}},
				Health: &corev1alpha1.Health{Probe: "http", ProbePort: 8080, ProbePath: "/h"},
			},
		}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
	})

	It("ACCEPTS a portless AppTemplate with NO health probe (batch/exec workload)", func() {
		ctx := context.Background()
		app := &corev1alpha1.AppTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cel-app-headless", Namespace: ns},
			Spec: corev1alpha1.AppTemplateSpec{
				Image: "docker.io/library/busybox:latest",
				Cmd:   []string{"sh", "-c", "echo done"},
				// no Ports, no Health -> ready-when-running; must be allowed.
			},
		}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
	})

	It("ACCEPTS an AppTemplate with probe: none (explicitly no readiness probe)", func() {
		ctx := context.Background()
		app := &corev1alpha1.AppTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cel-app-probenone", Namespace: ns},
			Spec: corev1alpha1.AppTemplateSpec{
				Image:  "docker.io/library/busybox:latest",
				Health: &corev1alpha1.Health{Probe: "none"},
			},
		}
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
	})

	It("REJECTS a dedicated SandboxTemplate with a tcp probe but no probePort", func() {
		ctx := context.Background()
		st := &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cel-sbxt-bad", Namespace: ns},
			Spec: corev1alpha1.SandboxTemplateSpec{
				Image:  "ghcr.io/example/img:1",
				Ports:  []corev1alpha1.PortMap{{Container: 8080, Host: 8080}},
				Health: &corev1alpha1.Health{Probe: "tcp"}, // no ProbePort
			},
		}
		err := k8sClient.Create(ctx, st)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("probePort"))
	})

	It("ACCEPTS a GENERIC SandboxTemplate (no image, no health) — worker-shape only", func() {
		ctx := context.Background()
		st := &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cel-sbxt-generic", Namespace: ns},
			Spec:       corev1alpha1.SandboxTemplateSpec{}, // image empty => generic; health comes from the AppTemplate
		}
		Expect(k8sClient.Create(ctx, st)).To(Succeed())
	})

	It("ACCEPTS a dedicated SandboxTemplate with a valid probe+port", func() {
		ctx := context.Background()
		st := &corev1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "cel-sbxt-good", Namespace: ns},
			Spec: corev1alpha1.SandboxTemplateSpec{
				Image:  "ghcr.io/example/img:1",
				Ports:  []corev1alpha1.PortMap{{Container: 8080, Host: 8080}},
				Health: &corev1alpha1.Health{Probe: "http", ProbePort: 8080, ProbePath: "/h"},
			},
		}
		Expect(k8sClient.Create(ctx, st)).To(Succeed())
	})
})
