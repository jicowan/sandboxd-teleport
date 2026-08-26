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
	"log"
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

// mergedRootfsCache is the virtiofsd --cache mode for the host-merged rootfs share.
// It MUST be write-through so a paused guest's completed writes are already on the
// host upper when checkpoint tars it (rootfs-upper.tar) — otherwise a runtime-written
// file (e.g. a Python __pycache__ .pyc) sits in the guest's writeback page cache, the
// tar misses it, and virtiofsd's find-paths migration fails to re-open it on restore
// (VhostUserCheckDeviceState BackendInternalError). "never" gives write-through (no
// guest file page cache): writes hit the host immediately AND the memory snapshot no
// longer double-counts scratch that's also in the tar. "auto"/"always" enable guest
// writeback caching on this kernel (6.18.35), which breaks the tar's coherence.
// (virtiofsd's valid --cache values are never|auto|always; there is no "none".)
const mergedRootfsCache = "never"

// createStart boots a microVM for sandbox id from the prepared OCI bundle and
// starts its single container inside, via the kata-agent. Mirrors substrate's
// coldBootActor, collapsed to sandboxd's one-container-per-sandbox model.
func (d *chDriver) createStart(id, bundle string, ports []portMap) (retErr error) {
	if d.interiorNetNS == 0 {
		return fmt.Errorf("microvm createStart: interior netns unavailable (networking disabled)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), bootTimeout)
	defer cancel()
	tStart := time.Now() // whole-createStart clock (cold-start latency)

	if d.kernel == "" || (d.image() == "" && d.initrd() == "") {
		return fmt.Errorf("microvm createStart: SANDBOXD_CH_KERNEL and one of SANDBOXD_CH_IMAGE / SANDBOXD_CH_INITRD (kata guest kernel + rootfs image or agent-init initrd) must be set")
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
	// Inbound (router->guest DNAT) + IAM (cred-vendor pin) routing over the veth just
	// built. Retains gVisor's routing model (see setupInboundPorts).
	if err := d.setupInboundPorts(ports); err != nil {
		return fmt.Errorf("microvm inbound ports: %w", err)
	}

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

	// 5) Assemble the container rootfs overlay ON THE HOST (lower = bundle image,
	//    upper/work = a per-sandbox dir on DISK — /work, not guest RAM) and serve the
	//    MERGED tree over virtiofsd. The guest runs the container directly on it (no
	//    guest-side overlay), so rootfs writes cost host disk, not guest RAM — no
	//    ~19%-of-RAM tmpfs write cliff, and scratch no longer inflates the memory
	//    snapshot (PRD-microvm-rootfs-upper-on-host-disk). CH demand-pages from
	//    virtiofsd for the sandbox lifetime, so we own the process (killed in delete).
	if err := resetRootfsUpperDir(id); err != nil {
		return fmt.Errorf("microvm rootfs upper: %w", err)
	}
	if err := kata.StageMergedRootfs(ctx, rootfs, rootfsUpperDir(id), id, id); err != nil {
		return fmt.Errorf("microvm stage merged rootfs: %w", err)
	}
	vfsdLog, _ := os.OpenFile(filepath.Join(kata.VMDir(id), "virtiofsd.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	vfsdCmd, err := kata.StartVirtiofsd(ctx, kata.VirtiofsdOptions{
		Binary:     d.virtiofsd,
		SocketPath: kata.VirtiofsdSocketPath(id),
		SharedDir:  kata.SharedDir(id),
		Cache:      mergedRootfsCache,
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
	vmCfg := buildVMConfig(id, d.kernel, d.image(), d.initrd(), kparams, serialLog, memMiB, vcpus)
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

	// 9) Boot. Stamp tBoot so we can report guest boot time (BootVM -> agent ready) —
	//    the phase the agent-init initrd vs image-disk choice most affects.
	tBoot := time.Now()
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
	// Guest boot time: BootVM -> kata-agent answering. This is the metric to watch when
	// A/B-ing the agent-init initrd (SANDBOXD_CH_INITRD) vs the image disk.
	bootMode := "image"
	if d.initrd() != "" {
		bootMode = "initrd"
	}
	log.Printf("microvm %s: guest booted (%s) — agent ready %s after vm.boot", id, bootMode, time.Since(tBoot).Round(time.Millisecond))
	defer func() {
		if retErr != nil {
			_ = ac.Close()
		}
	}()

	// 11) Post-boot agent setup: sandbox, guest networking, then the container run
	//     DIRECTLY on the host-merged rootfs (no carrier, no guest-side overlay — the
	//     host already merged image+upper and virtiofsd serves the result).
	if err := ac.CreateSandboxForActor(ctx, id, spec.Hostname, false); err != nil {
		return fmt.Errorf("microvm create sandbox: %w", err)
	}
	if err := d.configureGuestNetwork(ctx, ac, uint64(d.actorVethMTU(ctx))); err != nil {
		return fmt.Errorf("microvm guest network: %w", err)
	}
	if err := ac.StartRootfsContainer(ctx, id, spec); err != nil {
		return fmt.Errorf("microvm start workload: %w", err)
	}

	stopOOM := make(chan struct{})
	d.mu.Lock()
	// A cold boot's frozen find-paths base id IS its own id (createStart reconstructs
	// the RO lower + carrier at cid=id below).
	d.vms[id] = &chVM{id: id, apiSocket: apiSocket, chCmd: chCmd, vfsdCmd: vfsdCmd, agent: ac, baseID: id, stopOOM: stopOOM}
	d.mu.Unlock()
	go d.watchOOM(id, ac, stopOOM) // observability parity: surface guest OOM-kills
	if d.streamConsole {
		// Host-merged workload: exec id == id (StartRootfsContainer), not <id>_ovl.
		d.forwardWorkloadLogs(id, id, ac) // relay WORKLOAD stdout/stderr → kubectl logs (opt-in)
	}
	log.Printf("microvm %s: createStart complete (%s) in %s", id, bootMode, time.Since(tStart).Round(time.Millisecond))
	return nil
}

// image returns the kata guest rootfs IMAGE path (ext4, RO /dev/vda), from
// SANDBOXD_CH_IMAGE. Empty when the initrd (agent-init) model is selected, so the
// disk-image boot path is skipped in favor of initrd(). Defaults to the bundled kata
// image ONLY when no initrd is configured.
func (d *chDriver) image() string {
	if d.initrd() != "" {
		return envOr("SANDBOXD_CH_IMAGE", "") // initrd mode: no image disk unless explicitly set
	}
	return envOr("SANDBOXD_CH_IMAGE", "/usr/local/share/kata/kata-containers.img")
}

// initrd returns the kata AGENT-INIT initrd path (SANDBOXD_CH_INITRD). When set, the
// guest boots from this initramfs (kata-agent as PID 1, tmpfs rootfs, no systemd) —
// smaller + faster to the agent than the disk image. Empty = use the image disk.
func (d *chDriver) initrd() string {
	return envOr("SANDBOXD_CH_INITRD", "")
}

// guestConfig reads guest sizing + agent kernel params. The vCPU/memory the guest
// boots with is resolved by this precedence (highest first):
//  1. an explicit kata config (SANDBOXD_KATA_CONFIG default_vcpus/default_memory) or
//     the explicit SANDBOXD_CH_VCPUS / SANDBOXD_CH_MEMORY_MIB overrides — an operator
//     escape hatch for hand-tuned guests;
//  2. the worker POD's resource LIMITS, surfaced by the operator via the downward API
//     (SANDBOXD_POD_CPU_LIMIT / SANDBOXD_POD_MEM_LIMIT) — so a SandboxTemplate's
//     resources.limits size the guest (issue #38). Memory is limit MINUS the agent
//     reserve (sandboxMemLimit) so the guest can never allocate past its own cgroup and
//     OOM-kill the sandboxd agent; vCPUs is ceil(cpu limit), min 1.
//  3. the built-in default (2048 MiB / 1 vCPU).
// This is a chDriver method, so it is microVM-ONLY: the gVisor (runsc) driver never
// calls it and its worker pods are unaffected.
func (d *chDriver) guestConfig() (memMiB, vcpus int, kparams string, err error) {
	var cfgBytes []byte
	if cf := envOr("SANDBOXD_KATA_CONFIG", ""); cf != "" {
		cfgBytes, _ = os.ReadFile(cf)
	}
	cfg, err := kata.ParseConfig(cfgBytes, 2048, 1)
	if err != nil {
		return 0, 0, "", fmt.Errorf("parse kata config: %w", err)
	}
	memMiB, vcpus = deriveGuestSize(
		cfg.MemoryMiB, cfg.VCPUs,
		envInt64("SANDBOXD_CH_MEMORY_MIB", 0),
		envInt64("SANDBOXD_CH_VCPUS", 0),
		envInt64("SANDBOXD_POD_MEM_LIMIT", 0),
		envInt64("SANDBOXD_POD_CPU_LIMIT", 0),
		loadMemReserveConfig(),
	)
	// The kata-agent debug console is a RAW ROOT SHELL in the guest, bound on vsock
	// port 1026 (see kata.DebugConsoleDump). It's reachable by anything that can open
	// the host-side hybrid-vsock socket (/run/vc/vm/<id>/clh.sock, 0700 root, in the
	// worker's private mount ns — today only the sandboxd process). It grants no
	// capability sandboxd doesn't already have over ttrpc, but it's gratuitous guest
	// attack surface, so gate it behind SANDBOXD_DEBUG=1 (the same switch that arms
	// verbose runsc debug + log streaming) rather than binding it unconditionally.
	kparams = cfg.KernelParams
	if os.Getenv("SANDBOXD_DEBUG") == "1" {
		kparams = kata.WithDebugConsole(kparams)
	}
	return memMiB, vcpus, kparams, nil
}

// deriveGuestSize resolves the guest vCPU/memory per the precedence documented on
// guestConfig. Pure (no env/IO) so it is table-tested.
//
//   - cfgMemMiB/cfgVCPUs: values from ParseConfig (kata config or its 2048/1 default).
//     ParseConfig already substitutes the default when a key is absent, so a value that
//     differs from the default means the kata config set it explicitly — that wins.
//   - envMemMiB/envVCPUs: explicit SANDBOXD_CH_MEMORY_MIB / SANDBOXD_CH_VCPUS (0 = unset).
//   - podMemLimit/podCPULimit: the pod's limits.memory (bytes) / limits.cpu (whole
//     cores, ceil'd by the downward API) from the operator (0 = unset).
//   - reserve: the agent memory-reserve config, so guest RAM = podMemLimit − reserve.
func deriveGuestSize(cfgMemMiB, cfgVCPUs int, envMemMiB, envVCPUs, podMemLimit, podCPULimit int64, reserve memReserveConfig) (memMiB, vcpus int) {
	const defMemMiB, defVCPUs = 2048, 1

	// Memory. Explicit CH env or a non-default kata config wins; else derive from the
	// pod memory limit (minus the agent reserve); else the default.
	switch {
	case envMemMiB > 0:
		memMiB = int(envMemMiB)
	case cfgMemMiB != defMemMiB && cfgMemMiB > 0:
		memMiB = cfgMemMiB
	case podMemLimit > 0:
		if guest := sandboxMemLimit(podMemLimit, true, reserve); guest > 0 {
			memMiB = int(guest >> 20) // bytes → MiB
		}
	}
	if memMiB <= 0 {
		memMiB = cfgMemMiB // falls back to the kata default (2048) when nothing derived
		if memMiB <= 0 {
			memMiB = defMemMiB
		}
	}

	// vCPUs. Explicit CH env or a non-default kata config wins; else ceil(cpu limit),
	// min 1 (the downward API already delivers a whole-core, ceil'd value); else default.
	switch {
	case envVCPUs > 0:
		vcpus = int(envVCPUs)
	case cfgVCPUs != defVCPUs && cfgVCPUs > 0:
		vcpus = cfgVCPUs
	case podCPULimit > 0:
		vcpus = int(podCPULimit)
	}
	if vcpus < 1 {
		vcpus = cfgVCPUs
		if vcpus < 1 {
			vcpus = defVCPUs
		}
	}
	return memMiB, vcpus
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
// RO kata guest image on /dev/vda + the virtio-fs RO lower (PCI segment 1 with ACPI,
// segment 0 on the acpi=off fast-boot path), no actor disks (writable upper = tmpfs).
// buildVMConfig assembles the CH VmConfig for one of two guest-rootfs models:
//   - initrd != "": the kata AGENT-INIT initrd is the rootfs (loaded as initramfs,
//     runs on tmpfs, kata-agent is PID 1). No /dev/vda disk, no systemd — smaller and
//     faster to the agent. `image` is ignored.
//   - initrd == "": the kata guest IMAGE boots as a virtio-blk disk on /dev/vda1
//     (root=…, systemd → kata-containers.target).
// The virtio-fs RO lower (workload rootfs) is on PCI segment 1 when the guest has
// ACPI, or segment 0 on the amd64 acpi=off fast-boot path (no MCFG to enumerate a
// non-zero segment — see the acpiOff branch below); the writable overlay upper is a
// guest tmpfs either way.
func buildVMConfig(id, kernel, image, initrd, kparams, serialLog string, memMiB, vcpus int) ch.VmConfig {
	console := "ttyS0"
	if runtime.GOARCH == "arm64" {
		console = "ttyAMA0"
	}
	// acpi=off shaves ~0.34s (~18%) off guest boot (BootVM→agent ready): CH
	// direct-boots the guest, so ACPI table parsing + ACPI-driven device probing is
	// dead weight. Safe here because we boot a FIXED topology (boot_vcpus==max_vcpus,
	// no memory hotplug — the only things that need guest ACPI) and tear down by
	// killing the VMM, not the ACPI power button. Live A/B (real /run through
	// sandboxd, python-slim image): 1.93s→1.59s guest boot; still reaches a live agent
	// + running container. arm64 keeps ACPI (its boot/GIC path depends on it).
	//
	// The catch: with ACPI off, x86 can't read the MCFG table, so the guest only sees
	// PCI segment 0 (legacy config space) — a virtio-fs device on segment 1 becomes
	// invisible and the workload-rootfs mount fails EINVAL. So when acpiOff we put
	// virtio-fs on segment 0 alongside the disk (single-segment) instead of kata's
	// segment-1 default. The virtio-blk image disk is on segment 0 either way.
	acpiOff := runtime.GOARCH == "amd64"
	common := "panic=1 no_timer_check noreplace-smp console=" + console + ",115200n8"
	if acpiOff {
		common += " acpi=off"
	}

	fsSegment := int32(1) // kata default: virtio-fs on PCI segment 1 (needs ACPI MCFG)
	var platform *ch.PlatformConfig = &ch.PlatformConfig{NumPciSegments: 2}
	if acpiOff {
		fsSegment = 0    // no MCFG without ACPI → keep virtio-fs on segment 0
		platform = nil   // single segment; omit num_pci_segments
	}

	cfg := ch.VmConfig{
		Cpus:     ch.CpusConfig{BootVcpus: int32(vcpus), MaxVcpus: int32(vcpus)},
		Memory:   ch.MemoryConfig{Size: int64(memMiB) * 1024 * 1024, Shared: true},
		Fs:       []ch.FsConfig{{Tag: kata.FsTag, Socket: kata.VirtiofsdSocketPath(id), NumQueues: 1, QueueSize: 1024, PciSegment: fsSegment}},
		Platform: platform,
		Rng:      &ch.RngConfig{Src: "/dev/urandom"},
		Serial:   &ch.ConsoleConfig{Mode: "File", File: serialLog},
		Vsock:    &ch.VsockConfig{Cid: 3, Socket: kata.VsockSocketPath(id)},
	}

	if initrd != "" {
		// agent-init initrd: no root disk, no systemd unit; the initrd's /sbin/init IS
		// the kata-agent (runs on tmpfs, PID 1). It needs to be told it's init and given
		// the cgroup-v2 params kata's runtime normally injects, or it exits immediately
		// (guest powers off right after "Run /init"). agent.log=debug surfaces the exit
		// reason on the serial console.
		cmdline := common +
			" init=/sbin/init cgroup_no_v1=all systemd.unified_cgroup_hierarchy=1 agent.log=debug"
		if kparams != "" {
			cmdline += " " + kparams
		}
		cfg.Payload = ch.PayloadConfig{Kernel: kernel, Cmdline: cmdline, Initramfs: initrd}
		return cfg
	}

	// image disk: kata guest rootfs on /dev/vda1 (ext4), systemd -> kata-containers.target.
	cmdline := "root=/dev/vda1 rootflags=data=ordered,errors=remount-ro ro rootfstype=ext4 " +
		common + " systemd.unit=kata-containers.target systemd.mask=systemd-networkd.service systemd.mask=systemd-networkd.socket"
	if kparams != "" {
		cmdline += " " + kparams
	}
	cfg.Payload = ch.PayloadConfig{Kernel: kernel, Cmdline: cmdline}
	cfg.Disks = []ch.DiskConfig{{Path: image, Readonly: true, ImageType: "Raw", NumQueues: int32(vcpus), QueueSize: 1024}}
	return cfg
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
