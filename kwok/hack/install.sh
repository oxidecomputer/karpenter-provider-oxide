#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=kwok/hack/common.sh
source "$(dirname "$0")/common.sh"
: "${VALUES:?Set VALUES to a configured values.yaml}"
if [[ -z ${CHART:-} ]]; then
  if [[ -d "$KWOK_DIR/../charts/karpenter-provider-oxide-kwok" ]]; then
    CHART="$KWOK_DIR/../charts/karpenter-provider-oxide-kwok"
  else
    charts=("$KWOK_DIR"/karpenter-provider-oxide-kwok-*.tgz)
    [[ ${#charts[@]} == 1 && -f ${charts[0]} ]] || { echo 'Set CHART to the packaged chart'; exit 1; }
    CHART=${charts[0]}
  fi
fi
"$KWOK_DIR/hack/preflight.sh"
h upgrade --install "$RELEASE" "$CHART" --create-namespace --values "$VALUES" --wait --timeout "$TIMEOUT" "$@"
