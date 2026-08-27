VERSION ?= $(shell git describe --tags --always | sed -e 's/^v//')

MY_IMAGE_REPO ?= ghcr.io/$(GITHUB_USER)/karpenter-provider-oxide
IMAGE_REPO ?= ghcr.io/oxidecomputer/karpenter-provider-oxide
HELM_OCI_REPO ?= ghcr.io/oxidecomputer/helm-charts
CONTROLLER_CHART_NAME ?= karpenter-provider-oxide
CRD_CHART_NAME ?= karpenter-provider-oxide-crds
CONTROLLER_CHART_DIR ?= charts/$(CONTROLLER_CHART_NAME)
CRD_CHART_DIR ?= charts/$(CRD_CHART_NAME)
IMAGE_TAG ?= dev
IMG ?= $(IMAGE_REPO):$(IMAGE_TAG)
KO_DOCKER_REPO ?= $(IMAGE_REPO)
KOCACHE ?= ~/.ko
MAKEFILE_PATH := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
TOOLS_MOD := $(MAKEFILE_PATH)/tools/go.mod
GO_TOOL := go tool -modfile=$(TOOLS_MOD)
KARPENTER_NAMESPACE ?= kube-system
HELM_VERSION ?= v4.2.4

LDFLAGS ?= -ldflags=-X=sigs.k8s.io/karpenter/pkg/operator.Version=$(VERSION)
GOFLAGS += $(LDFLAGS)
WITH_GOFLAGS = GOFLAGS="$(GOFLAGS)"

KARPENTER_CORE_DIR = $(shell go list -m -f '{{ .Dir }}' sigs.k8s.io/karpenter)

GOARCH ?= $(shell go env GOARCH)
BINARY_FILENAME = karpenter-oxide-$(GOARCH)

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

SHELL_SOURCES = $(shell git ls-files --cached --others --exclude-standard '*.sh')

.PHONY: all
all: precommit


