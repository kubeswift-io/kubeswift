# KubeSwift Makefile

# IMAGE_TAG defaults to sha-<short-git-hash> matching CI's tagging convention.
# Override with: make deploy IMAGE_TAG=v1.0.0
IMAGE_TAG ?= sha-$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
CONTROLLER_IMAGE ?= ghcr.io/projectbeskar/kubeswift/controller-manager:$(IMAGE_TAG)
SWIFTLETD_IMAGE ?= ghcr.io/projectbeskar/kubeswift/swiftletd:$(IMAGE_TAG)
GPU_DISCOVERY_IMAGE ?= ghcr.io/projectbeskar/kubeswift/gpu-discovery:$(IMAGE_TAG)
IMAGE_REGISTRY ?= ghcr.io/projectbeskar/kubeswift
# Push destination: parent OCI repo only (Helm appends chart name from Chart.yaml)
CHART_OCI_PUSH ?= oci://ghcr.io/projectbeskar/charts
# Install/pull reference: full path including chart name
CHART_OCI_INSTALL ?= oci://ghcr.io/projectbeskar/charts/kubeswift

# controller-gen is PINNED so `make generate` is reproducible regardless of
# whatever version a developer has on PATH. An unpinned binary churns the
# `controller-gen.kubebuilder.io/version` annotation on all 11 CRDs (and any
# schema differences between versions) into unrelated diffs. Bumping is a
# deliberate action: change CONTROLLER_TOOLS_VERSION, run `make generate`,
# and commit the regenerated CRDs as their own change. `go run ...@version`
# pins without depending on a prior `go install`.
CONTROLLER_TOOLS_VERSION ?= v0.20.1
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

# Version stamping (defaults for local dev; overridden by release-* targets)
VERSION ?= dev
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "unknown")

.PHONY: build build-go build-rust build-images build-controller-image build-swiftletd-image \
	build-gpu-discovery-image generate deploy deploy-with-webhook undeploy load-images smoke-test smoke-test-cleanup \
	clonestrategy-test snapshot-test local-roundtrip-test local-clone-identity-test \
	b0-cross-node-tcp-test e2e-tests \
	verify-e2e-scripts \
	preflight help push-images package-chart push-chart release-dev release-rc release-stable print-version

help:
	@echo "Targets:"
	@echo "  build                 Build Go and Rust"
	@echo "  build-go              Build Go binaries (controller-manager, swiftctl, gpu-discovery)"
	@echo "  build-rust            Build Rust crates"
	@echo "  build-images          Build all container images (with version stamping)"
	@echo "  build-controller-image  Build controller-manager image"
	@echo "  build-swiftletd-image Build swiftletd image"
	@echo "  push-images           Push images to registry (requires auth)"
	@echo "  package-chart         Package Helm chart"
	@echo "  push-chart            Push chart to OCI registry"
	@echo "  release-dev           Build, push images + chart (dev: sha-<shortsha>)"
	@echo "  release-rc            Build, push images + chart (RC tag)"
	@echo "  release-stable        Build, push images + chart (stable tag) + GitHub Release"
	@echo "  print-version         Print version info from hack/version.sh"
	@echo "  generate              Generate CRDs and deepcopy (also syncs charts/ + runs verify-crd-sync)"
	@echo "  verify-crd-sync       Fail if config/crd/kustomization.yaml drifts from config/crd/bases/"
	@echo "  deploy                Deploy controller-manager (minimal install, no webhooks). Use deploy-with-webhook for webhook-enabled deploys (requires cert-manager)."
	@echo "  deploy-with-webhook   Deploy controller-manager with admission webhooks enabled (requires cert-manager cluster-side; applies config/overlays/webhook on top of the minimal install)"
	@echo "  undeploy              Remove KubeSwift from cluster, then CRDs"
	@echo "  load-images           Load built images into kind/minikube (local clusters)"
	@echo "  smoke-test            Run boot smoke test (requires KubeSwift cluster)"
	@echo "  smoke-test-cleanup    Remove smoke-test resources (SwiftGuest, SwiftImage, etc.) for re-runs"
	@echo "  clonestrategy-test    Run cloneStrategy: snapshot e2e (requires snapshot-capable CSI)"
	@echo "  snapshot-test         Run Tier A (CSI VolumeSnapshot) snapshot+restore e2e"
	@echo "  local-roundtrip-test  Run Tier B (local hostPath) memory snapshot+in-place restore e2e"
	@echo "  local-clone-identity-test  Run Tier B clone-identity-collision e2e"
	@echo "  b0-cross-node-tcp-test     Run B0 cross-node TCP regression test (requires 2+ kernel-nodes)"
	@echo "  e2e-tests             Run every cluster-side e2e in sequence"
	@echo "  verify-e2e-scripts    Static check (bash -n) of every e2e script (fast, no cluster)"
	@echo "  preflight             Run worker-node readiness preflight (host checks only)"

