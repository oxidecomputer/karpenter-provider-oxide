#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=kwok/hack/common.sh
source "$(dirname "$0")/common.sh"
need jq
job="$RELEASE-kwok-audit-$(date +%s)"
# Reuse the bridge image, credentials and read-only Kubernetes permissions in a short-lived Job.
k -n "$NAMESPACE" get deployment "$RELEASE-kwok-bridge" -o json | jq --arg name "$job" '{apiVersion:"batch/v1",kind:"Job",metadata:{name:$name},spec:{backoffLimit:0,activeDeadlineSeconds:150,ttlSecondsAfterFinished:600,template:{metadata:{},spec:(.spec.template.spec | .restartPolicy="Never" | .containers[0].args=["audit","--require-empty"] | del(.containers[0].readinessProbe,.containers[0].livenessProbe,.containers[0].ports))}}}' | k -n "$NAMESPACE" create -f -
result=0
k -n "$NAMESPACE" wait job/"$job" --for=condition=complete --timeout=180s || result=$?
k -n "$NAMESPACE" logs job/"$job" --all-containers || result=$?
if [[ $result == 0 ]]; then k -n "$NAMESPACE" delete job "$job"; fi
exit "$result"
