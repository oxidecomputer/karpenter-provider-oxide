#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=kwok/hack/common.sh
source "$(dirname "$0")/common.sh"
: "${OXIDE_IMAGE_ID:?Set an existing test image UUID}"
: "${OXIDE_SUBNET_ID:?Set an existing test subnet UUID}"
# No image upload or cloud fixture ownership is guessed. Both resources are user-supplied.
for id in "$OXIDE_IMAGE_ID" "$OXIDE_SUBNET_ID"; do
  [[ "$id" =~ ^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$ ]] || { echo "Expected a UUID" >&2; exit 1; }
done
sed -e "s/REPLACE_IMAGE_UUID/$OXIDE_IMAGE_ID/" -e "s/REPLACE_SUBNET_UUID/$OXIDE_SUBNET_ID/" "$KWOK_DIR/examples/nodeclass.yaml" | k apply -f -
k wait oxidenodeclass/oxide-kwok --for=condition=Ready --timeout="$TIMEOUT"
k apply -f "$KWOK_DIR/examples/nodepool.yaml"
