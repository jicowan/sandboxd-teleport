# ============================================================================
# sandboxd-teleport — top-level Makefile
#
# One place to build/push the component images, provision AWS infra (Terraform),
# and install sandboxd onto an existing EKS cluster (Helm). Getting started:
#
#   make help                 # list targets
#   make infra                # provision S3 + IAM + Pod Identity (Terraform)
#   make images               # build + push operator/router/worker images to ECR
#   make install              # helm install sandboxd onto your (BYO) cluster
#
# The three components come from three Go modules under checkpoint-restore/:
#   operator  -> controlplane/cmd            (image repo: $(OPERATOR_REPO))
#   router    -> controlplane/cmd/router     (image repo: $(ROUTER_REPO))
#   worker    -> sandboxd/                   (image repo: $(WORKER_REPO)) + pinned runsc
#
# Images are built by COPYING prebuilt binaries into a distroless base (build/docker/*).
# By default `make images` builds the binaries locally (BINSRC=local). To package the
# binaries published by the release workflow instead: `make images BINSRC=release RELEASE=vX`.
# ============================================================================

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

# ---- Configuration (override on the CLI or via environment) ---------------
# ECR is the default registry. Set AWS_ACCOUNT + AWS_REGION for your account.
AWS_ACCOUNT   ?= $(shell aws sts get-caller-identity --query Account --output text 2>/dev/null)
AWS_REGION    ?= us-west-2
REGISTRY      ?= $(AWS_ACCOUNT).dkr.ecr.$(AWS_REGION).amazonaws.com

OPERATOR_REPO ?= sandboxd-operator
ROUTER_REPO   ?= sandboxd-router
WORKER_REPO   ?= sandboxd
BROKER_REPO   ?= aio-sandbox-broker-sandboxd

# Image tag: default to the short git SHA; images are also pushed as :latest.
TAG           ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

# Pinned runsc version — MUST match the on-node runsc, or checkpoint/restore fails.
RUNSC_VERSION ?= release-20260622.0
RUNSC_ARCH    ?= x86_64
RUNSC_URL      = https://storage.googleapis.com/gvisor/releases/release/$(RUNSC_VERSION)/$(RUNSC_ARCH)/runsc

# Binary source for image builds: local (go build here) | release (download from GH).
BINSRC        ?= local
RELEASE       ?=                       # required when BINSRC=release, e.g. v0.1.0
GH_REPO       ?= jicowan/sandboxd-teleport

# Build layout
DIST          := dist
PLATFORM      ?= linux/amd64
GOOS          := linux
GOARCH        := amd64

CP_DIR        := checkpoint-restore/controlplane
WK_DIR        := checkpoint-restore/sandboxd

# Helm / infra
CHART         := charts/sandboxd
NAMESPACE     ?= sandboxd-controlplane-system
RELEASE_NAME  ?= sandboxd
TF_DIR        := terraform

# ---- Help ------------------------------------------------------------------
.PHONY: help
help: ## Show this help
	@echo "sandboxd-teleport — packaging targets"
	@echo ""
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Key vars: REGISTRY=$(REGISTRY) TAG=$(TAG) BINSRC=$(BINSRC) RUNSC_VERSION=$(RUNSC_VERSION)"

# ============================================================================
# Binaries
# ============================================================================
.PHONY: build build-operator build-router build-worker fetch-runsc fetch-release

build: build-operator build-router build-worker ## Build all three component binaries (local)

build-operator: ## Build the operator binary -> dist/operator/manager
	@mkdir -p $(DIST)/operator
	cd $(CP_DIR) && CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -a -o $(CURDIR)/$(DIST)/operator/manager ./cmd
	@echo "built $(DIST)/operator/manager"

build-router: ## Build the router binary -> dist/router/manager
	@mkdir -p $(DIST)/router
	cd $(CP_DIR) && CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -a -o $(CURDIR)/$(DIST)/router/manager ./cmd/router
	@echo "built $(DIST)/router/manager"

