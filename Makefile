# Use go.mod go version as a single source of truth of GO version.
GO_VERSION := $(shell awk '/^go /{print $$2}' go.mod|head -n1)

# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.34.0

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

GO_CMD ?= go
GO_FMT ?= gofmt
# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

GIT_TAG ?= $(shell git describe --tags --dirty --always)
# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
# Image URL to use all building/pushing image targets
DOCKER_BUILDX_CMD ?= docker buildx
IMAGE_BUILD_CMD ?= $(DOCKER_BUILDX_CMD) build
IMAGE_BUILD_EXTRA_OPTS ?=
STAGING_IMAGE_REGISTRY := us-central1-docker.pkg.dev/k8s-staging-images
IMAGE_REGISTRY ?= ${STAGING_IMAGE_REGISTRY}/lws
IMAGE_NAME := lws
IMAGE_REPO := $(IMAGE_REGISTRY)/$(IMAGE_NAME)
IMG ?= $(IMAGE_REPO):$(GIT_TAG)
# Output type of docker buildx build
OUTPUT_TYPE ?= image
HELM_CHART_REPO := ${STAGING_IMAGE_REGISTRY}/lws/charts
# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
BASE_IMAGE ?= gcr.io/distroless/static:nonroot
BUILDER_IMAGE ?= golang:$(GO_VERSION)
CGO_ENABLED ?= 0

ifdef EXTRA_TAG
IMAGE_EXTRA_TAG ?= $(IMAGE_REPO):$(EXTRA_TAG)
endif
ifdef IMAGE_EXTRA_TAG
IMAGE_BUILD_EXTRA_OPTS += -t $(IMAGE_EXTRA_TAG)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

version_pkg = sigs.k8s.io/lws/pkg/version
LD_FLAGS += -X '$(version_pkg).GitVersion=$(GIT_TAG)'
LD_FLAGS += -X '$(version_pkg).GitCommit=$(shell git rev-parse HEAD)'
LD_FLAGS += -X '$(version_pkg).BuildDate=$(BUILD_DATE)'

PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))
ARTIFACTS ?= $(PROJECT_DIR)/bin

INTEGRATION_TARGET ?= ./test/integration/...

E2E_KIND_VERSION ?= kindest/node:v1.34.0
CERT_MANAGER_VERSION ?= v1.17.0
USE_EXISTING_CLUSTER ?= false

# For local testing, we should allow user to use different kind cluster name
# Default will delete default kind cluster
KIND_CLUSTER_NAME ?= kind

# Setting SED allows macos users to install GNU sed and use the latter
# instead of the default BSD sed.
ifeq ($(shell command -v gsed 2>/dev/null),)
    SED ?= $(shell command -v sed)
else
    SED ?= $(shell command -v gsed)
endif
ifeq ($(shell ${SED} --version 2>&1 | grep -q GNU; echo $$?),1)
    $(error !!! GNU sed is required. If on OS X, use 'brew install gnu-sed'.)
endif

# Update these variables when preparing a new release or a release branch.
# Then run `make prepare-release-branch`
RELEASE_VERSION=v0.7.0
RELEASE_BRANCH=main
# Version used form Helm which is not using the leading "v"
CHART_VERSION := $(shell echo $(RELEASE_VERSION) | cut -c2-)

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

include Makefile-deps.mk

##@ Development
.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) \
		rbac:roleName=manager-role output:rbac:artifacts:config=config/rbac \
		crd:generateEmbeddedObjectMeta=true output:crd:artifacts:config=config/crd/bases \
		webhook output:webhook:artifacts:config=config/webhook \
		paths="{./api/..., ./pkg/...}"

.PHONY: generate
generate: controller-gen code-generator generate-apiref crds ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations and client-go libraries.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."
	./hack/update-codegen.sh $(GO_CMD) $(PROJECT_DIR)/bin

.PHONY: fmt
fmt: ## Run go fmt against code.
	$(GO_CMD) fmt ./...

.PHONY: fmt-verify
fmt-verify:
	@out=`$(GO_FMT) -w -l -d $$(find . -name '*.go' | grep -v /vendor/)`; \
	if [ -n "$$out" ]; then \
	    echo "$$out"; \
	    exit 1; \
	fi

