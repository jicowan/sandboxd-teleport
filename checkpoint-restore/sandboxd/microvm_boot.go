//go:build linux

// microVM boot: chDriver.createStart — the cold-boot sequence that turns a
// prepared OCI bundle into a running Cloud Hypervisor microVM with the workload's
// container assembled inside via the kata-agent.
//
// PROVENANCE: the sequence and the helper bodies (ensureKataCompatibleSpec,
// buildVMConfig/buildFsConfigs, writeGuestResolvConf, configureGuestNetwork) are
// ported from Agent Substrate's cmd/ateom-microvm (run.go/spec.go, Apache-2.0,
// Copyright 2026 Google LLC) and adapted to sandboxd's model: sandboxd runs ONE
// container per sandbox and hands createStart a bundle whose rootfs is already
// prepared (prepareRootfsContainerd in main.go's /run), so this does not fetch
// images or manage multi-container actors — it boots one guest and starts one
// container. Boot chain verified live on the nested-virt node.
//
// Licensed under the Apache License, Version 2.0.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jicowan/aio-sandbox/sandboxd/ateomnet"
	"github.com/jicowan/aio-sandbox/sandboxd/ch"
	"github.com/jicowan/aio-sandbox/sandboxd/kata"
	"github.com/jicowan/aio-sandbox/sandboxd/third_party/kata/agentpb"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// bootTimeout bounds the whole cold-boot so a wedged guest can't hang /run forever.
const bootTimeout = 120 * time.Second

