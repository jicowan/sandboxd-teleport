//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ateomnet

// Per-sandbox network bandwidth limits (PRD-sandbox-network-bandwidth-limits).
//
// The cap is enforced HOST-SIDE on the host veth peer (ateom0, in the worker pod
// netns) — the guest can't see or remove it, and it survives teleport because the
// caller re-applies it wherever it rebuilds the veth (SetupActorNetwork on cold boot
// AND restore). The veth-peer direction inversion is the crux:
//
//   - a packet the SANDBOX SENDS arrives at ateom0 as INGRESS (it came from the peer
//     eth0), so a sandbox EGRESS cap = shape ateom0's ingress. Linux can't rate-limit
//     an ingress qdisc directly, so we redirect ateom0's ingress to an IFB device
//     (via a match-all u32 + mirred, the same idiom as the tap TC-mirror) and put a
//     TBF on the IFB's egress.
//   - a packet DESTINED for the sandbox leaves ateom0 as EGRESS (toward eth0), so a
//     sandbox INGRESS cap = a plain TBF on ateom0's root (egress) qdisc.
//
// One sandbox per worker, so a single IFB (bwIfbName) suffices. ApplyBandwidth is
// idempotent (it tears down any prior shaping first) so a restore re-apply is clean.

import (
	"fmt"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// bwIfbName is the per-worker IFB device backing the egress (sandbox→world) cap.
const bwIfbName = "ateom-bwifb"

// BandwidthConfig is a per-sandbox bandwidth cap in BYTES/sec (0 = uncapped for that
// direction). BurstBytes is the token-bucket burst; 0 picks a sane default.
type BandwidthConfig struct {
	EgressBPS  uint64 // sandbox → world
	IngressBPS uint64 // world → sandbox
	BurstBytes uint32
}

// Zero reports whether no cap is set (both directions uncapped).
func (b BandwidthConfig) Zero() bool { return b.EgressBPS == 0 && b.IngressBPS == 0 }

// ApplyBandwidth installs (or clears) the sandbox's bandwidth caps on ateom0. Must run
// in the worker POD netns (where ateom0 lives). Idempotent: it removes any prior
// shaping first, so cold boot and restore both call it safely. A zero config just
// clears shaping.
func ApplyBandwidth(cfg BandwidthConfig) error {
	host, err := netlink.LinkByName(HostVethName)
	if err != nil {
		return fmt.Errorf("bandwidth: host veth %q: %w", HostVethName, err)
	}
	// Always start clean (re-setup / restore / clear).
	clearBandwidth(host)
	if cfg.Zero() {
		return nil
	}

	// INGRESS cap (world → sandbox): TBF on ateom0's root (egress toward the sandbox).
	if cfg.IngressBPS > 0 {
		if err := netlink.QdiscAdd(newTbf(host.Attrs().Index, netlink.HANDLE_ROOT, cfg.IngressBPS, cfg.BurstBytes)); err != nil {
			return fmt.Errorf("bandwidth: ingress TBF on %s: %w", HostVethName, err)
		}
	}

	// EGRESS cap (sandbox → world): redirect ateom0's ingress to an IFB, TBF on the IFB.
	if cfg.EgressBPS > 0 {
		ifb := &netlink.Ifb{LinkAttrs: netlink.LinkAttrs{Name: bwIfbName}}
		if err := netlink.LinkAdd(ifb); err != nil {
			return fmt.Errorf("bandwidth: create ifb %q: %w", bwIfbName, err)
		}
		if err := netlink.LinkSetUp(ifb); err != nil {
			return fmt.Errorf("bandwidth: ifb up: %w", err)
		}
		// ingress qdisc on ateom0 + match-all u32 mirred-redirect to the ifb (same
		// idiom as the tap TC-mirror in microvm_net.go).
		ingress := &netlink.Ingress{QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: host.Attrs().Index,
			Parent:    netlink.HANDLE_INGRESS,
			Handle:    netlink.MakeHandle(0xffff, 0),
		}}
		if err := netlink.QdiscReplace(ingress); err != nil {
			return fmt.Errorf("bandwidth: ingress qdisc on %s: %w", HostVethName, err)
		}
		redir := &netlink.U32{
			FilterAttrs: netlink.FilterAttrs{
				LinkIndex: host.Attrs().Index,
				Parent:    netlink.MakeHandle(0xffff, 0),
				Priority:  1,
				Protocol:  unix.ETH_P_ALL,
			},
			Actions: []netlink.Action{&netlink.MirredAction{
				ActionAttrs:  netlink.ActionAttrs{Action: netlink.TC_ACT_STOLEN},
				MirredAction: netlink.TCA_EGRESS_REDIR,
				Ifindex:      ifb.Attrs().Index,
			}},
		}
		if err := netlink.FilterAdd(redir); err != nil {
			return fmt.Errorf("bandwidth: ingress→ifb redirect: %w", err)
		}
		if err := netlink.QdiscAdd(newTbf(ifb.Attrs().Index, netlink.HANDLE_ROOT, cfg.EgressBPS, cfg.BurstBytes)); err != nil {
			return fmt.Errorf("bandwidth: egress TBF on ifb: %w", err)
		}
	}
	return nil
}

// ClearBandwidth removes any bandwidth shaping (best-effort; teardown/failure paths).
func ClearBandwidth() {
	if host, err := netlink.LinkByName(HostVethName); err == nil {
		clearBandwidth(host)
	}
}

// clearBandwidth drops the IFB device and ateom0's root+ingress qdiscs. Best-effort:
// a missing device/qdisc is not an error (the state we want).
func clearBandwidth(host netlink.Link) {
	if ifb, err := netlink.LinkByName(bwIfbName); err == nil {
		_ = netlink.LinkDel(ifb) // also drops its TBF
	}
	_ = netlink.QdiscDel(&netlink.Ingress{QdiscAttrs: netlink.QdiscAttrs{
		LinkIndex: host.Attrs().Index, Parent: netlink.HANDLE_INGRESS, Handle: netlink.MakeHandle(0xffff, 0),
	}})
	_ = netlink.QdiscDel(&netlink.GenericQdisc{QdiscAttrs: netlink.QdiscAttrs{
		LinkIndex: host.Attrs().Index, Parent: netlink.HANDLE_ROOT,
	}, QdiscType: "tbf"})
}

// newTbf builds a token-bucket qdisc capping to rateBPS bytes/sec. burst defaults to
// ~1/8s of rate (min 32 KiB) when 0; the queue limit holds ~400ms of traffic so a
// bursty sender is smoothed, not hard-dropped, up to that depth.
func newTbf(linkIndex int, parent uint32, rateBPS uint64, burst uint32) *netlink.Tbf {
	if burst == 0 {
		burst = uint32(rateBPS / 8)
		if burst < 32<<10 {
			burst = 32 << 10
		}
	}
	limit := uint32(rateBPS*400/1000) + burst // ~400ms of queue + burst
	return &netlink.Tbf{
		QdiscAttrs: netlink.QdiscAttrs{LinkIndex: linkIndex, Parent: parent, Handle: netlink.MakeHandle(1, 0)},
		Rate:       rateBPS,
		Buffer:     burst,
		Limit:      limit,
	}
}