.PHONY: gomod-verify
gomod-verify:
	$(GO_CMD) mod tidy
	git --no-pager diff --exit-code go.mod go.sum

.PHONY: vet
vet: ## Run go vet against code.
	$(GO_CMD) vet ./...

.PHONY: test
test: manifests fmt vet envtest gotestsum ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
	$(GOTESTSUM) --junitfile $(ARTIFACTS)/junit.xml -- ./api/... ./pkg/... ./cmd/... -coverprofile  $(ARTIFACTS)/cover.out

KIND = $(shell pwd)/bin/kind
.PHONY: kind
kind:
	@GOBIN=$(PROJECT_DIR)/bin GO111MODULE=on $(GO_CMD) install sigs.k8s.io/kind@v0.27.0

.PHONY: kind-image-build
kind-image-build: PLATFORMS=linux/amd64
kind-image-build: IMAGE_BUILD_EXTRA_OPTS=--load
kind-image-build: kind image-build

.PHONY: test-integration
test-integration: manifests fmt vet envtest ginkgo ## Run integration tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
	$(GINKGO) --junit-report=junit.xml --output-dir=$(ARTIFACTS) -v $(INTEGRATION_TARGET)

.PHONY: test-e2e
test-e2e: kustomize manifests fmt vet envtest ginkgo kind-image-build
	E2E_KIND_VERSION=$(E2E_KIND_VERSION) KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) KIND=$(KIND) KUBECTL=$(KUBECTL) KUSTOMIZE=$(KUSTOMIZE) GINKGO=$(GINKGO) USE_EXISTING_CLUSTER=$(USE_EXISTING_CLUSTER) IMAGE_TAG=$(IMG) ARTIFACTS=$(ARTIFACTS) ./hack/e2e-test.sh

.PHONY: test-e2e-cert-manager
test-e2e-cert-manager: kustomize manifests fmt vet envtest ginkgo kind-image-build
	USE_CERT_MANAGER=true CERT_MANAGER_VERSION=$(CERT_MANAGER_VERSION) E2E_KIND_VERSION=$(E2E_KIND_VERSION) KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) KIND=$(KIND) KUBECTL=$(KUBECTL) KUSTOMIZE=$(KUSTOMIZE) GINKGO=$(GINKGO) USE_EXISTING_CLUSTER=$(USE_EXISTING_CLUSTER) IMAGE_TAG=$(IMG) ARTIFACTS=$(ARTIFACTS) ./hack/e2e-test.sh

# Gang scheduling E2E tests with different schedulers
VOLCANO_VERSION ?= v1.12.1

.PHONY: test-e2e-gang-scheduling
test-e2e-gang-scheduling: test-e2e-gang-scheduling-volcano ## Run all gang scheduling E2E tests

.PHONY: test-e2e-gang-scheduling-volcano
test-e2e-gang-scheduling-volcano: kustomize manifests fmt vet envtest ginkgo kind-image-build ## Run gang scheduling E2E tests with Volcano
	SCHEDULER_PROVIDER=volcano VOLCANO_VERSION=$(VOLCANO_VERSION) E2E_KIND_VERSION=$(E2E_KIND_VERSION) KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) KIND=$(KIND) KUBECTL=$(KUBECTL) KUSTOMIZE=$(KUSTOMIZE) GINKGO=$(GINKGO) USE_EXISTING_CLUSTER=$(USE_EXISTING_CLUSTER) IMAGE_TAG=$(IMG) ARTIFACTS=$(ARTIFACTS) ./hack/e2e-test.sh

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter & yamllint
	$(GOLANGCI_LINT) run --timeout 15m0s

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

PATHS_TO_VERIFY := config/components api client-go site/ charts/
.PHONY: verify
verify: gomod-verify lint fmt-verify toc-verify manifests generate prepare-release-branch
	git --no-pager diff --exit-code $(PATHS_TO_VERIFY)
	if git ls-files --exclude-standard --others $(PATHS_TO_VERIFY) | grep -q . ; then exit 1; fi

##@ Build
.PHONY: build
build: manifests fmt vet ## Build manager binary.
	$(GO_BUILD_ENV) $(GO_CMD) build -ldflags="$(LD_FLAGS)" -o bin/manager cmd/main.go

