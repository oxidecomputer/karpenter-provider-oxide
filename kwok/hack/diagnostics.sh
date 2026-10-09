#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=kwok/hack/common.sh
source "$(dirname "$0")/common.sh"
: "${OUTPUT:=$KWOK_DIR/../_artifacts/kwok-diagnostics}"
mkdir -p "$OUTPUT"
k version -o yaml > "$OUTPUT/versions.yaml"
k get nodepools,nodeclaims,oxidenodeclasses,nodes -o wide > "$OUTPUT/resources.txt"
k get events -A --sort-by=.metadata.creationTimestamp > "$OUTPUT/events.txt"
# Deliberately exclude Secret bodies, cloud user-data and unrestricted object annotations.
k get nodeclaims -o json | jq '[.items[] | {name:.metadata.name,uid:.metadata.uid,providerID:.status.providerID,conditions:.status.conditions}]' > "$OUTPUT/claims.json"
k get nodes -o json | jq '[.items[] | {name:.metadata.name,providerID:.spec.providerID,conditions:.status.conditions,backend:.metadata.labels["kwok.oxide.computer/backend-state"]}]' > "$OUTPUT/nodes.json"
k -n "$NAMESPACE" get configmap "$RELEASE-kwok" -o jsonpath='{.data.kwok\.yaml}' > "$OUTPUT/stages.yaml"
printf 'Diagnostics: %s\nController logs can contain endpoint-specific data; review before sharing.\n' "$OUTPUT"