build: build-go build-rust

build-go:
	go build -ldflags "-X github.com/projectbeskar/kubeswift/internal/version.Version=$(VERSION) -X github.com/projectbeskar/kubeswift/internal/version.GitCommit=$(GIT_COMMIT) -X github.com/projectbeskar/kubeswift/internal/version.BuildDate=$(BUILD_DATE)" ./cmd/...

build-rust:
	cd rust && KUBESWIFT_VERSION="$(VERSION)" KUBESWIFT_GIT_COMMIT="$(GIT_COMMIT)" KUBESWIFT_BUILD_DATE="$(BUILD_DATE)" cargo build

build-images: build-controller-image build-swiftletd-image build-gpu-discovery-image

build-controller-image:
	docker build -f images/controller-manager/Containerfile . -t $(CONTROLLER_IMAGE) \
		--build-arg VERSION=$(VERSION) --build-arg GIT_COMMIT=$(GIT_COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE)

build-swiftletd-image:
	docker build -f images/swiftletd/Containerfile . -t $(SWIFTLETD_IMAGE) \
		--build-arg VERSION=$(VERSION) --build-arg GIT_COMMIT=$(GIT_COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE)

build-gpu-discovery-image:
	docker build -f images/gpu-discovery/Containerfile . -t $(GPU_DISCOVERY_IMAGE) \
		--build-arg VERSION=$(VERSION) --build-arg GIT_COMMIT=$(GIT_COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE)

push-images: build-images
	docker push $(CONTROLLER_IMAGE)
	docker push $(SWIFTLETD_IMAGE)
	docker push $(GPU_DISCOVERY_IMAGE)

package-chart:
	@CHART_VER="$${CHART_VERSION:-$$(./hack/chart-version.sh dev 2>/dev/null || echo "0.0.0-dev.unknown")}"; \
	helm package charts/kubeswift --version "$$CHART_VER" --app-version "$$CHART_VER"

push-chart: package-chart
	@CHART_VER="$${CHART_VERSION:-$$(./hack/chart-version.sh dev 2>/dev/null || echo "0.0.0-dev.unknown")}"; \
	helm push kubeswift-$$CHART_VER.tgz $(CHART_OCI_PUSH)

print-version:
	@eval $$(./hack/version.sh) && \
	echo "VERSION=$$VERSION" && \
	echo "VERSION_TAG=$$VERSION_TAG" && \
	echo "GIT_COMMIT=$$GIT_COMMIT" && \
	echo "GIT_COMMIT_SHORT=$$GIT_COMMIT_SHORT" && \
	echo "IMAGE_TAG=$$IMAGE_TAG" && \
	echo "CHART_VERSION=$$CHART_VERSION"

release-dev:
	@eval $$(./hack/version.sh) && \
	$(MAKE) build-images VERSION="$$VERSION" GIT_COMMIT="$$GIT_COMMIT" IMAGE_TAG="$$IMAGE_TAG" && \
	$(MAKE) push-images IMAGE_TAG="$$IMAGE_TAG" && \
	CHART_VER="$$CHART_VERSION" && \
	helm package charts/kubeswift --version "$$CHART_VER" --app-version "$$CHART_VER" && \
	helm push kubeswift-$$CHART_VER.tgz $(CHART_OCI_PUSH)

release-rc:
	@eval $$(./hack/version.sh) && \
	$(MAKE) build-images VERSION="$$VERSION" GIT_COMMIT="$$GIT_COMMIT" IMAGE_TAG="$$IMAGE_TAG" && \
	$(MAKE) push-images IMAGE_TAG="$$IMAGE_TAG" && \
	CHART_VER="$$CHART_VERSION" && \
	helm package charts/kubeswift --version "$$CHART_VER" --app-version "$$CHART_VER" && \
	helm push kubeswift-$$CHART_VER.tgz $(CHART_OCI_PUSH)

