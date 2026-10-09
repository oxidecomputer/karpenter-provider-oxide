#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=kwok/hack/common.sh
source "$(dirname "$0")/common.sh"
need kubectl
need helm
need jq
printf 'Target context: %s\n' "$KUBE_CONTEXT"
k version -o json | jq '{serverVersion:.serverVersion.gitVersion}'
k get namespace kube-system -o json | jq '{clusterUID:.metadata.uid}'
k -n kube-public get configmap cluster-info -o name
k -n kube-system get configmap kubeadm-config -o name
k auth can-i create nodes
k auth can-i create clusterroles
printf 'Bootstrap discovery exists. Oxide API connectivity is checked by the deployed provider and bridge.\n'
