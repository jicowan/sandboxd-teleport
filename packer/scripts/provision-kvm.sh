#!/usr/bin/env bash
# provision-kvm.sh — enable KVM hardware-virtualization prerequisites on an
# EKS-optimized AL2023 node AMI, for running Cloud Hypervisor / Firecracker
# microVM sandboxes (see docs/sandboxd/PRD/PRD-microvm-runtime-cloud-hypervisor.md).
#
# This is the ONLY node-level customization: the VMM binaries + guest kernel +
# kata-agent rootfs ship in the sandboxd worker CONTAINER image, not here. We just
# make /dev/kvm present, permissioned, and loaded at every boot.
#
# Idempotent and boot-safe: config lands in /etc so it survives reboots and
# applies on the *.metal instance where /dev/kvm actually exists (the build
# instance may not expose it — that's fine; we only WRITE config here).
set -euxo pipefail

KVM_DEVICE_MODE="${KVM_DEVICE_MODE:-0666}"

echo "==> sandboxd microVM node provisioning (KVM enablement only)"

# 1) Load the KVM modules at every boot. AL2023 uses systemd; drop a modules-load.d
#    file so kvm + the vendor module load early. We include BOTH intel and amd
#    vendor modules — the kernel silently ignores the one that doesn't match the
#    CPU, so a single AMI works on both c5.metal (Intel) and c7a.metal (AMD).
cat >/etc/modules-load.d/kvm.conf <<'EOF'
# Loaded at boot for sandboxd microVM sandboxes (Cloud Hypervisor / Firecracker).
# The non-matching vendor module is ignored by the kernel; one AMI serves both.
kvm
kvm_intel
kvm_amd
EOF

# 1a) Nested KVM is NOT required for our use case (the workload runs directly in a
#     microVM on a bare-metal host — we are the first level of virtualization, not
#     nested). We intentionally do NOT set nested=1; leaving the vendor-module
#     default avoids the perf/stability caveats of nested virt.

# 2) Ensure /dev/kvm is group-accessible via udev. On a bare-metal instance the
#    kernel creates /dev/kvm when kvm_intel/kvm_amd loads; this rule fixes its
#    ownership/mode deterministically so a (non-root) process could open it. The
#    sandboxd worker runs privileged today, so this is belt-and-suspenders.
cat >/etc/udev/rules.d/65-kvm.rules <<EOF
# sandboxd: make /dev/kvm accessible for microVM VMMs.
KERNEL=="kvm", GROUP="kvm", MODE="${KVM_DEVICE_MODE}"
EOF

# Ensure a 'kvm' group exists for the udev rule (AL2023 may not ship one).
getent group kvm >/dev/null 2>&1 || groupadd --system kvm

# 3) A tiny oneshot unit that VALIDATES /dev/kvm at boot and logs loudly if it is
#    absent — so a misprovisioned (non-metal) node fails visibly in journalctl
#    rather than silently failing every microVM /run later. Non-fatal: we do not
#    want to block node join, only to make the missing prerequisite diagnosable.
cat >/etc/systemd/system/sandboxd-kvm-check.service <<'EOF'
[Unit]
Description=sandboxd: verify /dev/kvm is present for microVM sandboxes
After=systemd-modules-load.service
Wants=systemd-modules-load.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/sandboxd-kvm-check.sh

[Install]
WantedBy=multi-user.target
EOF

cat >/usr/local/bin/sandboxd-kvm-check.sh <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
if [[ -e /dev/kvm ]]; then
  echo "sandboxd-kvm-check: /dev/kvm present ($(stat -c '%A %G' /dev/kvm)); microVM sandboxes supported."
  exit 0
fi
echo "sandboxd-kvm-check: WARNING /dev/kvm ABSENT. This node cannot run microVM sandboxes." >&2
echo "sandboxd-kvm-check: /dev/kvm is only exposed on BARE-METAL (*.metal) EC2 instances." >&2
echo "sandboxd-kvm-check: check the instance type and that kvm_intel/kvm_amd loaded (lsmod | grep kvm)." >&2
exit 0  # non-fatal: do not block node join; the failure is logged for diagnosis.
EOF
chmod +x /usr/local/bin/sandboxd-kvm-check.sh

# 4) Enable (don't start — we're building an image) the check + modules-load so
#    they run on first boot of the real node.
systemctl enable sandboxd-kvm-check.service
systemctl enable systemd-modules-load.service || true

# 5) Best-effort attempt to load now, purely so the build log SHOWS whether the
#    build host had KVM (informational; a non-metal build host won't, and that is
#    expected and OK — the config above is what matters at runtime).
modprobe kvm 2>/dev/null || echo "==> (build host has no KVM module loadable — expected on non-metal build instances)"
if [[ -e /dev/kvm ]]; then
  echo "==> build host DOES expose /dev/kvm: $(stat -c '%A %G' /dev/kvm)"
else
  echo "==> build host does NOT expose /dev/kvm (expected on non-metal); runtime *.metal node will."
fi

echo "==> KVM enablement written. Node hygiene: cleaning cloud-init/build artifacts."
# Standard image hygiene so the baked AMI first-boots cleanly as a fresh node.
cloud-init clean --logs 2>/dev/null || true
rm -f /etc/ssh/ssh_host_* 2>/dev/null || true
rm -rf /var/lib/cloud/instances/* 2>/dev/null || true

echo "==> sandboxd microVM node provisioning complete."
