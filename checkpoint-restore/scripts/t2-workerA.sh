#!/bin/bash
set +e
S=/var/lib/teleport
ROOTA=$S/rootA
B=$S/bundle
log(){ echo "[$(date -u +%T)] $*"; }

# generate spec, patch to run the counter workload as root container
cd $B
runsc spec 2>/dev/null
python3 - "$B/config.json" <<'PY'
import json,sys
p=sys.argv[1]; c=json.load(open(p))
c["process"]["args"]=["/bin/sh","-c",
  'n=0; mkdir -p /state; echo boot >> /state/log; while true; do n=$((n+1)); echo "count=$n" >> /state/log; echo "ram=$n"; sleep 2; done']
c["process"]["terminal"]=False
c["root"]={"path":"rootfs","readonly":False}
json.dump(c,open(p,"w"),indent=2)
print("spec ready")
PY

R="runsc -root $ROOTA --network=none -overlay2=root:self"
$R delete -force wA 2>/dev/null; pkill -f "root=$ROOTA" 2>/dev/null; sleep 1
log "create+start worker A (standalone root container)"
$R create -bundle $B -pid-file $S/wA.pid wA >$S/cA.log 2>&1; log "  create rc=$?"
$R start wA >>$S/cA.log 2>&1; log "  start rc=$?  state=$($R state wA 2>/dev/null | grep -o '"status": "[a-z]*"')"
log "let it accumulate 12s..."
sleep 12
log "  worker A state file:"; $R exec wA cat /state/log 2>&1 | tail -3