build-worker: ## Build the sandboxd worker binary -> dist/worker/sandboxd
	@mkdir -p $(DIST)/worker
	cd $(WK_DIR) && CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -a -o $(CURDIR)/$(DIST)/worker/sandboxd ./
	@echo "built $(DIST)/worker/sandboxd"

fetch-runsc: ## Download the pinned runsc binary -> dist/worker/runsc
	@mkdir -p $(DIST)/worker
	@echo "fetching runsc $(RUNSC_VERSION) ($(RUNSC_ARCH))"
	curl -fsSL -o $(DIST)/worker/runsc "$(RUNSC_URL)"
	chmod +x $(DIST)/worker/runsc

fetch-release: ## Download prebuilt binaries from a GitHub release (needs RELEASE=vX)
	@test -n "$(RELEASE)" || { echo "ERROR: set RELEASE=<tag>, e.g. make fetch-release RELEASE=v0.1.0"; exit 1; }
	@mkdir -p $(DIST)/operator $(DIST)/router $(DIST)/worker
	gh release download $(RELEASE) --repo $(GH_REPO) --dir $(DIST) --clobber \
		--pattern 'operator' --pattern 'router' --pattern 'sandboxd' --pattern 'runsc'
	@# Lay the downloaded artifacts out where the Dockerfiles expect them.
	@cp -f $(DIST)/operator $(DIST)/operator/manager 2>/dev/null || mv -f $(DIST)/operator $(DIST)/operator-bin && mkdir -p $(DIST)/operator && mv -f $(DIST)/operator-bin $(DIST)/operator/manager
	@cp -f $(DIST)/router   $(DIST)/router/manager   2>/dev/null || mv -f $(DIST)/router   $(DIST)/router-bin   && mkdir -p $(DIST)/router   && mv -f $(DIST)/router-bin   $(DIST)/router/manager
	@mkdir -p $(DIST)/worker && mv -f $(DIST)/sandboxd $(DIST)/worker/sandboxd && mv -f $(DIST)/runsc $(DIST)/worker/runsc
	@chmod +x $(DIST)/worker/sandboxd $(DIST)/worker/runsc

# Internal: ensure the binaries exist for image builds, per BINSRC.
.PHONY: _ensure-bins
_ensure-bins:
ifeq ($(BINSRC),release)
	@$(MAKE) fetch-release
else
	@$(MAKE) build fetch-runsc
endif

# ============================================================================
# Images (ECR)
# ============================================================================
.PHONY: images image-operator image-router image-worker image-broker ecr-login ecr-repos

ecr-login: ## Log docker in to ECR
	@test -n "$(AWS_ACCOUNT)" || { echo "ERROR: AWS_ACCOUNT unset and no AWS creds to derive it"; exit 1; }
	aws ecr get-login-password --region $(AWS_REGION) | \
		docker login --username AWS --password-stdin $(REGISTRY)

ecr-repos: ## Ensure the ECR repositories exist (idempotent)
	@for r in $(OPERATOR_REPO) $(ROUTER_REPO) $(WORKER_REPO) $(BROKER_REPO); do \
		aws ecr describe-repositories --region $(AWS_REGION) --repository-names $$r >/dev/null 2>&1 || \
		aws ecr create-repository --region $(AWS_REGION) --repository-name $$r >/dev/null && echo "ensured ecr repo: $$r"; \
	done

images: _ensure-bins ecr-login ecr-repos image-operator image-router image-worker image-broker ## Build + push all component images to ECR
	@echo "pushed: $(REGISTRY)/{$(OPERATOR_REPO),$(ROUTER_REPO),$(WORKER_REPO),$(BROKER_REPO)}:$(TAG)"

image-operator: ## Build + push the operator image
	docker build --platform=$(PLATFORM) -f build/docker/Dockerfile.operator --build-arg BIN=manager \
		-t $(REGISTRY)/$(OPERATOR_REPO):$(TAG) -t $(REGISTRY)/$(OPERATOR_REPO):latest $(DIST)/operator
	docker push $(REGISTRY)/$(OPERATOR_REPO):$(TAG)
	docker push $(REGISTRY)/$(OPERATOR_REPO):latest