// createStart boots a microVM for sandbox id from the prepared OCI bundle and
// starts its single container inside, via the kata-agent. Mirrors substrate's
// coldBootActor, collapsed to sandboxd's one-container-per-sandbox model.
func (d *chDriver) createStart(id, bundle string) (retErr error) {
	if d.interiorNetNS == 0 {
		return fmt.Errorf("microvm createStart: interior netns unavailable (networking disabled)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), bootTimeout)
	defer cancel()

	if d.kernel == "" || d.image() == "" {
		return fmt.Errorf("microvm createStart: SANDBOXD_CH_KERNEL and SANDBOXD_CH_IMAGE (kata guest kernel + rootfs image) must be set")
	}
	rootfs := filepath.Join(bundle, "rootfs")
	netnsPath := filepath.Join("/run/netns", microvmInteriorNetNSName)

	// 1) Shape the bundle's OCI spec for the kata-agent + write guest DNS into the
	//    RO lower (before it's served over virtio-fs).
	spec, err := ensureKataCompatibleSpec(bundle, id, netnsPath)
	if err != nil {
		return fmt.Errorf("microvm spec: %w", err)
	}
	if err := writeGuestResolvConf(rootfs); err != nil {
		return fmt.Errorf("microvm resolv.conf: %w", err)
	}

	// 2) Host networking: per-sandbox veth into the interior netns (the tap is
	//    built after the VM exists, so its fds are fresh).
	if err := ateomnet.SetupActorNetwork(ctx, ateomnet.NetworkConfig{
		InteriorNetNS:      d.interiorNetNS,
		HostVethHWAddr:     hostVethHWAddr,
		SweepInteriorLinks: true,
	}); err != nil {
		return fmt.Errorf("microvm network setup: %w", err)
	}
	defer func() {
		if retErr != nil {
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			_ = ateomnet.CleanupActorNetwork(cctx, d.interiorNetNS)
			ccancel()
		}
	}()

	// 3) Guest sizing + agent kernel params from the kata config.
	memMiB, vcpus, kparams, err := d.guestConfig()
	if err != nil {
		return err
	}

	// 4) Clean stale per-sandbox state + create the VM runtime dir for the sockets.
	kata.CleanupSandboxState(ctx, id)
	if err := os.MkdirAll(kata.VMDir(id), 0o700); err != nil {
		return fmt.Errorf("microvm vm dir: %w", err)
	}

	// 5) Stage the overlay RO lower (bind the bundle rootfs into virtiofsd's
	//    find-paths dir) + start the virtiofsd that serves it. CH demand-pages from
	//    it for the sandbox lifetime, so we own the process (killed in delete).
	if err := kata.ReconstructSharedDirFromImage(ctx, rootfs, id, id); err != nil {
		return fmt.Errorf("microvm stage rootfs: %w", err)
	}
	vfsdLog, _ := os.OpenFile(filepath.Join(kata.VMDir(id), "virtiofsd.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	vfsdCmd, err := kata.StartVirtiofsd(ctx, kata.VirtiofsdOptions{
		Binary:     d.virtiofsd,
		SocketPath: kata.VirtiofsdSocketPath(id),
		SharedDir:  kata.SharedDir(id),
		Log:        vfsdLog,
	})
	if err != nil {
		return fmt.Errorf("microvm virtiofsd: %w", err)
	}
	defer func() {
		if retErr != nil && vfsdCmd.Process != nil {
			_ = vfsdCmd.Process.Kill()
			_, _ = vfsdCmd.Process.Wait()
		}
	}()

	// 6) Launch a bare VMM (CH + api-socket); we own this process for teardown.
	//    Capture CH's stdout/stderr to clh.log so a VMM that exits early leaves a
	//    diagnosable trail (its warnings + the reason it died) instead of vanishing.
	apiSocket := filepath.Join(kata.VMDir(id), "clh-api.sock")
	launchOpts := ch.LaunchVMMOptions{Binary: d.chBin, APISocket: apiSocket}
	if lf := chLog(id); lf != nil {
		launchOpts.Stdout, launchOpts.Stderr = lf, lf
	}
	chCmd, client, err := ch.LaunchVMM(ctx, launchOpts)
	if err != nil {
		return fmt.Errorf("microvm launch VMM: %w", err)
	}
	defer func() {
		if retErr != nil && chCmd.Process != nil {
			_ = chCmd.Process.Kill()
			_, _ = chCmd.Process.Wait()
		}
	}()

	// 7) vm.create with the kata-compatible config (RO guest image on /dev/vda + the
	//    virtio-fs RO lower on PCI segment 1; writable upper is a guest tmpfs).
	serialLog := filepath.Join(kata.VMDir(id), "serial.log")
	vmCfg := buildVMConfig(id, d.kernel, d.image(), kparams, serialLog, memMiB, vcpus)
	if err := client.CreateVM(ctx, vmCfg); err != nil {
		return fmt.Errorf("microvm vm.create: %w", err)
	}

	// 8) Build the tap in the interior netns + add a fd-backed virtio-net to the
	//    created (pre-boot) VM (SCM_RIGHTS).
	tapFiles, err := d.setupRestoreTap(ctx, "tap0_kata", 1)
	if err != nil {
		return fmt.Errorf("microvm tap: %w", err)
	}
	defer func() {
		for _, f := range tapFiles {
			_ = f.Close() // CH dups adopted FDs; ours always close.
		}
	}()
	var fds []int
	for _, f := range tapFiles {
		fds = append(fds, int(f.Fd()))
	}
	if err := client.AddNetWithFDs(ctx, actorGuestMAC, 2*len(tapFiles), fds); err != nil {
		return fmt.Errorf("microvm add-net: %w", err)
	}

	// 9) Boot.
	if err := client.BootVM(ctx); err != nil {
		return fmt.Errorf("microvm vm.boot: %w", err)
	}

	// 10) Dial the kata-agent over hybrid-vsock. CH creates the socket FILE at
	//     vm.create (instantly), but the guest agent doesn't listen on the vsock
	//     port until several seconds into boot (systemd -> kata-containers.target),
	//     so we must retry the CONNECT handshake, not just wait for the file — a
	//     single early CONNECT races the boot and gets EOF.
	vsockPath := kata.VsockSocketPath(id)
	if !waitForFileMV(vsockPath, 15*time.Second) {
		return fmt.Errorf("microvm: kata-agent vsock %q did not appear", vsockPath)
	}
	ac, err := kata.DialAgentRetry(ctx, vsockPath, 60*time.Second)
	if err != nil {
		return fmt.Errorf("microvm dial agent: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = ac.Close()
		}
	}()

	// 11) Post-boot agent setup: sandbox, guest networking, then the container as
	//     overlay(virtio-fs RO lower + guest-tmpfs upper).
	if err := ac.CreateSandboxForActor(ctx, id, spec.Hostname, false); err != nil {
		return fmt.Errorf("microvm create sandbox: %w", err)
	}
	if err := d.configureGuestNetwork(ctx, ac, uint64(d.actorVethMTU(ctx))); err != nil {
		return fmt.Errorf("microvm guest network: %w", err)
	}
	if err := ac.CreateCarrier(ctx, id, spec); err != nil {
		return fmt.Errorf("microvm carrier: %w", err)
	}
	if err := ac.StartOverlayWorkload(ctx, id, id+"_ovl", kata.OverlayUpperBase(id), spec); err != nil {
		return fmt.Errorf("microvm start workload: %w", err)
	}

	d.mu.Lock()
	d.vms[id] = &chVM{id: id, apiSocket: apiSocket, chCmd: chCmd, vfsdCmd: vfsdCmd, agent: ac}
	d.mu.Unlock()
	return nil
}

// image returns the kata guest rootfs image path (ext4, RO /dev/vda), from
// SANDBOXD_CH_IMAGE.
func (d *chDriver) image() string {
	return envOr("SANDBOXD_CH_IMAGE", "/usr/local/share/kata/kata-containers.img")
}

// guestConfig reads guest sizing + agent kernel params from the kata config,
// enabling the debug console for diagnostics.
func (d *chDriver) guestConfig() (memMiB, vcpus int, kparams string, err error) {
	var cfgBytes []byte
	if cf := envOr("SANDBOXD_KATA_CONFIG", ""); cf != "" {
		cfgBytes, _ = os.ReadFile(cf)
	}
	cfg, err := kata.ParseConfig(cfgBytes, 2048, 1)
	if err != nil {
		return 0, 0, "", fmt.Errorf("parse kata config: %w", err)
	}
	return cfg.MemoryMiB, cfg.VCPUs, kata.WithDebugConsole(cfg.KernelParams), nil
}

// configureGuestNetwork performs the shim's guest-side networking via the agent:
// eth0 IP/MAC/MTU, routes, and a pinned ARP entry for the gateway (fixed MAC).
func (d *chDriver) configureGuestNetwork(ctx context.Context, ac *kata.AgentClient, mtu uint64) error {
	if err := ac.UpdateInterface(ctx, &agentpb.Interface{
		Device: ateomnet.ActorVethName,
		Name:   ateomnet.ActorVethName,
		HwAddr: actorGuestMAC,
		Mtu:    mtu,
		IPAddresses: []*agentpb.IPAddress{
			{Family: agentpb.IPFamily_v4, Address: ateomnet.ActorVethIP, Mask: "30"},
		},
	}); err != nil {
		return err
	}
	if err := ac.UpdateRoutes(ctx, []*agentpb.Route{
		{Dest: ateomnet.ActorVethSubnet, Device: ateomnet.ActorVethName, Scope: uint32(unix.RT_SCOPE_LINK), Family: agentpb.IPFamily_v4},
		{Dest: "", Gateway: ateomnet.ActorVethGateway, Device: ateomnet.ActorVethName, Family: agentpb.IPFamily_v4},
	}); err != nil {
		return err
	}
	return ac.AddARPNeighbors(ctx, []*agentpb.ARPNeighbor{{
		ToIPAddress: &agentpb.IPAddress{Family: agentpb.IPFamily_v4, Address: ateomnet.ActorVethGateway},
		Device:      ateomnet.ActorVethName,
		Lladdr:      hostVethMAC,
		State:       0x80, // NUD_PERMANENT
	}})
}

// buildVMConfig assembles the cloud-hypervisor VmConfig: kata-compatible cmdline,
// RO kata guest image on /dev/vda + the virtio-fs RO lower on PCI segment 1, no
// actor disks (the writable upper is a guest tmpfs).
func buildVMConfig(id, kernel, image, kparams, serialLog string, memMiB, vcpus int) ch.VmConfig {
	console := "ttyS0"
	if runtime.GOARCH == "arm64" {
		console = "ttyAMA0"
	}
	cmdline := "root=/dev/vda1 rootflags=data=ordered,errors=remount-ro ro rootfstype=ext4 " +
		"panic=1 no_timer_check noreplace-smp console=" + console + ",115200n8 " +
		"systemd.unit=kata-containers.target systemd.mask=systemd-networkd.service systemd.mask=systemd-networkd.socket"
	if kparams != "" {
		cmdline += " " + kparams
	}
	return ch.VmConfig{
		Cpus:    ch.CpusConfig{BootVcpus: int32(vcpus), MaxVcpus: int32(vcpus)},
		Memory:  ch.MemoryConfig{Size: int64(memMiB) * 1024 * 1024, Shared: true},
		Payload: ch.PayloadConfig{Kernel: kernel, Cmdline: cmdline},
		Disks: []ch.DiskConfig{
			{Path: image, Readonly: true, ImageType: "Raw", NumQueues: int32(vcpus), QueueSize: 1024},
		},
		Fs: []ch.FsConfig{{
			Tag: kata.FsTag, Socket: kata.VirtiofsdSocketPath(id),
			NumQueues: 1, QueueSize: 1024, PciSegment: 1,
		}},
		Platform: &ch.PlatformConfig{NumPciSegments: 2},
		Rng:      &ch.RngConfig{Src: "/dev/urandom"},
		Serial:   &ch.ConsoleConfig{Mode: "File", File: serialLog},
		Vsock:    &ch.VsockConfig{Cid: 3, Socket: kata.VsockSocketPath(id)},
	}
}

// ensureKataCompatibleSpec augments the bundle's config.json with the fields the
// kata-agent requires (linux.resources, cgroupsPath), strips gVisor/CRI
// annotations, points the netns at the interior netns, and replaces the mounts
// with the kata-agent-accepted set. Ported from substrate spec.go.
func ensureKataCompatibleSpec(bundle, id, netnsPath string) (*specs.Spec, error) {
	specPath := filepath.Join(bundle, "config.json")
	b, err := os.ReadFile(specPath)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", specPath, err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, fmt.Errorf("parsing %q: %w", specPath, err)
	}
	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}
	if spec.Linux.Resources == nil {
		spec.Linux.Resources = defaultKataResources()
	}
	if spec.Linux.CgroupsPath == "" {
		spec.Linux.CgroupsPath = "/sandboxdchv/" + id
	}
	for k := range spec.Annotations {
		if strings.HasPrefix(k, "io.kubernetes.cri.") {
			delete(spec.Annotations, k)
		}
	}
	netnsSet := false
	for i := range spec.Linux.Namespaces {
		if spec.Linux.Namespaces[i].Type == specs.NetworkNamespace {
			spec.Linux.Namespaces[i].Path = netnsPath
			netnsSet = true
		}
	}
	if !netnsSet {
		spec.Linux.Namespaces = append(spec.Linux.Namespaces, specs.LinuxNamespace{
			Type: specs.NetworkNamespace, Path: netnsPath,
		})
	}
	spec.Mounts = defaultKataMounts()

	out, err := json.MarshalIndent(&spec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling spec: %w", err)
	}
	if err := os.WriteFile(specPath, out, 0o600); err != nil {
		return nil, fmt.Errorf("writing %q: %w", specPath, err)
	}
	return &spec, nil
}

