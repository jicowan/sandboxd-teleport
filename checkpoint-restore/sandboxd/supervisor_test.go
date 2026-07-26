//go:build linux

package main

import "testing"

// TestUsesProbe covers the #2 readiness-mode decision: a sandbox is gated on an active
// tcp/http probe ONLY when it declares one AND exposes a port; otherwise it is
// PROCESS-ready ("running == ready"), so portless batch/exec/headless workloads don't
// wedge in Resuming waiting for a probe that never fires.
func TestUsesProbe(t *testing.T) {
	cases := []struct {
		name     string
		probe    string
		numPorts int
		want     bool
	}{
		{"http probe + port -> probe mode", "http", 1, true},
		{"tcp probe + port -> probe mode", "tcp", 2, true},
		{"http probe but NO port -> process mode", "http", 0, false},
		{"no probe + port -> process mode", "", 1, false},
		{"probe none + port -> process mode", "none", 1, false},
		{"nothing -> process mode (batch/exec)", "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := usesProbe(health{Probe: c.probe}, c.numPorts)
			if got != c.want {
				t.Fatalf("usesProbe(probe=%q, ports=%d) = %v, want %v", c.probe, c.numPorts, got, c.want)
			}
		})
	}
}
