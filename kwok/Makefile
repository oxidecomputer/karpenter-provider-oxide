SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help
ROOT := $(abspath ..)
VERSION ?= 0.0.0-dev
IMAGE_TAG ?= $(VERSION)
ARCH ?= $(shell go env GOARCH)
KIND_NAME ?= oxide-kwok
KUBE_CONTEXT ?= kind-$(KIND_NAME)
NAMESPACE ?= oxide-kwok
RELEASE ?= oxide-kwok
KIND_IMAGE ?= kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5
HELM ?= helm
KO ?= $(ROOT)/bin/ko
PROVIDER_IMAGE ?= ko.local/karpenter-provider-oxide
BRIDGE_IMAGE ?= ko.local/karpenter-provider-oxide-kwok
ARTIFACTS ?= $(ROOT)/_artifacts
export KUBE_CONTEXT NAMESPACE RELEASE KIND_NAME

.PHONY: help cluster preflight fixtures install apply local-images package bundle bundle-local test test-api verify smoke scale-down diagnostics audit uninstall delete-cluster
help:
	@echo 'cluster / preflight / local-images / apply / install / fixtures / smoke / scale-down / audit / diagnostics / uninstall / delete-cluster'
	@echo 'package / bundle (published images) / bundle-local (includes local image archive) / test / test-api / verify'

cluster:
	@kind version | grep -q 'v0.32.' || { echo 'Install kind v0.32.x for the pinned node image'; exit 1; }
	kind create cluster --name '$(KIND_NAME)' --image '$(KIND_IMAGE)' --config examples/kind.yaml --wait 120s
	kubectl --context 'kind-$(KIND_NAME)' annotate namespace kube-system kwok.oxide.computer/created-by=kwok-make --overwrite

preflight:
	./hack/preflight.sh
fixtures:
	./hack/fixtures.sh
install:
	./hack/install.sh
local-images:
	cd '$(ROOT)' && KO_DOCKER_REPO='$(PROVIDER_IMAGE)' $(KO) build --bare --local --platform 'linux/$(ARCH)' --tags '$(IMAGE_TAG)' ./cmd/controller
	cd '$(ROOT)' && KO_DOCKER_REPO='$(BRIDGE_IMAGE)' $(KO) build --bare --local --platform 'linux/$(ARCH)' --tags '$(IMAGE_TAG)' ./kwok
apply: local-images
	@test '$(KUBE_CONTEXT)' = 'kind-$(KIND_NAME)' || { echo 'For existing clusters push images to a registry and use install'; exit 1; }
	kind load docker-image --name '$(KIND_NAME)' '$(PROVIDER_IMAGE):$(IMAGE_TAG)' '$(BRIDGE_IMAGE):$(IMAGE_TAG)'
	$(HELM) dependency build --skip-refresh '$(ROOT)/charts/karpenter-provider-oxide-kwok'
	./hack/install.sh --set provider.controller.image.repository='$(PROVIDER_IMAGE)' --set provider.controller.image.tag='$(IMAGE_TAG)' --set bridge.image.repository='$(BRIDGE_IMAGE)' --set bridge.image.tag='$(IMAGE_TAG)'
package:
	VERSION='$(VERSION)' ARTIFACTS='$(ARTIFACTS)' HELM='$(HELM)' ./hack/package.sh
bundle: package
	VERSION='$(VERSION)' ARTIFACTS='$(ARTIFACTS)' ./hack/bundle.sh
bundle-local: local-images package
	VERSION='$(VERSION)' ARTIFACTS='$(ARTIFACTS)' INCLUDE_IMAGES=true PROVIDER_IMAGE='$(PROVIDER_IMAGE):$(IMAGE_TAG)' BRIDGE_IMAGE='$(BRIDGE_IMAGE):$(IMAGE_TAG)' ./hack/bundle.sh

test:
	cd '$(ROOT)' && go test ./kwok/...
test-api:
	@test -n "$${KUBEBUILDER_ASSETS:-}" || { echo 'Set KUBEBUILDER_ASSETS to local envtest binaries'; exit 1; }
	cd '$(ROOT)' && go test ./kwok/controllers/node -run TestAPIServerRegistration -count=1
verify: test
	$(HELM) dependency build --skip-refresh '$(ROOT)/charts/karpenter-provider-oxide-kwok'
	$(HELM) lint '$(ROOT)/charts/karpenter-provider-oxide-kwok' --set provider.settings.oxide.host=http://nexus.invalid --set provider.settings.oxide.project=test
	shellcheck hack/*.sh
smoke:
	./hack/smoke.sh
scale-down:
	./hack/teardown.sh
audit:
	./hack/audit.sh

diagnostics:
	./hack/diagnostics.sh
uninstall:
	./hack/uninstall.sh
delete-cluster: audit
	@test '$(KUBE_CONTEXT)' = 'kind-$(KIND_NAME)' || { echo 'Refusing to delete an existing-context cluster'; exit 1; }
	@test "$$(kubectl --context '$(KUBE_CONTEXT)' get namespace kube-system -o jsonpath='{.metadata.annotations.kwok\.oxide\.computer/created-by}')" = kwok-make
	@test -z "$$(kubectl --context '$(KUBE_CONTEXT)' get nodeclaims -o name)"
	kind delete cluster --name '$(KIND_NAME)'
