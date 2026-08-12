// PROVENANCE: ported from Agent Substrate's cmd/ateom-microvm/internal/ch/createvm.go
// (Apache-2.0, Copyright 2026 Google LLC). See api.go for the full note.
//
// Licensed under the Apache License, Version 2.0.

package ch

import (
	"context"
	"fmt"
)

// VmConfig is the body of /api/v1/vm.create — the subset of cloud-hypervisor's
// VmConfig needed to boot a microVM guest. Modeled on kata's clh driver.
// vm.create + vm.boot are issued with PUT.
type VmConfig struct {
	Cpus     CpusConfig      `json:"cpus"`
	Memory   MemoryConfig    `json:"memory"`
	Payload  PayloadConfig   `json:"payload"`
	Disks    []DiskConfig    `json:"disks,omitempty"`
	Fs       []FsConfig      `json:"fs,omitempty"`
	Rng      *RngConfig      `json:"rng,omitempty"`
	Serial   *ConsoleConfig  `json:"serial,omitempty"`
	Console  *ConsoleConfig  `json:"console,omitempty"`
	Vsock    *VsockConfig    `json:"vsock,omitempty"`
	Platform *PlatformConfig `json:"platform,omitempty"`
}

// FsConfig is a virtio-fs device backed by a vhost-user (virtiofsd) socket. The
// sandbox rootfs is served over this as the guest's root (virtiofs-boot proven on
// the nested-virt node); the guest mounts it via the Tag.
type FsConfig struct {
	Tag        string `json:"tag"`
	Socket     string `json:"socket"`
	NumQueues  int32  `json:"num_queues,omitempty"`
	QueueSize  int32  `json:"queue_size,omitempty"`
	PciSegment int32  `json:"pci_segment,omitempty"`
}

// PlatformConfig sets VM-wide platform options. NumPciSegments must be >1 when a
// virtio-fs device sits on a non-zero PCI segment (kata puts fs on segment 1).
type PlatformConfig struct {
	NumPciSegments int32 `json:"num_pci_segments,omitempty"`
}

// CpusConfig sets the boot/max vCPU counts.
type CpusConfig struct {
	BootVcpus int32 `json:"boot_vcpus"`
	MaxVcpus  int32 `json:"max_vcpus"`
}

// MemoryConfig sets guest RAM. Shared=true makes CH back RAM with a memfd, which
// is what lets vm.snapshot write a SPARSE image (the memory-only snapshot the
// checkpoint/restore path relies on).
type MemoryConfig struct {
	Size   int64 `json:"size"`
	Shared bool  `json:"shared"`
}

// PayloadConfig points at the guest kernel + its cmdline.
type PayloadConfig struct {
	Kernel  string `json:"kernel"`
	Cmdline string `json:"cmdline"`
}

// DiskConfig is one virtio-blk disk (e.g. the guest image). The sandbox rootfs is
// served over virtio-fs, not a disk.
type DiskConfig struct {
	Path      string `json:"path"`
	Readonly  bool   `json:"readonly"`
	Direct    bool   `json:"direct"`
	NumQueues int32  `json:"num_queues,omitempty"`
	QueueSize int32  `json:"queue_size,omitempty"`
	ImageType string `json:"image_type,omitempty"`
}

// RngConfig sets the entropy source (e.g. /dev/urandom).
type RngConfig struct {
	Src string `json:"src"`
}

// ConsoleConfig is a serial/console device. Mode "Off" disables it; "File" with
// File set captures the guest console (for boot debugging); "Tty" to a pty.
type ConsoleConfig struct {
	Mode string `json:"mode"`
	File string `json:"file,omitempty"`
}

// VsockConfig is the hybrid-vsock the kata-agent listens on. Cid is the guest CID
// (kata uses 3); Socket is the host unix socket the driver then dials to drive the
// agent.
type VsockConfig struct {
	Cid    int64  `json:"cid"`
	Socket string `json:"socket"`
}

// CreateVM creates (but does not boot) the VM from cfg via /api/v1/vm.create.
// The VMM must already be up (LaunchVMM). After this the VM is in "Created".
func (c *Client) CreateVM(ctx context.Context, cfg VmConfig) error {
	if err := c.api.put(ctx, "/api/v1/vm.create", cfg); err != nil {
		return fmt.Errorf("vm.create: %w", err)
	}
	return nil
}

// BootVM boots a created VM via /api/v1/vm.boot (transitions Created -> Running).
func (c *Client) BootVM(ctx context.Context) error {
	if err := c.api.put(ctx, "/api/v1/vm.boot", nil); err != nil {
		return fmt.Errorf("vm.boot: %w", err)
	}
	return nil
}
