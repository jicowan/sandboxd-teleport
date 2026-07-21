#!/bin/bash
set +e
RROOT=/run/containerd/runsc/k8s.io
PAUSE=22c18f6760a243616eaed85ced7e889ec90ea3023caf16e18b0759117ef993af
WORK=7d5543f65b56eb456d0977995dc63035b2119a48cacdc9dee308e3e2fa6b5504
PB=/run/containerd/io.containerd.runtime.v2.task/k8s.io/$PAUSE
WB=/run/containerd/io.containerd.runtime.v2.task/k8s.io/$WORK
S=/var/lib/cr2
NR=$S/nr
log(){ echo "[$(date -u +%T)] $*"; }

# cleanup prior
for C in $WORK $PAUSE; do runsc --root=$NR delete -force $C 2>/dev/null; done
pkill -f "root=$NR" 2>/dev/null; sleep 1
umount $S/pb/rootfs $S/wb/rootfs 2>/dev/null
rm -rf $S; mkdir -p $S/imgp $S/imgw $NR $S/pb/rootfs $S/wb/rootfs

# CHECKPOINT each container to own image (leave running)
runsc --root=$RROOT checkpoint --image-path=$S/imgp --leave-running $PAUSE >$S/ckp.log 2>&1; log "ckpt pause rc=$?"
runsc --root=$RROOT checkpoint --image-path=$S/imgw --leave-running $WORK  >$S/ckw.log 2>&1; log "ckpt work rc=$?"

# capture current live state for comparison
LIVE=$(runsc --root=$RROOT exec $WORK cat /state/log 2>/dev/null | tail -1)
log "live last log line: $LIVE"

# build restore bundles (real configs + bind live rootfs)
cp $PB/config.json $S/pb/config.json
cp $WB/config.json $S/wb/config.json
mount --bind $PB/rootfs $S/pb/rootfs
mount --bind $WB/rootfs $S/wb/rootfs

# RESTORE root-first, NO foreground pipes (-detach, redirect)
runsc --root=$NR restore -bundle $S/pb -image-path=$S/imgp -pid-file=$S/pb.pid -detach $PAUSE >$S/rp.log 2>&1
log "restore pause rc=$? state=$(runsc --root=$NR state $PAUSE 2>/dev/null | grep -o '"status": "[a-z]*"')"

runsc --root=$NR restore -bundle $S/wb -image-path=$S/imgw -pid-file=$S/wb.pid -detach $WORK >$S/rw.log 2>&1
log "restore work rc=$?"
sleep 3
log "work state=$(runsc --root=$NR state $WORK 2>/dev/null | grep -o '"status": "[a-z]*"')"
log "=== restore pause log ==="; cat $S/rp.log
log "=== restore work log ==="; cat $S/rw.log
log "=== restored /state/log tail ==="
runsc --root=$NR exec $WORK cat /state/log 2>&1 | tail -4
log "DONE"
