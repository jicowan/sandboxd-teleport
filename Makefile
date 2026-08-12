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
MICROVM_WORKER_REPO ?= sandboxd-microvm
BROKER_REPO   ?= aio-sandbox-broker-sandboxd

# Image tag: default to the short git SHA; images are also pushed as :latest.
TAG           ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

# Pinned runsc version — MUST match the on-node runsc, or checkpoint/restore fails.
# gVisor's release bucket is keyed by the bare date (20260622), not the full tag
# (release-20260622.0), so derive the date for the download URL.
RUNSC_VERSION ?= release-20260622.0
RUNSC_ARCH    ?= x86_64
RUNSC_DATE     = $(shell echo "$(RUNSC_VERSION)" | sed -E 's/release-([0-9]+).*/\1/')
RUNSC_URL      = https://storage.googleapis.com/gvisor/releases/release/$(RUNSC_DATE)/$(RUNSC_ARCH)/runsc

# Pinned microVM runtime versions — the CH microVM worker (SANDBOXD_RUNTIME=microvm)
# bundles cloud-hypervisor + the kata-static guest kernel/rootfs. Live-validated on a
# nested-virtualization node (see PRD-microvm-runtime-cloud-hypervisor.md).
CH_VERSION    ?= v53.0
KATA_VERSION  ?= 4.0.0
CH_URL         = https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$(CH_VERSION)/cloud-hypervisor-static
KATA_URL       = https://github.com/kata-containers/kata-containers/releases/download/$(KATA_VERSION)/kata-static-$(KATA_VERSION)-amd64.tar.zst

# virtiofsd is pinned SEPARATELY, NOT taken from the kata-static bundle: kata-static
# 4.0.0 ships virtiofsd v1.13.x, whose old vhost-user (pre vhost-0.16 /
# vhost-user-backend-0.22) HANGS cloud-hypervisor's snapshot/restore migration
# handshake — CH deadlocks in the vhost-user-fs find-paths exchange (live-diagnosed:
# CH threads block in futex_wait/unix_stream_data_wait, virtiofsd at 0 CPU). v1.14.0
# is the first release with the fix. Substrate pins the same for the same reason.
# The x86_64-musl binary attached to the v1.14.0 release. The /-/project/<id>/uploads/
# form is the canonical UNAUTHENTICATED URL (21523468 = gitlab project id of
# virtio-fs/virtiofsd); the /-/releases/.../downloads/ and /<path>/-/uploads/ forms 403
# outside a browser session. Sha pinned because a bad URL silently yields a login page.
# Zip layout: target/x86_64-unknown-linux-musl/release/virtiofsd.
VIRTIOFSD_VERSION  ?= v1.14.0
VIRTIOFSD_URL       = https://gitlab.com/-/project/21523468/uploads/f505704014ae7a816e515f2a05a93d8b/virtiofsd-v1.14.0.zip
VIRTIOFSD_SHA256    = 2e4fe9571f492b00baa34bc4e708e950039c5da05b830b31a8d179cb6ac8978e

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
	@for r in $(OPERATOR_REPO) $(ROUTER_REPO) $(WORKER_REPO) $(MICROVM_WORKER_REPO) $(BROKER_REPO); do \
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

