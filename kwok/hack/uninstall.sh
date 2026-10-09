#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=kwok/hack/common.sh
source "$(dirname "$0")/common.sh"
"$KWOK_DIR/hack/audit.sh"
# The release includes the CRD chart. Do not uninstall CRDs while ANY resources use them.
for resource in nodeclaims nodepools oxidenodeclasses oxidebootdisks oxidecapacities nodeoverlays capacitybuffers; do
  if [[ -n $(k get "$resource" -o name) ]]; then
    echo "Refusing uninstall: $resource still exist. Complete cleanup first." >&2
    exit 1
  fi
done
h uninstall "$RELEASE" --wait --timeout "$TIMEOUT"
