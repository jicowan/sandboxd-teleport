#!/bin/bash
set +e
S=/var/lib/teleport
ROOTA=$S/rootA
R="runsc -root $ROOTA --network=none -overlay2=root:self"
log(){ echo "[$(date -u +%T)] $*"; }
rm -rf $S/img; mkdir -p $S/img
log "checkpoint worker A"
$R exec wA cat /state/log 2>/dev/null | tail -1 | sed 's/^/  PRE-CKPT last line: /'
$R checkpoint --image-path=$S/img wA >$S/ckpt.log 2>&1
log "  checkpoint rc=$?  ($(ls $S/img | tr '\n' ' '))"
log "  worker A state after checkpoint: $($R state wA 2>/dev/null | grep -o '"status": "[a-z]*"')"
du -sh $S/img 2>&1 | sed 's/^/  img size: /'