release-stable:
	@eval $$(./hack/version.sh) && \
	$(MAKE) build-images VERSION="$$VERSION" GIT_COMMIT="$$GIT_COMMIT" IMAGE_TAG="$$IMAGE_TAG" && \
	$(MAKE) push-images IMAGE_TAG="$$IMAGE_TAG" && \
	CHART_VER="$$CHART_VERSION" && \
	helm package charts/kubeswift --version "$$CHART_VER" --app-version "$$CHART_VER" && \
	helm push kubeswift-$$CHART_VER.tgz $(CHART_OCI_PUSH) && \
	echo "Create GitHub Release: gh release create $(VERSION_TAG) --generate-notes"

generate:
	$(CONTROLLER_GEN) object crd paths="./api/..." output:crd:dir=config/crd/bases
	@# After regen, copy the canonical CRDs into the Helm chart and run
	@# the sync check. Without these two steps, deploys silently skip
	@# new CRDs (the bug that produced the Phase 1 cluster gap).
	cp config/crd/bases/*.yaml charts/kubeswift/crds/
	./hack/verify-crd-sync.sh

verify-crd-sync:
	./hack/verify-crd-sync.sh

deploy: generate verify-crd-sync
	@echo "Deploying with IMAGE_TAG=$(IMAGE_TAG)"
	kubectl apply -k config/crd
	kubectl wait --for=condition=Established --timeout=30s crd/swiftguests.swift.kubeswift.io
	kubectl wait --for=condition=Established --timeout=30s crd/swiftimages.image.kubeswift.io
	kubectl wait --for=condition=Established --timeout=30s crd/swiftkernels.kernel.kubeswift.io
	kubectl wait --for=condition=Established --timeout=30s crd/swiftseedprofiles.seed.kubeswift.io
	kubectl wait --for=condition=Established --timeout=30s crd/swiftguestclasses.swift.kubeswift.io
	kubectl wait --for=condition=Established --timeout=30s crd/swiftgpuprofiles.gpu.kubeswift.io
	kubectl wait --for=condition=Established --timeout=30s crd/swiftgpunodes.gpu.kubeswift.io
	kubectl wait --for=condition=Established --timeout=30s crd/swiftguestpools.swift.kubeswift.io
	@# Set controller-manager image tag via kustomize, apply, then reset.
	cd config/manager && sed -i 's/newTag: .*/newTag: $(IMAGE_TAG)/' kustomization.yaml
	kubectl apply -k config/default
	cd config/manager && sed -i 's/newTag: .*/newTag: latest/' kustomization.yaml
	@# Set KUBESWIFT_LAUNCHER_IMAGE so the controller creates pods with the correct swiftletd image.
	kubectl set env deployment/controller-manager -n kubeswift-system \
		KUBESWIFT_LAUNCHER_IMAGE=$(IMAGE_REGISTRY)/swiftletd:$(IMAGE_TAG)
	@# GPU discovery: set image tag and deploy RBAC + DaemonSet.
	kubectl apply -f config/rbac/gpu-discovery-rbac.yaml
	sed 's|gpu-discovery:latest|gpu-discovery:$(IMAGE_TAG)|' config/daemonset/gpu-discovery.yaml | kubectl apply -f -

# deploy-with-webhook layers config/overlays/webhook on top of the minimal
# install. The overlay composes config/default + config/webhook and patches
# the controller-manager Deployment to --webhook-enabled=true with the
# cert-manager TLS Secret volumeMount. Requires cert-manager installed
# cluster-side (the overlay's Certificate + Issuer reference cert-manager
# CRDs).
#
# Operators reaching for an apply path that doesn't strand the cluster's
# ValidatingWebhookConfiguration / MutatingWebhookConfiguration resources
# pointing at a webhook-disabled controller (TFU-16 walkthrough finding):
# use this target instead of `make deploy` when the cluster has the
# webhook resources installed.
deploy-with-webhook: deploy
	@echo "Layering webhook overlay (patches deployment to --webhook-enabled=true + cert volume; applies config/webhook resources)"
	@# Same IMAGE_TAG sed-patch trick as `deploy` — the overlay composes
	@# config/manager, so kustomize re-renders manager with the patch on top.
	cd config/manager && sed -i 's/newTag: .*/newTag: $(IMAGE_TAG)/' kustomization.yaml
	kubectl apply -k config/overlays/webhook
	cd config/manager && sed -i 's/newTag: .*/newTag: latest/' kustomization.yaml
	@# Re-set the launcher image env var — the overlay's deployment patch
	@# replaces the spec.template.spec.containers[].args list, which
	@# triggers a rollout. The env var set by `deploy` survives across
	@# kustomize-apply because env is not declared in deployment.yaml,
	@# but re-set defensively to handle the case where it was cleared.
	kubectl set env deployment/controller-manager -n kubeswift-system \
		KUBESWIFT_LAUNCHER_IMAGE=$(IMAGE_REGISTRY)/swiftletd:$(IMAGE_TAG)
	kubectl -n kubeswift-system rollout status deploy/controller-manager --timeout=120s