.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: generate
generate: ## Generate deepcopy code and CRDs w/ controller-gen, vendor the karpenter.sh CRDs, and refresh chart docs
	go generate ./...
	$(CONTROLLER_GEN) object crd paths="./pkg/apis/..." \
		output:crd:artifacts:config=pkg/apis/crds
	cp $(KARPENTER_CORE_DIR)/pkg/apis/crds/* pkg/apis/crds
	cp pkg/apis/crds/* $(CRD_CHART_DIR)/templates
	$(HELM_DOCS) --chart-search-root charts

.PHONY: docs
docs: ## Build the single-page documentation preview.
	hack/docs.sh build

.PHONY: docs-serve
docs-serve: ## Serve the single-page documentation preview.
	hack/docs.sh serve

.PHONY: verify
verify: release-check lint-shell ## Run linters and vetting on codebase with no commit checks and no mutations.
	"$(GOLANGCI_LINT)" config verify
	$(GOLANGCI_LINT) run
	go vet ./...

.PHONY: lint-shell
lint-shell: ## Run shellcheck over the checked-in shell scripts.
	@command -v shellcheck >/dev/null 2>&1 || { \
		echo "WARNING: shellcheck is not installed, skipping shell lint"; \
		exit 0; \
	}; \
	shellcheck --severity=style $(SHELL_SOURCES)

.PHONY: fix
fix: golangci-lint ## Perform lint, fmt, and mod/sum file fixes
	go mod tidy
	go fmt ./...
	cd tools/ && go mod tidy
	$(GOLANGCI_LINT) run --fix

.PHONY: test
test: generate ## Run tests.
	go test -v ./... -coverprofile cover.out

.PHONY: coverage
coverage: ## Render the coverage profile written by `make test` as HTML
	go tool cover -html cover.out -o coverage.html

.PHONY: vulncheck
vulncheck: ## Verify code vulnerabilities
	$(GOVULNCHECK) ./pkg/...

.PHONY: licenses
licenses: ## Verifies dependency licenses
	! $(GO_LICENSES) csv ./... | grep -v -e 'MIT' -e 'Apache-2.0' -e 'BSD-3-Clause' -e 'BSD-2-Clause' -e 'ISC' -e 'MPL-2.0'

.PHONY: precommit
precommit: fix verify test release-snapshot ## Run all precommit checks (lint, fix, fmt, tests, local snapshot release).

.PHONY: update-karpenter
update-karpenter: ## Update kubernetes-sigs/karpenter to latest, then re-vendor its CRDs
	go get -u sigs.k8s.io/karpenter@HEAD
	go mod tidy
	$(MAKE) generate

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

export PATH := $(LOCALBIN):$(PATH)

## Tool Binaries
CONTROLLER_GEN ?= $(GO_TOOL) controller-gen
KUBECTL ?= $(LOCALBIN)/kubectl
KO ?= KOCACHE=$(KOCACHE) $(GO_TOOL) ko
GORELEASER ?= $(GO_TOOL) goreleaser
GOVULNCHECK ?= $(GO_TOOL) govulncheck
GO_LICENSES ?= $(GO_TOOL) go-licenses
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint-custom
HELM ?= $(LOCALBIN)/helm
HELM_DOCS ?= $(GO_TOOL) helm-docs

.PHONY: tools
tools: kubectl helm $(LOCALBIN) golangci-lint ## Install all required dev tools to the repo's local bin/
	GOBIN="$(LOCALBIN)" go install -modfile=$(TOOLS_MOD) tool

K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

.PHONY: kubectl
kubectl: $(LOCALBIN)
	@command -v $(KUBECTL) >/dev/null 2>&1 && exit 0; \
		v=$$(curl -fsSL "https://dl.k8s.io/release/stable-$(K8S_VERSION).txt"); \
		curl -fsSL -o "$(LOCALBIN)/kubectl" "https://dl.k8s.io/release/$$v/bin/$$(go env GOOS)/$$(go env GOARCH)/kubectl"; \
		chmod +x "$(LOCALBIN)/kubectl"

.PHONY: helm
helm: $(LOCALBIN)
	curl -fsSL -o $(LOCALBIN)/get_helm.sh https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4
	chmod 700 $(LOCALBIN)/get_helm.sh
	HELM_INSTALL_DIR=$(LOCALBIN) USE_SUDO=false $(LOCALBIN)/get_helm.sh --version $(HELM_VERSION)

.PHONY: golangci-lint
golangci-lint:
	@echo "Building custom golangci-lint with plugins..."
	go tool -modfile $(TOOLS_MOD) golangci-lint custom --destination $(LOCALBIN) --name golangci-lint-custom

IMAGE_REF_FILE := $(shell echo "$${TMPDIR:-/tmp}")/karpenter-oxide-image-ref

.PHONY: build
build: generate ## Builds a container image using ko and pushes to $KO_DOCKER_REPO
	$(WITH_GOFLAGS) KO_DOCKER_REPO="$(KO_DOCKER_REPO)" $(KO) build --bare ./cmd/controller | tee $(IMAGE_REF_FILE)

.PHONY: build-dev
build-dev: generate ## Builds a container image using ko and pushes to $MY_IMAGE_REPO
	$(WITH_GOFLAGS) KO_DOCKER_REPO="$(MY_IMAGE_REPO)" $(KO) build --bare ./cmd/controller | tee $(IMAGE_REF_FILE)

.PHONY: binary
binary: ## Build the Karpenter controller binary using go build
	go build $(GOFLAGS) -o $(BINARY_FILENAME) ./cmd/controller/...

.PHONY: run
run: ## Run the controller against your local cluster with the latest CRDs. Requires OXIDE_HOST, OXIDE_TOKEN and OXIDE_PROJECT.
	$(KUBECTL) apply -f ./pkg/apis/crds/
		OXIDE_HOST=${OXIDE_HOST} \
		OXIDE_TOKEN=${OXIDE_TOKEN} \
		OXIDE_PROJECT=${OXIDE_PROJECT} \
		SYSTEM_NAMESPACE=$(KARPENTER_NAMESPACE) \
		DISABLE_LEADER_ELECTION=true \
		FEATURE_GATES="NodeOverlay=true,StaticCapacity=true,NodeRepair=true" \
		OXIDE_INVENTORY_ENABLED="true" \
		LOG_LEVEL="debug" \
		go run ./cmd/controller/main.go

ARTIFACTS ?= $(PWD)/_artifacts

.PHONY: manifests
manifests: generate
	rm -rf $(ARTIFACTS)/helm
	mkdir -p $(ARTIFACTS)/helm
	$(HELM) package $(CONTROLLER_CHART_DIR) \
		--version $(VERSION) \
		--app-version $(VERSION) \
		--destination $(ARTIFACTS)/helm
	$(HELM) package $(CRD_CHART_DIR) \
		--version $(VERSION) \
		--app-version $(VERSION) \
		--destination $(ARTIFACTS)/helm
	test -f $(ARTIFACTS)/helm/$(CONTROLLER_CHART_NAME)-$(VERSION).tgz
	test -f $(ARTIFACTS)/helm/$(CRD_CHART_NAME)-$(VERSION).tgz

##@ Dev Deployment

## Extra helm options for the dev deployment targets
CLUSTER_ENDPOINT ?= $(shell $(KUBECTL) config view --minify -o jsonpath='{.clusters[].cluster.server}')
HELM_OPTS ?= --set settings.oxide.host=${OXIDE_HOST} \
			--set settings.oxide.project=${OXIDE_PROJECT} \
			--set controller.resources.requests.cpu=1 \
			--set controller.resources.requests.memory=1Gi \
			--set controller.resources.limits.cpu=1 \
			--set controller.resources.limits.memory=1Gi \
			--set settings.featureGates.nodeRepair=true \
			--set settings.featureGates.nodeOverlay=true \
			--set settings.featureGates.staticCapacity=true \
			--set bootstrap.kubeadm.enabled=true \
			--set settings.preferencePolicy=Ignore \
			--set logLevel=debug \
			--create-namespace

.PHONY: install-crds
install-crds: generate ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(HELM) upgrade --install karpenter-crds $(CRD_CHART_DIR) --take-ownership --force-conflicts

.PHONY: uninstall-crds
uninstall-crds: ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	$(HELM) uninstall karpenter-crds

.PHONY: deploy
deploy: generate ## Deploy the controller to the cluster in ~/.kube/config. Pulls a remote image, but uses the local helm chart.
	$(HELM) upgrade --install karpenter $(CONTROLLER_CHART_DIR) \
		--namespace $(KARPENTER_NAMESPACE) \
		$(HELM_OPTS) \
		--set controller.image.repository=$(IMAGE_REPO) \
		--set controller.image.tag=$(IMAGE_TAG) \
		--wait

.PHONY: helm-apply
helm-apply:
	REF="$$(cat $(IMAGE_REF_FILE))"; \
	$(HELM) upgrade --install karpenter $(CONTROLLER_CHART_DIR) \
		--namespace $(KARPENTER_NAMESPACE) \
		$(HELM_OPTS) \
		--set controller.image.repository="$${REF%:*}" \
		--set controller.image.tag="$${REF##*:}" \
		--wait

.PHONY: apply
apply: install-crds build-dev helm-apply ## Build/push the image with ko and deploy that exact digest to the current kube context.

.PHONY: undeploy
undeploy: ## Undeploy the controller from the K8s cluster specified in ~/.kube/config.
	$(HELM) uninstall karpenter --namespace $(KARPENTER_NAMESPACE) --wait

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

##@ CI and Release

.PHONY: ci-verify
ci-verify: precommit ## Verify fmt, linters, mod/sum files, generated CRDs, chart docs, etc. and fail if any mutate.
	@git diff --exit-code || { \
		echo; \
		echo "ERROR: working tree is dirty after 'make ci-verify'."; \
		echo "Run 'make fix generate' locally and commit the result."; \
		exit 1; \
	}


.PHONY: release-check
release-check: tools
	$(GORELEASER) check

.PHONY: release-snapshot
release-snapshot: tools ## Build the release artifacts locally without publishing.
	$(GORELEASER) release --snapshot --clean

.PHONY: release
release: tools ## Build and push the multi-arch (linux/amd64,linux/arm64) images to ghcr, push the helm charts, and cut a GitHub release.
	@git describe --tags --exact-match >/dev/null 2>&1 || { \
		echo "ERROR: 'make release' must run from an exact vX.Y.Z tag."; \
		exit 1; \
	}
	$(GORELEASER) release --clean
	for chart in $(ARTIFACTS)/helm/*.tgz; do \
		$(HELM) push "$$chart" "oci://$(HELM_OCI_REPO)" --debug; \
	done