image-router: ## Build + push the router image
	docker build --platform=$(PLATFORM) -f build/docker/Dockerfile.operator --build-arg BIN=manager \
		-t $(REGISTRY)/$(ROUTER_REPO):$(TAG) -t $(REGISTRY)/$(ROUTER_REPO):latest $(DIST)/router
	docker push $(REGISTRY)/$(ROUTER_REPO):$(TAG)
	docker push $(REGISTRY)/$(ROUTER_REPO):latest

image-worker: ## Build + push the worker image (sandboxd + pinned runsc)
	docker build --platform=$(PLATFORM) -f build/docker/Dockerfile.worker \
		-t $(REGISTRY)/$(WORKER_REPO):$(TAG) -t $(REGISTRY)/$(WORKER_REPO):latest $(DIST)/worker
	docker push $(REGISTRY)/$(WORKER_REPO):$(TAG)
	docker push $(REGISTRY)/$(WORKER_REPO):latest

# The broker is Python — no prebuilt binary/release artifact. Built from source
# in its own context (broker/), so it does NOT depend on _ensure-bins.
image-broker: ecr-login ecr-repos ## Build + push the broker image (Python, from source)
	docker build --platform=$(PLATFORM) -f broker/Dockerfile.sandboxd \
		-t $(REGISTRY)/$(BROKER_REPO):$(TAG) -t $(REGISTRY)/$(BROKER_REPO):latest broker
	docker push $(REGISTRY)/$(BROKER_REPO):$(TAG)
	docker push $(REGISTRY)/$(BROKER_REPO):latest

# ============================================================================
# Infra (Terraform) — provisions S3 + IAM + Pod Identity for a BYO cluster
# ============================================================================
.PHONY: infra infra-plan infra-destroy
infra: ## terraform apply — S3 bucket + IAM roles + Pod Identity (needs CLUSTER_NAME)
	cd $(TF_DIR) && terraform init -input=false && terraform apply -auto-approve \
		-var="region=$(AWS_REGION)" $(if $(CLUSTER_NAME),-var="cluster_name=$(CLUSTER_NAME)")

infra-plan: ## terraform plan
	cd $(TF_DIR) && terraform init -input=false && terraform plan \
		-var="region=$(AWS_REGION)" $(if $(CLUSTER_NAME),-var="cluster_name=$(CLUSTER_NAME)")

infra-destroy: ## terraform destroy (tears down S3/IAM/Pod Identity — NOT the cluster)
	cd $(TF_DIR) && terraform destroy \
		-var="region=$(AWS_REGION)" $(if $(CLUSTER_NAME),-var="cluster_name=$(CLUSTER_NAME)")

# ============================================================================
# Install (Helm) — onto an existing cluster
# ============================================================================
.PHONY: install upgrade uninstall lint template
install: ## helm install sandboxd (control plane) — set REGISTRY/TAG/bucket via --set or values
	helm upgrade --install $(RELEASE_NAME) $(CHART) --namespace $(NAMESPACE) --create-namespace \
		--set image.registry=$(REGISTRY) --set image.tag=$(TAG) \
		--wait --timeout 5m   # block until valkey/operator/router Deployments are Ready

upgrade: install ## alias for install (helm upgrade --install)

uninstall: ## helm uninstall sandboxd
	helm uninstall $(RELEASE_NAME) --namespace $(NAMESPACE)

lint: ## helm lint the chart
	helm lint $(CHART)

template: ## render the chart to stdout (dry-run)
	helm template $(RELEASE_NAME) $(CHART) --namespace $(NAMESPACE) \
		--set image.registry=$(REGISTRY) --set image.tag=$(TAG)

# ============================================================================
.PHONY: clean
clean: ## remove dist/ build artifacts
	rm -rf $(DIST)
