#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=kwok/hack/common.sh
source "$(dirname "$0")/common.sh"
: "${VERSION:?Set VERSION}"
: "${ARTIFACTS:=$KWOK_DIR/../_artifacts}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
bundle="$work/oxide-kwok-$VERSION"
mkdir -p "$bundle"
cp "$ARTIFACTS/helm/karpenter-provider-oxide-kwok-$VERSION.tgz" "$bundle/"
cp -R "$KWOK_DIR/hack" "$KWOK_DIR/examples" "$bundle/"
cp "$KWOK_DIR/README.md" "$bundle/README.md"
cp "$KWOK_DIR/versions.env" "$bundle/"
if [[ ${INCLUDE_IMAGES:-false} == true ]]; then
  : "${PROVIDER_IMAGE:?Set PROVIDER_IMAGE}"
  : "${BRIDGE_IMAGE:?Set BRIDGE_IMAGE}"
  KWOK_IMAGE=$(sed -n 's/^KWOK_IMAGE=//p' "$KWOK_DIR/versions.env")
  docker pull "$KWOK_IMAGE"
  docker tag "$KWOK_IMAGE" registry.k8s.io/kwok/kwok:v0.8.0
  docker image inspect "$PROVIDER_IMAGE" "$BRIDGE_IMAGE" registry.k8s.io/kwok/kwok:v0.8.0 \
    --format '{{.Id}} {{.Os}}/{{.Architecture}} {{json .RepoTags}}' > "$bundle/images.txt"
  docker save "$PROVIDER_IMAGE" "$BRIDGE_IMAGE" registry.k8s.io/kwok/kwok:v0.8.0 -o "$bundle/images.tar"
  cat > "$bundle/images-values.yaml" <<YAML
provider:
  controller:
    image:
      repository: ${PROVIDER_IMAGE%:*}
      tag: ${PROVIDER_IMAGE##*:}
kwok:
  # Docker archives contain the selected architecture, not the registry multiarch index.
  image: registry.k8s.io/kwok/kwok:v0.8.0
bridge:
  image:
    repository: ${BRIDGE_IMAGE%:*}
    tag: ${BRIDGE_IMAGE##*:}
YAML
fi
(cd "$bundle" && shasum -a 256 ./*.tgz > SHA256SUMS)
if [[ -f "$bundle/images.tar" ]]; then (cd "$bundle" && shasum -a 256 images.tar >> SHA256SUMS); fi
tar -czf "$ARTIFACTS/oxide-kwok-$VERSION.tar.gz" -C "$work" "oxide-kwok-$VERSION"
printf 'Handoff bundle: %s/oxide-kwok-%s.tar.gz\n' "$ARTIFACTS" "$VERSION"
