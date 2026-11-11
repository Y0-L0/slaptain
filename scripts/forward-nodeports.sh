#!/usr/bin/env bash
# Mirror every NodePort Service of a cluster on the same port on 127.0.0.1,
# for clusters whose nodes the e2e runner cannot reach directly.
# Runs until killed. Services created later are picked up within seconds,
# and a forward whose pod went away is restarted.
#
#   scripts/forward-nodeports.sh <context>
set -uo pipefail

context=$1
declare -A forwarded

forward() {
  local ns=$1 svc=$2 node=$3 port=$4
  while :; do
    kubectl --context "$context" -n "$ns" port-forward --address 127.0.0.1 "svc/$svc" "$node:$port"
    sleep 1
  done
}

while :; do
  while read -r ns svc node port; do
    [ -n "${forwarded[$node]:-}" ] && continue
    forwarded[$node]=1
    forward "$ns" "$svc" "$node" "$port" &
  done < <(kubectl --context "$context" get svc -A -o json | jq -r '
    .items[] | select(.spec.type == "NodePort") | . as $s
    | .spec.ports[] | "\($s.metadata.namespace) \($s.metadata.name) \(.nodePort) \(.port)"')
  sleep 3
done
