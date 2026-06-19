CLUSTER_NAME ?= $(shell kubectl config view --minify -o jsonpath='{.clusters[].name}' | rev | cut -d"/" -f1 | rev | cut -d"." -f1)

## Inject the app version into operator.Version
LDFLAGS ?= -ldflags=-X=sigs.k8s.io/karpenter/pkg/operator.Version=$(shell git describe --tags --always | cut -d"v" -f2)

GOFLAGS += $(LDFLAGS)
WITH_GOFLAGS = GOFLAGS="$(GOFLAGS)"

## Extra helm options
CLUSTER_ENDPOINT ?= $(shell kubectl config view --minify -o jsonpath='{.clusters[].cluster.server}')
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

# CR for local builds of Karpenter
KARPENTER_NAMESPACE ?= kube-system
KARPENTER_VERSION ?= $(shell git tag --sort=committerdate | tail -1 | cut -d"v" -f2)
KO_DOCKER_REPO ?= ${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_DEFAULT_REGION}.amazonaws.com/dev
KOCACHE ?= ~/.ko

# Common Directories
MOD_DIRS = $(shell find . -path "./website" -prune -o -name go.mod -type f -print | xargs dirname)
KARPENTER_CORE_DIR = $(shell go list -m -f '{{ .Dir }}' sigs.k8s.io/karpenter)
TMPFILE := $(shell mktemp)

# Filename when building the binary controller only
GOARCH ?= $(shell go env GOARCH)
BINARY_FILENAME = karpenter-oxide-$(GOARCH)

help: ## Display help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

presubmit: verify ## Run all steps in the developer loop

run: ## Run Karpenter controller binary against your local cluster with latest CRD's. Requires OXIDE_HOST, OXIDE_TOKEN and OXIDE_PROJECT.
	kubectl apply -f ./pkg/apis/crds/
	OXIDE_HOST=${OXIDE_HOST} \
		OXIDE_TOKEN=${OXIDE_TOKEN} \
		OXIDE_PROJECT=${OXIDE_PROJECT} \
		SYSTEM_NAMESPACE=${KARPENTER_NAMESPACE} \
		KUBERNETES_MIN_VERSION="1.19.0-0" \
		DISABLE_LEADER_ELECTION=true \
		FEATURE_GATES="NodeOverlay=true,StaticCapacity=true" \
		LOG_LEVEL="debug" \
		go run ./cmd/controller/main.go

coverage:
	go tool cover -html coverage.out -o coverage.html

verify: ## Verify code. Includes dependencies, linting, formatting, etc
	go mod tidy
	go mod download
	go generate ./...
	cp  $(KARPENTER_CORE_DIR)/pkg/apis/crds/* pkg/apis/crds
	cp pkg/apis/crds/* charts/karpenter-crd/templates
	go tool golangci-lint run
	@git diff --quiet ||\
		{ echo "New file modification detected in the Git working tree. Please check in before commit."; git --no-pager diff --name-only | uniq | awk '{print "  - " $$0}'; \
		if [ "${CI}" = true ]; then\
			exit 1;\
		fi;}

vulncheck: ## Verify code vulnerabilities
	@go tool govulncheck ./pkg/...

licenses: download ## Verifies dependency licenses
	# TODO: remove nodeadm check once license is updated
	! go tool go-licenses csv ./... | grep -v -e 'MIT' -e 'Apache-2.0' -e 'BSD-3-Clause' -e 'BSD-2-Clause' -e 'ISC' -e 'MPL-2.0'

image: ## Build the Karpenter controller images using ko build
	$(eval CONTROLLER_IMG=$(shell $(WITH_GOFLAGS) KOCACHE=$(KOCACHE) KO_DOCKER_REPO="$(KO_DOCKER_REPO)" ko build --bare github.com/aws/karpenter-provider-aws/cmd/controller))
	$(eval IMG_REPOSITORY=$(shell echo $(CONTROLLER_IMG) | cut -d "@" -f 1 | cut -d ":" -f 1))
	$(eval IMG_TAG=$(shell echo $(CONTROLLER_IMG) | cut -d "@" -f 1 | cut -d ":" -f 2 -s))
	$(eval IMG_DIGEST=$(shell echo $(CONTROLLER_IMG) | cut -d "@" -f 2))

binary: ## Build the Karpenter controller binary using go build
	go build $(GOFLAGS) -o $(BINARY_FILENAME) ./cmd/controller/...

apply: verify image ## Deploy the controller from the current state of your git repository into your ~/.kube/config cluster
	kubectl apply -f ./pkg/apis/crds/
	helm upgrade --install karpenter charts/karpenter --namespace ${KARPENTER_NAMESPACE} \
        $(HELM_OPTS) \
        --set controller.image.repository=$(IMG_REPOSITORY) \
        --set controller.image.tag=$(IMG_TAG) \
        --set controller.image.digest=$(IMG_DIGEST)

delete: ## Delete the controller from your ~/.kube/config cluster
	helm uninstall karpenter --namespace ${KARPENTER_NAMESPACE}

update-karpenter: ## Update kubernetes-sigs/karpenter to latest
	go get -u sigs.k8s.io/karpenter@HEAD
	go mod tidy

.PHONY: help presubmit run coverage verify vulncheck licenses image apply delete update-karpenter

define newline


endef
