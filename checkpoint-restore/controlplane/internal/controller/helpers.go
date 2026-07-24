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
func setReadyCond(conds *[]metav1.Condition, status metav1.ConditionStatus, reason, msg string) {
	meta := metav1.Condition{
		Type:    "Ready",
		Status:  status,
		Reason:  reason,
		Message: msg,
	}
	// find + replace, else append
	for i := range *conds {
		if (*conds)[i].Type == "Ready" {
			if (*conds)[i].Status != status || (*conds)[i].Reason != reason {
				meta.LastTransitionTime = metav1.Now()
			} else {
				meta.LastTransitionTime = (*conds)[i].LastTransitionTime
			}
			(*conds)[i] = meta
			return
		}
	}
	meta.LastTransitionTime = metav1.Now()
	*conds = append(*conds, meta)
}