.PHONY: run
run: manifests fmt vet ## Run a controller from your host.
	$(GO_CMD) run ./cmd/main.go

.PHONY: image-build
image-build:
	$(IMAGE_BUILD_CMD) -t $(IMG) \
		--platform=$(PLATFORMS) \
		--build-arg BASE_IMAGE=$(BASE_IMAGE) \
		--build-arg BUILDER_IMAGE=$(BUILDER_IMAGE) \
		--build-arg CGO_ENABLED=$(CGO_ENABLED) \
		--output=type=$(OUTPUT_TYPE) \
		$(PUSH) \
		$(IMAGE_BUILD_EXTRA_OPTS) ./

.PHONY: image-push
image-push: PUSH=--push
image-push: image-build

.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	- $(CONTAINER_TOOL) buildx create --name project-v3-builder --use
	- $(MAKE) image-build PUSH=$(PUSH)
	- $(CONTAINER_TOOL) buildx rm project-v3-builder

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

clean-manifests = (cd config/manager && $(KUSTOMIZE) edit set image controller=us-central1-docker.pkg.dev/k8s-staging-images/lws/lws:$(RELEASE_BRANCH))

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) create -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply --server-side --force-conflicts -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: helm-chart-push
helm-chart-push: yq helm crds
	EXTRA_TAG="$(EXTRA_TAG)" GIT_TAG="$(GIT_TAG)" IMAGE_REGISTRY="$(IMAGE_REGISTRY)" HELM_CHART_REPO="$(HELM_CHART_REPO)" IMAGE_REPO="$(IMAGE_REPO)" HELM="$(HELM)" YQ="$(YQ)" ./hack/push-chart.sh

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest

## Tool Versions
KUSTOMIZE_VERSION ?= v5.2.1
CONTROLLER_TOOLS_VERSION ?= v0.16.2
HELM_VERSION ?= v3.17.1
# Use go.mod go version as a single source of truth of Ginkgo version.
GINKGO_VERSION ?= $(shell go list -m -f '{{.Version}}' github.com/onsi/ginkgo/v2)

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary. If wrong version is installed, it will be removed before downloading.
$(KUSTOMIZE): $(LOCALBIN)
	@if test -x $(LOCALBIN)/kustomize && ! $(LOCALBIN)/kustomize version | grep -q $(KUSTOMIZE_VERSION); then \
		echo "$(LOCALBIN)/kustomize version is not expected $(KUSTOMIZE_VERSION). Removing it before installing."; \
		rm -rf $(LOCALBIN)/kustomize; \
	fi
	test -s $(LOCALBIN)/kustomize || GOBIN=$(LOCALBIN) GO111MODULE=on $(GO_CMD) install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary. If wrong version is installed, it will be overwritten.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(LOCALBIN)/controller-gen && $(LOCALBIN)/controller-gen --version | grep -q $(CONTROLLER_TOOLS_VERSION) || \
	GOBIN=$(LOCALBIN) $(GO_CMD) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	test -s $(LOCALBIN)/setup-envtest || GOBIN=$(LOCALBIN) $(GO_CMD) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

GINKGO = $(shell pwd)/bin/ginkgo
.PHONY: ginkgo
ginkgo: ## Download ginkgo locally if necessary.
	test -s $(LOCALBIN)/ginkgo || \
	GOBIN=$(LOCALBIN) $(GO_CMD) install github.com/onsi/ginkgo/v2/ginkgo@$(GINKGO_VERSION)

GOTESTSUM = $(shell pwd)/bin/gotestsum
.PHONY: gotestsum
gotestsum: ## Download gotestsum locally if necessary.
	test -s $(LOCALBIN)/gotestsum || \
	GOBIN=$(LOCALBIN) $(GO_CMD) install gotest.tools/gotestsum@v1.8.2

