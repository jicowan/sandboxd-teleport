//go:build linux

// microVM networking: the tap + TC-mirror wiring that hands cloud-hypervisor's
// virtio-net a fd-backed device cross-connected to the interior-netns veth.
//
// PROVENANCE: ported from Agent Substrate's cmd/ateom-microvm/net.go (Apache-2.0,
// Copyright 2026 Google LLC). The veth/interior-netns/nftables half lives in the
// ateomnet package (also ported); this file is the tap/TC-mirror + fd-extraction
// half, adapted to chDriver. Model verified live on the nested-virt node.
//
// Licensed under the Apache License, Version 2.0.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/jicowan/aio-sandbox/sandboxd/ateomnet"
)

const (
	// hostVethMAC is deliberately FIXED (locally administered), unlike gVisor
	// where the kernel's random veth MAC is fine. A CH snapshot freezes the guest
	// kernel's ARP cache, including the entry for the gateway 169.254.17.1;
	// restoring against a new veth pair with a random MAC would blackhole guest
	// egress until that entry expires. A constant gateway MAC keeps the frozen
	// entry valid on every worker.
	hostVethMAC = "02:a8:1e:00:00:01"

	// actorGuestMAC is the FIXED MAC for the guest's eth0 (the CH virtio-net).
	// Fixed for the same reason: a cold boot freezes this MAC into the guest +
	// snapshot, and restore re-adds the virtio-net under the same MAC
	// (SnapshotNetDevices reads it back), so the guest's frozen interface config
	// stays valid across workers. Distinct from the gateway MAC (…:01).
	actorGuestMAC = "02:a8:1e:00:00:02"
)

var hostVethHWAddr = ateomnet.MustParseMAC(hostVethMAC)

// setupRestoreTap recreates, in the interior netns, the tap + TC-mirror wiring
// kata's tcfilter network model builds at boot: a tap device cross-connected to
// eth0 (the actor veth peer) with mirred-redirect ingress filters in both
// directions. Returns the open tap FDs (one per queue pair) for cloud-hypervisor
// to adopt via vm.add-net / vm.restore net_fds (the virtio-net device is
// fd-backed, so CH requires fresh FDs). Call after ateomnet.SetupActorNetwork.
func (d *chDriver) setupRestoreTap(ctx context.Context, name string, queuePairs int) ([]*os.File, error) {
	var fds []*os.File
	err := ateomnet.NetNSDo(ctx, d.interiorNetNS, func(ctx context.Context) error {
		eth0, err := netlink.LinkByName(ateomnet.ActorVethName)
		if err != nil {
			return fmt.Errorf("acquiring actor veth in interior netns: %w", err)
		}
		if old, lerr := netlink.LinkByName(name); lerr == nil {
			_ = netlink.LinkDel(old)
		}
		flags := netlink.TUNTAP_NO_PI | netlink.TUNTAP_VNET_HDR
		if queuePairs > 1 {
			flags |= netlink.TUNTAP_MULTI_QUEUE
		}
		tap := &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: name, MTU: eth0.Attrs().MTU},
			Mode:      netlink.TUNTAP_MODE_TAP,
			Flags:     flags,
			Queues:    queuePairs,
		}
		if err := netlink.LinkAdd(tap); err != nil {
			return fmt.Errorf("creating tap %q: %w", name, err)
		}
		fds = tap.Fds
		if err := netlink.LinkSetUp(tap); err != nil {
			return fmt.Errorf("bringing up tap %q: %w", name, err)
		}
		// Cross-connect: everything arriving on the veth peer redirects out the
		// tap and vice versa (kata's TCFilterModel: ingress qdisc + match-all u32
		// with a mirred egress-redirect action, here via U32.RedirIndex).
		for _, pair := range [][2]netlink.Link{{eth0, tap}, {tap, eth0}} {
			qdisc := &netlink.Ingress{QdiscAttrs: netlink.QdiscAttrs{
				LinkIndex: pair[0].Attrs().Index,
				Parent:    netlink.HANDLE_INGRESS,
				Handle:    netlink.MakeHandle(0xffff, 0),
			}}
			if err := netlink.QdiscReplace(qdisc); err != nil {
				return fmt.Errorf("adding ingress qdisc to %q: %w", pair[0].Attrs().Name, err)
			}
			filter := &netlink.U32{
				FilterAttrs: netlink.FilterAttrs{
					LinkIndex: pair[0].Attrs().Index,
					Parent:    netlink.MakeHandle(0xffff, 0),
					Priority:  1,
					Protocol:  unix.ETH_P_ALL,
				},
				ClassId:    netlink.MakeHandle(1, 1),
				RedirIndex: pair[1].Attrs().Index,
			}
			if err := netlink.FilterAdd(filter); err != nil {
				return fmt.Errorf("adding mirred filter %s -> %s: %w", pair[0].Attrs().Name, pair[1].Attrs().Name, err)
			}
		}
		return nil
	})
	if err != nil {
		for _, f := range fds {
			_ = f.Close()
		}
		return nil, err
	}
	return fds, nil
}

// actorVethMTU reads the MTU of the actor veth (eth0 in the interior netns) so the
// guest eth0 can be configured with a matching MTU via the agent (UpdateInterface).
// Defaults to 1500 if the link can't be read.
func (d *chDriver) actorVethMTU(ctx context.Context) int {
	mtu := 1500
	_ = ateomnet.NetNSDo(ctx, d.interiorNetNS, func(ctx context.Context) error {
		if l, err := netlink.LinkByName(ateomnet.ActorVethName); err == nil {
			mtu = l.Attrs().MTU
		} else {
			slog.WarnContext(ctx, "Failed to read actor veth MTU; using default",
				slog.String("link", ateomnet.ActorVethName), slog.Int("default_mtu", mtu), slog.Any("err", err))
		}
		return nil
	})
	return mtu
}
