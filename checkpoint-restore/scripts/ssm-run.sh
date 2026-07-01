#!/bin/bash
# ssm-run.sh <local-script-file> : run a script on the gvisor node via SSM, print output
IID=i-000d00ffa9964e5b3
REGION=us-west-2
SCRIPT_FILE=$1
# encode script; run via bash -c to preserve it exactly
B64=$(base64 < "$SCRIPT_FILE" | tr -d '\n')
CMD="echo $B64 | base64 -d > /var/lib/ssm-job.sh; bash /var/lib/ssm-job.sh 2>&1"
CID=$(aws ssm send-command --region $REGION --instance-ids $IID \
  --document-name "AWS-RunShellScript" \
  --parameters "commands=[\"$CMD\"]" \
  --query 'Command.CommandId' --output text 2>&1)
echo "command-id=$CID"
# poll
for i in $(seq 1 60); do
  ST=$(aws ssm get-command-invocation --region $REGION --command-id "$CID" --instance-id $IID --query 'Status' --output text 2>/dev/null)
  [ "$ST" = "Success" -o "$ST" = "Failed" -o "$ST" = "TimedOut" -o "$ST" = "Cancelled" ] && break
  sleep 3
done
echo "status=$ST"
echo "----- STDOUT -----"
aws ssm get-command-invocation --region $REGION --command-id "$CID" --instance-id $IID --query 'StandardOutputContent' --output text 2>/dev/null
echo "----- STDERR -----"
aws ssm get-command-invocation --region $REGION --command-id "$CID" --instance-id $IID --query 'StandardErrorContent' --output text 2>/dev/null
