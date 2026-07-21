#!/bin/bash
set +e
S=/var/lib/teleport
ROOTA=$S/rootA
log(){ echo "[$(date -u +%T)] $*"; }
umount $S/bundle/rootfs 2>/dev/null
ctr -n k8s.io task kill -s SIGKILL bb-src 2>/dev/null; sleep 1
ctr -n k8s.io containers rm bb-src 2>/dev/null
ctr -n k8s.io snapshots rm teleport-bb 2>/dev/null
rm -rf $ROOTA $S/bundle; mkdir -p $ROOTA $S/bundle/rootfs

log "create a containerd container (forces snapshot prepare via runc) to get busybox rootfs"
ctr -n k8s.io container create --snapshotter overlayfs docker.io/library/busybox:1.36 bb-src sleep 3600 >$S/create.log 2>&1
log "  create rc=$? ($(cat $S/create.log))"
# now bb-src has a snapshot; get its mounts
ctr -n k8s.io snapshots --snapshotter overlayfs mounts $S/bundle/rootfs bb-src > $S/mnt.sh 2>$S/mnt.err
log "  mounts cmd head: $(head -c 70 $S/mnt.sh)"
sh $S/mnt.sh 2>$S/mnt2.err
log "  rootfs mounted: $(mountpoint -q $S/bundle/rootfs && echo yes || echo NO)  err=$(cat $S/mnt2.err $S/mnt.err 2>/dev/null | head -1)"
ls $S/bundle/rootfs/bin/busybox >/dev/null 2>&1 && log "  busybox present ✓" || log "  busybox MISSING"
