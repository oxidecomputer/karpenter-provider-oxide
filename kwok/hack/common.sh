#!/usr/bin/env bash
# shellcheck shell=bash
set -euo pipefail
KWOK_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
: "${KUBE_CONTEXT:=kind-oxide-kwok}"
: "${NAMESPACE:=oxide-kwok}"
: "${RELEASE:=oxide-kwok}"
: "${KIND_NAME:=oxide-kwok}"
: "${TIMEOUT:=300s}"
export KWOK_DIR
export KUBE_CONTEXT NAMESPACE RELEASE KIND_NAME TIMEOUT
k() { kubectl --context "$KUBE_CONTEXT" --request-timeout=30s "$@"; }
h() { helm --kube-context "$KUBE_CONTEXT" --namespace "$NAMESPACE" "$@"; }
need() { command -v "$1" >/dev/null || { echo "Required command: $1" >&2; exit 1; }; }