// defaultKataMounts mirrors the mount set `ctr run --runtime io.containerd.kata.v2`
// produces (the proven-good shape for the kata agent).
func defaultKataMounts() []specs.Mount {
	return []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/run", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
	}
}

// defaultKataResources mirrors the device allowlist + cpu shares that
// `ctr run --runtime io.containerd.kata.v2` emits.
func defaultKataResources() *specs.LinuxResources {
	dev := func(t string, major, minor int64, access string) specs.LinuxDeviceCgroup {
		d := specs.LinuxDeviceCgroup{Allow: true, Type: t, Access: access}
		if major != 0 {
			d.Major = &major
		}
		if minor >= 0 {
			d.Minor = &minor
		}
		return d
	}
	shares := uint64(1024)
	return &specs.LinuxResources{
		Devices: []specs.LinuxDeviceCgroup{
			{Allow: false, Access: "rwm"},
			dev("c", 1, 3, "rwm"),
			dev("c", 1, 8, "rwm"),
			dev("c", 1, 7, "rwm"),
			dev("c", 5, 0, "rwm"),
			dev("c", 1, 5, "rwm"),
			dev("c", 1, 9, "rwm"),
			dev("c", 5, 1, "rwm"),
			dev("c", 136, -1, "rwm"),
			dev("c", 5, 2, "rwm"),
		},
		CPU: &specs.LinuxCPU{Shares: &shares},
	}
}

// writeGuestResolvConf copies the worker's /etc/resolv.conf into the bundle rootfs
// (the overlay RO lower) so the guest gets cluster DNS.
func writeGuestResolvConf(rootfs string) error {
	src, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil // best-effort; no host resolv.conf => skip
	}
	etc := filepath.Join(rootfs, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(etc, "resolv.conf"), src, 0o644)
}

// waitForFileMV polls for path to exist, up to d (the kata-agent hybrid-vsock
// socket appears during guest boot before the agent listens).
func waitForFileMV(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
