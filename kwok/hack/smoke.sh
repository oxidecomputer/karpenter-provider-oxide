#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=kwok/hack/common.sh
source "$(dirname "$0")/common.sh"
need jq
k apply -f "$KWOK_DIR/examples/workload.yaml"
k -n oxide-kwok-test rollout status deployment/inflate --timeout="$TIMEOUT"
k wait nodeclaims -l karpenter.sh/nodepool=oxide-kwok --for=condition=Launched --timeout="$TIMEOUT"
k wait nodeclaims -l karpenter.sh/nodepool=oxide-kwok --for=condition=Registered --timeout="$TIMEOUT"
k wait nodeclaims -l karpenter.sh/nodepool=oxide-kwok --for=condition=Initialized --timeout="$TIMEOUT"
claims=$(k get nodeclaims -l karpenter.sh/nodepool=oxide-kwok -o json)
nodes=$(k get nodes -l karpenter.sh/nodepool=oxide-kwok -o json)
jq -en --argjson claims "$claims" --argjson nodes "$nodes" '
  ($claims.items | length) > 0 and
  all($claims.items[]; . as $c |
    [$nodes.items[] | select(.spec.providerID == $c.status.providerID)] as $matching |
    ($matching | length) == 1 and
    $matching[0].metadata.annotations["kwok.oxide.computer/claim-uid"] == $c.metadata.uid and
    $matching[0].status.capacity == $c.status.capacity and
    $matching[0].status.allocatable == $c.status.allocatable)' >/dev/null
# Restart the bridge and ensure claim-to-node identities are unchanged.
before=$(jq -c '[.items[] | {name:.metadata.name,uid:.metadata.uid}] | sort_by(.name)' <<<"$nodes")
k -n "$NAMESPACE" rollout restart deployment/"$RELEASE-kwok-bridge"
k -n "$NAMESPACE" rollout status deployment/"$RELEASE-kwok-bridge" --timeout="$TIMEOUT"
after=$(k get nodes -l karpenter.sh/nodepool=oxide-kwok -o json | jq -c '[.items[] | {name:.metadata.name,uid:.metadata.uid}] | sort_by(.name)')
[[ "$before" == "$after" ]]
printf 'Smoke passed: launched, registered, initialized, scheduled Pods and stable Node identities.\n'
