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
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// intstrHealth returns the sandboxd plain-HTTP health port (kubelet probes hit this,
// never the mTLS control API on :8090).
func intstrHealth() intstr.IntOrString { return intstr.FromInt32(workerHealthPort) }

// busyCount derives the busy-worker count from the pool's idle + all-set sizes,
// floored at 0. Busy is not tracked directly — it's total(all-set) − idle(idle-set).
// If KV ever drifts so the idle set exceeds the all-set (e.g. a stale idle member),
// the raw subtraction goes NEGATIVE, which would under-provision effReplicas
// (busy+minIdle) and mis-report status. The floor makes a transient drift fail safe
// (never scale a pool below minIdle). See PRD-snapshot-fork.md fan-out hardening.
func busyCount(idle, total int) int32 {
	if busy := total - idle; busy > 0 {
		return int32(busy)
	}
	return 0
}

// deploymentMatches reports whether the existing worker Deployment already has
// the desired replicas and pod template (so we skip a no-op update).
func deploymentMatches(existing, desired *appsv1.Deployment) bool {
	er := int32(1)
	if existing.Spec.Replicas != nil {
		er = *existing.Spec.Replicas
	}
	dr := int32(1)
	if desired.Spec.Replicas != nil {
		dr = *desired.Spec.Replicas
	}
	if er != dr {
		return false
	}
	return equality.Semantic.DeepEqual(existing.Spec.Template, desired.Spec.Template)
}

// setReadyCond upserts the Ready condition on a conditions slice.
// upsertCondition replace-or-appends a condition keyed by Type, preserving the
// existing LastTransitionTime when Status AND Reason are unchanged (so the timestamp
// only moves on a real transition) and stamping Now() otherwise. Shared by every
// controller's condition setter (Ready on WarmPool/ForkSet/BaseSnapshot,
// SuspendRequest on Session). observedGeneration is stamped verbatim (0 = omit).
func upsertCondition(conds *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, msg string, observedGeneration int64) {
	c := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: observedGeneration,
		LastTransitionTime: metav1.Now(),
	}
	for i := range *conds {
		if (*conds)[i].Type == condType {
			if (*conds)[i].Status == status && (*conds)[i].Reason == reason {
				c.LastTransitionTime = (*conds)[i].LastTransitionTime
			}
			(*conds)[i] = c
			return
		}
	}
	*conds = append(*conds, c)
}

// setReadyCond upserts the "Ready" condition (no observedGeneration — WarmPool's
// historical shape). Thin wrapper over upsertCondition.
func setReadyCond(conds *[]metav1.Condition, status metav1.ConditionStatus, reason, msg string) {
	upsertCondition(conds, "Ready", status, reason, msg, 0)
}
