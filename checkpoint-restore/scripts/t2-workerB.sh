#!/bin/bash
set +e
S=/var/lib/teleport
ROOTB=$S/rootB
BB=$S/bundle-b
IMGB=$S/img-b
log(){ echo "[$(date -u +%T)] $*"; }

# fresh worker B: its own -root and its own bundle rootfs (separate busybox snapshot)
runsc -root $ROOTB delete -force wB 2>/dev/null; pkill -f "root=$ROOTB" 2>/dev/null; sleep 1
umount $BB/rootfs 2>/dev/null
ctr -n k8s.io task kill -s SIGKILL bb-srcB 2>/dev/null; sleep 1
ctr -n k8s.io containers rm bb-srcB 2>/dev/null
ctr -n k8s.io snapshots rm bb-srcB 2>/dev/null
rm -rf $ROOTB $BB; mkdir -p $ROOTB $BB/rootfs

log "prepare a SEPARATE busybox rootfs for worker B"
ctr -n k8s.io container create --snapshotter overlayfs docker.io/library/busybox:1.36 bb-srcB sleep 3600 >$S/createB.log 2>&1
log "  create rc=$?"
ctr -n k8s.io snapshots --snapshotter overlayfs mounts $BB/rootfs bb-srcB > $S/mntB.sh 2>&1
sh $S/mntB.sh 2>$S/mntB.err
log "  B rootfs mounted: $(mountpoint -q $BB/rootfs && echo yes || echo NO)"

# copy the SAME spec worker A used (must match for restore); A's bundle config is gone, regenerate identical
cd $BB; runsc spec 2>/dev/null
python3 - "$BB/config.json" <<'PY'
import json,sys
p=sys.argv[1]; c=json.load(open(p))
c["process"]["args"]=["/bin/sh","-c",
  'n=0; mkdir -p /state; echo boot >> /state/log; while true; do n=$((n+1)); echo "count=$n" >> /state/log; echo "ram=$n"; sleep 2; done']
c["process"]["terminal"]=False
c["root"]={"path":"rootfs","readonly":False}
json.dump(c,open(p,"w"),indent=2)
PY

R="runsc -root $ROOTB --network=none -overlay2=root:self"
log "RESTORE worker B from S3-downloaded checkpoint (create then restore)"
$R create -bundle $BB -pid-file $S/wB.pid wB >$S/crB.log 2>&1; log "  create rc=$?"
$R restore -bundle $BB -image-path=$IMGB -pid-file=$S/wB.pid -detach wB >$S/rB.log 2>&1
log "  restore rc=$?  $(cat $S/rB.log)"
sleep 3
log "  worker B state=$($R state wB 2>/dev/null | grep -o '"status": "[a-z]*"')"
log "  === RESTORED counter (expect continues from ~21, NOT reset to 1) ==="
$R exec wB cat /state/log 2>&1 | tail -5
sleep 3
log "  === 3s later (still advancing?) ==="
$R exec wB cat /state/log 2>&1 | tail -2