# Use same code-generator version as k8s.io/api
CODEGEN_VERSION := $(shell $(GO_CMD) list -m -f '{{.Version}}' k8s.io/api)
CODEGEN = $(shell pwd)/bin/code-generator
CODEGEN_ROOT = $(shell $(GO_CMD) env GOMODCACHE)/k8s.io/code-generator@$(CODEGEN_VERSION)
.PHONY: code-generator
code-generator:
	@GOBIN=$(PROJECT_DIR)/bin GO111MODULE=on $(GO_CMD) install k8s.io/code-generator/cmd/client-gen@$(CODEGEN_VERSION)
	cp -f $(CODEGEN_ROOT)/generate-groups.sh $(PROJECT_DIR)/bin/
	cp -f $(CODEGEN_ROOT)/generate-internal-groups.sh $(PROJECT_DIR)/bin/
	cp -f $(CODEGEN_ROOT)/kube_codegen.sh $(PROJECT_DIR)/bin/

##@ Release
.PHONY: artifacts
artifacts: kustomize helm yq
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	if [ -d artifacts ]; then rm -rf artifacts; fi
	mkdir -p artifacts
	$(KUSTOMIZE) build config/default -o artifacts/manifests.yaml
	@$(call clean-manifests)
	# Update the image tag and policy
	$(YQ)  e  '.image.manager.repository = "$(IMAGE_REPO)" | .image.manager.tag = "$(GIT_TAG)" | .image.manager.pullPolicy = "IfNotPresent"' -i charts/lws/values.yaml
	# create the package. TODO: consider signing it
	$(HELM) package --version $(GIT_TAG) --app-version $(GIT_TAG) charts/lws -d artifacts/
	mv artifacts/lws-$(GIT_TAG).tgz artifacts/lws-chart-$(GIT_TAG).tgz
	# Revert the image changes
	$(YQ)  e  '.image.manager.repository = "$(IMAGE_REGISTRY)/$(IMAGE_NAME)" | .image.manager.tag = "main" | .image.manager.pullPolicy = "Always"' -i charts/lws/values.yaml


.PHONY: prepare-release-branch
prepare-release-branch: yq kustomize ## Prepare the release branch with the release version.
	$(SED) -r 's/v[0-9]+\.[0-9]+\.[0-9]+/$(RELEASE_VERSION)/g' -i README.md -i site/config.toml
	$(SED) -r 's/--version="v?[0-9]+\.[0-9]+\.[0-9]+/--version="$(CHART_VERSION)/g' -i charts/lws/README.md
	$(SED) -r 's/\bVERSION=(\s*)v?[0-9]+\.[0-9]+\.[0-9]+\b/VERSION=\1$(RELEASE_VERSION)/g' -i site/content/en/docs/installation/_index.md
	$(SED) -r 's/\bCHART_VERSION=(\s*)v?[0-9]+\.[0-9]+\.[0-9]+\b/CHART_VERSION=\1$(CHART_VERSION)/g' -i site/content/en/docs/installation/_index.md
	$(YQ) e '.appVersion = "$(RELEASE_VERSION)"' -i charts/lws/Chart.yaml
	@$(call clean-manifests)

.PHONY: prometheus
prometheus:
	$(KUBECTL) apply --server-side -k config/prometheus

.PHONY: toc-update
toc-update:
	./hack/update-toc.sh

.PHONY: toc-verify
toc-verify:
	./hack/verify-toc.sh

.PHONY: generate-apiref
generate-apiref: genref
	cd $(PROJECT_DIR)/hack/genref/ && $(GENREF) -o $(PROJECT_DIR)/site/content/en/docs/reference

HELM = $(PROJECT_DIR)/bin/helm
.PHONY: helm
helm: ## Download helm locally if necessary.
	GOBIN=$(PROJECT_DIR)/bin GO111MODULE=on $(GO_CMD) install helm.sh/helm/v3/cmd/helm@$(HELM_VERSION)

YQ = $(PROJECT_DIR)/bin/yq
.PHONY: yq
yq: ## Download yq locally if necessary.
	GOBIN=$(PROJECT_DIR)/bin GO111MODULE=on $(GO_CMD) install github.com/mikefarah/yq/v4@v4.45.1

crds: kustomize yq # update helm CRD files
	$(KUSTOMIZE) build config/default \
	| $(YQ) 'select(.kind == "CustomResourceDefinition")' \
	> charts/lws/templates/crds/leaderworkerset.x-k8s.io_leaderworkersets.yaml