undeploy:
	kubectl delete -k config/default --ignore-not-found --timeout=60s
	kubectl delete -k config/crd --ignore-not-found --timeout=60s

load-images:
	@if command -v kind >/dev/null 2>&1; then \
		kind load docker-image $(CONTROLLER_IMAGE) $(SWIFTLETD_IMAGE) $(GPU_DISCOVERY_IMAGE); \
	elif command -v minikube >/dev/null 2>&1; then \
		minikube image load $(CONTROLLER_IMAGE); \
		minikube image load $(SWIFTLETD_IMAGE); \
		minikube image load $(GPU_DISCOVERY_IMAGE); \
	else \
		echo "Install kind or minikube for load-images"; exit 1; \
	fi

smoke-test:
	@test/smoke/boot-test.sh --no-cleanup

# Phase 2 live-migration manual demo (test surface — NOT a usable
# migration workflow). Drives the swiftletd send/receive primitives
# end-to-end via kubectl annotate; does NOT go through the
# SwiftMigration controller. See test/migration/manual/README.md for
# the security banner and prerequisites.
#
# Required env: SWIFTGUEST=<name> TARGET_NODE=<hostname>
# Optional env: NAMESPACE=<ns> (default: default)
migration-phase2-manual:
	@if [ -z "$$SWIFTGUEST" ]; then echo "SWIFTGUEST=<name> is required"; exit 1; fi
	@if [ -z "$$TARGET_NODE" ]; then echo "TARGET_NODE=<hostname> is required"; exit 1; fi
	@cd test/migration/manual && \
		SWIFTGUEST=$$SWIFTGUEST NAMESPACE=$${NAMESPACE:-default} ./source.sh && \
		TARGET_NODE=$$TARGET_NODE ./destination.sh && \
		./run.sh && \
		./verify.sh

# smoke-test-cleanup removes resources created by all smoke test scenarios.
# Uses NAMESPACE (default: default). Run: make smoke-test-cleanup NAMESPACE=myns
smoke-test-cleanup:
	@test/smoke/boot-test.sh --cleanup-only

# Tier A (csi-volume-snapshot) end-to-end: source guest, snapshot, restore,
# verify the per-guest PVC carries dataSource and the restore-seeded label.
# Caught the Tier A data-loss bug fixed in PR #21 — would have caught it
# from the day SwiftRestore was added had it run in CI.
snapshot-test:
	@test/snapshot/snapshot-test.sh

# cloneStrategy: snapshot end-to-end: SwiftImage with status.cloneSeed,
# fast per-guest PVC clone via dataSource: VolumeSnapshot.
clonestrategy-test:
	@test/clonestrategy/clonestrategy-test.sh

# Tier B (local hostPath) memory snapshot + in-place restore: tmpfs
# sentinel survives the kill+restore cycle.
local-roundtrip-test:
	@test/snapshot/local-roundtrip-test.sh

# Tier B clone restore: machine-id, hostname, SSH host keys, guest-side
# MAC are documented to collide between source and clones (resume-vs-boot
# limitation). The test asserts that the documented behavior is what
# operators observe.
local-clone-identity-test:
	@test/snapshot/local-clone-identity-test.sh

# B0 regression: cross-node pod-to-pod TCP from launcher pods. Catches
# any revert of the launcher br0 default subnet to a value that would
# collide with the cluster's per-node Calico pod CIDR allocations.
# See docs/design/live-migration-phase-3a-spike.md (B0 finding).
b0-cross-node-tcp-test:
	@test/networking/b0-cross-node-tcp.sh validate

# Every cluster-side e2e in sequence. Each script accepts --no-cleanup;
# this target opts out so the cluster is clean between scripts.
e2e-tests: smoke-test snapshot-test clonestrategy-test local-roundtrip-test local-clone-identity-test b0-cross-node-tcp-test

# Fast static check: every e2e script parses (bash -n). Catches
# typos / unclosed quotes without needing a cluster. Designed to run
# on every PR.
verify-e2e-scripts:
	@set -e; for script in $$(find test -name '*.sh' -type f); do \
		echo "  bash -n $$script"; \
		bash -n "$$script" || { echo "FAIL: $$script has syntax errors"; exit 1; }; \
	done; \
	echo "  all e2e scripts parse"

preflight:
	@./scripts/kubeswift-preflight.sh
