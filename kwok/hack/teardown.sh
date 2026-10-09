#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=kwok/hack/common.sh
source "$(dirname "$0")/common.sh"
# Leave controllers alive until normal drain and cloud cleanup have completed.
k -n oxide-kwok-test delete deployment inflate --ignore-not-found --wait --timeout="$TIMEOUT"
k delete nodepool oxide-kwok --ignore-not-found --wait --timeout="$TIMEOUT"
if [[ -n $(k get nodeclaims -l karpenter.sh/nodepool=oxide-kwok -o name) ]]; then
  k wait nodeclaims -l karpenter.sh/nodepool=oxide-kwok --for=delete --timeout="$TIMEOUT"
fi
if [[ -n $(k get nodes -l karpenter.sh/nodepool=oxide-kwok -o name) ]]; then
  k wait nodes -l karpenter.sh/nodepool=oxide-kwok --for=delete --timeout="$TIMEOUT"
fi
k delete oxidenodeclass oxide-kwok --ignore-not-found --wait --timeout="$TIMEOUT"
k delete namespace oxide-kwok-test --ignore-not-found --wait --timeout="$TIMEOUT"
printf 'Test Kubernetes resources removed. Run make audit to check cloud inventory before uninstalling.\n'