# ---- microVM worker (SANDBOXD_RUNTIME=microvm) --------------------------------
.PHONY: fetch-microvm-assets image-worker-microvm
fetch-microvm-assets: ## Download cloud-hypervisor + kata guest kernel/rootfs + virtiofsd v1.14 -> dist/microvm-worker/
	@mkdir -p $(DIST)/microvm-worker
	@echo "fetching cloud-hypervisor $(CH_VERSION)"
	curl -fsSL -o $(DIST)/microvm-worker/cloud-hypervisor "$(CH_URL)"
	@echo "fetching kata-static $(KATA_VERSION) (guest kernel + rootfs image)"
	curl -fsSL -o $(DIST)/microvm-worker/kata.tar.zst "$(KATA_URL)"
	@rm -rf $(DIST)/microvm-worker/kata && mkdir -p $(DIST)/microvm-worker/kata
	tar --zstd -xf $(DIST)/microvm-worker/kata.tar.zst -C $(DIST)/microvm-worker/kata
	@# Resolve the kata symlinks to the real kernel + rootfs image and stage them flat
	@# in the build context (Docker COPY doesn't follow symlinks well).
	cp -L $(DIST)/microvm-worker/kata/opt/kata/share/kata-containers/vmlinux.container $(DIST)/microvm-worker/vmlinux.container
	cp -L $(DIST)/microvm-worker/kata/opt/kata/share/kata-containers/kata-containers.img $(DIST)/microvm-worker/kata-containers.img
	@# virtiofsd v1.14.0 from upstream — NOT the kata bundle's v1.13.x, which hangs CH's
	@# snapshot/restore migration handshake (see VIRTIOFSD_* above). Verify the sha
	@# (a bad URL silently returns a login page), then extract the musl static binary.
	@echo "fetching virtiofsd $(VIRTIOFSD_VERSION) (upstream; kata's v1.13 breaks restore)"
	curl -fsSL -o $(DIST)/microvm-worker/virtiofsd.zip "$(VIRTIOFSD_URL)"
	@echo "$(VIRTIOFSD_SHA256)  $(DIST)/microvm-worker/virtiofsd.zip" | shasum -a 256 -c -
	@rm -rf $(DIST)/microvm-worker/vfsd && mkdir -p $(DIST)/microvm-worker/vfsd
	unzip -q -o $(DIST)/microvm-worker/virtiofsd.zip -d $(DIST)/microvm-worker/vfsd
	cp $(DIST)/microvm-worker/vfsd/target/x86_64-unknown-linux-musl/release/virtiofsd $(DIST)/microvm-worker/virtiofsd
	@chmod +x $(DIST)/microvm-worker/cloud-hypervisor $(DIST)/microvm-worker/virtiofsd
	@rm -rf $(DIST)/microvm-worker/kata $(DIST)/microvm-worker/kata.tar.zst $(DIST)/microvm-worker/vfsd $(DIST)/microvm-worker/virtiofsd.zip
	@echo "microVM assets staged in $(DIST)/microvm-worker/"

image-worker-microvm: build-worker fetch-microvm-assets ecr-login ecr-repos ## Build + push the microVM worker image (sandboxd + CH + kata kernel/rootfs/virtiofsd)
	@cp $(DIST)/worker/sandboxd $(DIST)/microvm-worker/sandboxd
	docker build --platform=$(PLATFORM) -f build/docker/Dockerfile.worker.microvm \
		-t $(REGISTRY)/$(MICROVM_WORKER_REPO):$(TAG) -t $(REGISTRY)/$(MICROVM_WORKER_REPO):latest $(DIST)/microvm-worker
	docker push $(REGISTRY)/$(MICROVM_WORKER_REPO):$(TAG)
	docker push $(REGISTRY)/$(MICROVM_WORKER_REPO):latest

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
# AMI (Packer) — build the KVM-enabled node image for microVM sandboxes
# ============================================================================
PACKER_DIR    := packer
K8S_VERSION   ?= 1.31

.PHONY: ami-microvm ami-microvm-validate
ami-microvm: ## Build the KVM-enabled microVM node AMI (Packer; run on nested-virt Nitro or *.metal)
	cd $(PACKER_DIR) && packer init microvm-node.pkr.hcl && \
		packer build -var region=$(AWS_REGION) -var k8s_version=$(K8S_VERSION) microvm-node.pkr.hcl

ami-microvm-validate: ## Validate + fmt-check the Packer template (no build)
	cd $(PACKER_DIR) && packer init microvm-node.pkr.hcl && \
		packer fmt -check -diff microvm-node.pkr.hcl && \
		packer validate -var region=$(AWS_REGION) -var k8s_version=$(K8S_VERSION) microvm-node.pkr.hcl

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
