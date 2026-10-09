#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=kwok/hack/common.sh
source "$(dirname "$0")/common.sh"
: "${VERSION:?Set a SemVer release VERSION}"
: "${ARTIFACTS:=$KWOK_DIR/../_artifacts}"
: "${HELM:=helm}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/charts" "$ARTIFACTS/helm"
cp -R "$KWOK_DIR/../charts/karpenter-provider-oxide" "$KWOK_DIR/../charts/karpenter-provider-oxide-crds" "$KWOK_DIR/../charts/karpenter-provider-oxide-kwok" "$work/charts/"
# Set the nested production chart's appVersion to the same image version as the bridge.
# Dependency chart versions stay at the repository version; no second production chart is maintained.
"$HELM" package "$work/charts/karpenter-provider-oxide" --app-version "$VERSION" --destination "$work" >/dev/null
rm -rf "$work/charts/karpenter-provider-oxide"
tar -xzf "$work/karpenter-provider-oxide-0.0.1.tgz" -C "$work/charts"
"$HELM" dependency build --skip-refresh "$work/charts/karpenter-provider-oxide-kwok"
"$HELM" package "$work/charts/karpenter-provider-oxide-kwok" --version "$VERSION" --app-version "$VERSION" --destination "$ARTIFACTS/helm"
