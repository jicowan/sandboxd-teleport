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

// Package metrics defines the control-plane Prometheus metrics and registers
// them with controller-runtime's global registry (served on the operator's
// existing metrics endpoint — no new server). Covers the resume path (P5) and the
// pool busy/idle gauge (the real-time-at-scale observability the TDD flagged).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Outcome label values for resume/suspend results.
const (
	OutcomeSuccess    = "success"
	OutcomeNoCapacity = "no_capacity"
	OutcomeError      = "error"
)

// Kind label values distinguishing cold start from restore-on-connect.
const (
	KindColdStart = "cold_start"
	KindRestore   = "restore"
)

var (
	// PoolWorkers is the near-real-time gauge of workers per pool by state
	// (idle|busy). Set from KV on each WarmPool reconcile.
	PoolWorkers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sandboxd_pool_workers",
		Help: "Number of sandboxd workers per pool by state (idle|busy).",
	}, []string{"pool", "state"})

	// ResumesTotal counts resume attempts by kind (cold_start|restore) and outcome.
	ResumesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_resumes_total",
		Help: "Total resume attempts by kind and outcome.",
	}, []string{"kind", "outcome"})

	// ResumeDuration is resume latency (seconds) by kind, for successful resumes —
	// the time-to-ready that gates the request (TTFB clock, O8).
	ResumeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sandboxd_resume_duration_seconds",
		Help:    "Resume latency (claim -> ready) in seconds, by kind.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 15, 30, 60},
	}, []string{"kind"})

	// SuspendsTotal counts idle-suspend actions by action (suspend|reset) and outcome.
	SuspendsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_suspends_total",
		Help: "Total idle-suspend actions by action and outcome.",
	}, []string{"action", "outcome"})

	// CheckpointsTotal counts periodic background checkpoints by outcome (P5).
	CheckpointsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_periodic_checkpoints_total",
		Help: "Total periodic background checkpoints by outcome.",
	}, []string{"outcome"})

	// SweepDuration is how long one sweep pass takes, by sweep (suspend|checkpoint).
	// With the O(due) indexes this should stay flat as the fleet grows; a rise is the
	// signal that the indexing is no longer keeping up (PRD-control-plane-scalability §6).
	SweepDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sandboxd_sweep_duration_seconds",
		Help:    "Duration of one idle-suspend/checkpoint sweep pass, by sweep.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2},
	}, []string{"sweep"})

	// SweepDue is the number of sessions a sweep found DUE (read from the index) on
	// its last pass, by sweep. Watching due-count vs. total session count tells us
	// whether the indexed path is doing O(due) work as intended.
	SweepDue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sandboxd_sweep_due",
		Help: "Sessions found due on the last sweep pass, by sweep (suspend|checkpoint).",
	}, []string{"sweep"})

	// GCReapedTotal counts session-footprint reaps by the store cleared and the
	// session class that triggered it (PRD-session-garbage-collection §7). store is
	// one of s3|kv|cr; class is ttl|abandoned|orphan-cr|orphan-s3. In dry-run these
	// count what WOULD be reaped, tagged separately via GCCandidates.
	GCReapedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sandboxd_gc_reaped_total",
		Help: "Session-footprint items reaped by GC, by store (s3|kv|cr) and class.",
	}, []string{"store", "class"})

	// GCCandidates is the number of items the last GC pass identified as reapable,
	// by class — set regardless of dry-run, so a dry-run deploy still shows what the
	// armed GC would remove (the validation signal before arming; PRD §8).
	GCCandidates = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sandboxd_gc_candidates",
		Help: "Reapable items identified on the last GC pass, by class (ttl|abandoned|orphan-cr|orphan-s3).",
	}, []string{"class"})
)

// register wires everything into the controller-runtime registry exactly once.
func init() {
	ctrlmetrics.Registry.MustRegister(
		PoolWorkers, ResumesTotal, ResumeDuration, SuspendsTotal, CheckpointsTotal,
		SweepDuration, SweepDue, GCReapedTotal, GCCandidates,
	)
}

// SetPoolWorkers updates the busy/idle gauge for a pool.
func SetPoolWorkers(pool string, idle, busy int) {
	PoolWorkers.WithLabelValues(pool, "idle").Set(float64(idle))
	PoolWorkers.WithLabelValues(pool, "busy").Set(float64(busy))
}
