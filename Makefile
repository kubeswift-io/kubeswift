# KubeSwift Makefile

IMAGE_TAG ?= latest
CONTROLLER_IMAGE ?= ghcr.io/projectbeskar/kubeswift/controller-manager:$(IMAGE_TAG)
SWIFTLETD_IMAGE ?= ghcr.io/projectbeskar/kubeswift/swiftletd:$(IMAGE_TAG)
IMAGE_REGISTRY ?= ghcr.io/projectbeskar/kubeswift
# Push destination: parent OCI repo only (Helm appends chart name from Chart.yaml)
CHART_OCI_PUSH ?= oci://ghcr.io/projectbeskar/charts
# Install/pull reference: full path including chart name
CHART_OCI_INSTALL ?= oci://ghcr.io/projectbeskar/charts/kubeswift

# Version stamping (defaults for local dev; overridden by release-* targets)
VERSION ?= dev
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "unknown")

.PHONY: build build-go build-rust build-images build-controller-image build-swiftletd-image \
	generate deploy undeploy load-images smoke-test smoke-test-cleanup preflight help \
	push-images package-chart push-chart release-dev release-rc release-stable print-version

help:
	@echo "Targets:"
	@echo "  build                 Build Go and Rust"
	@echo "  build-go              Build Go binaries (controller-manager, webhook-server, swiftctl)"
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
	@echo "  generate              Generate CRDs and deepcopy"
	@echo "  deploy                Apply CRDs and KubeSwift to cluster"
	@echo "  undeploy              Remove KubeSwift from cluster, then CRDs"
	@echo "  load-images           Load built images into kind/minikube (local clusters)"
	@echo "  smoke-test            Run boot smoke test (requires KubeSwift cluster)"
	@echo "  smoke-test-cleanup    Remove smoke-test resources (SwiftGuest, SwiftImage, etc.) for re-runs"
	@echo "  preflight             Run worker-node readiness preflight (host checks only)"

build: build-go build-rust

build-go:
	go build -ldflags "-X github.com/projectbeskar/kubeswift/internal/version.Version=$(VERSION) -X github.com/projectbeskar/kubeswift/internal/version.GitCommit=$(GIT_COMMIT) -X github.com/projectbeskar/kubeswift/internal/version.BuildDate=$(BUILD_DATE)" ./cmd/...

build-rust:
	cd rust && KUBESWIFT_VERSION="$(VERSION)" KUBESWIFT_GIT_COMMIT="$(GIT_COMMIT)" KUBESWIFT_BUILD_DATE="$(BUILD_DATE)" cargo build

build-images: build-controller-image build-swiftletd-image

build-controller-image:
	docker build -f images/controller-manager/Containerfile . -t $(CONTROLLER_IMAGE) \
		--build-arg VERSION=$(VERSION) --build-arg GIT_COMMIT=$(GIT_COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE)

build-swiftletd-image:
	docker build -f images/swiftletd/Containerfile rust/ -t $(SWIFTLETD_IMAGE) \
		--build-arg VERSION=$(VERSION) --build-arg GIT_COMMIT=$(GIT_COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE)

push-images: build-images
	docker push $(CONTROLLER_IMAGE)
	docker push $(SWIFTLETD_IMAGE)

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
	$(shell go env GOPATH)/bin/controller-gen object crd paths="./api/..." output:crd:dir=config/crd/bases

deploy: generate
	kubectl apply -k config/crd
	kubectl wait --for=condition=Established --timeout=30s crd/swiftguests.swift.kubeswift.io
	kubectl wait --for=condition=Established --timeout=30s crd/swiftimages.image.kubeswift.io
	kubectl wait --for=condition=Established --timeout=30s crd/swiftseedprofiles.seed.kubeswift.io
	kubectl wait --for=condition=Established --timeout=30s crd/swiftguestclasses.swift.kubeswift.io
	kubectl apply -k config/default

undeploy:
	kubectl delete -k config/default --ignore-not-found --timeout=60s
	kubectl delete -k config/crd --ignore-not-found --timeout=60s

load-images:
	@if command -v kind >/dev/null 2>&1; then \
		kind load docker-image $(CONTROLLER_IMAGE) $(SWIFTLETD_IMAGE); \
	elif command -v minikube >/dev/null 2>&1; then \
		minikube image load $(CONTROLLER_IMAGE); \
		minikube image load $(SWIFTLETD_IMAGE); \
	else \
		echo "Install kind or minikube for load-images"; exit 1; \
	fi

smoke-test:
	@test/smoke/boot-test.sh --no-cleanup

# smoke-test-cleanup removes resources created by the smoke test so you can re-run cleanly.
# Uses NAMESPACE (default: default). Run: make smoke-test-cleanup NAMESPACE=myns
smoke-test-cleanup:
	@NS="$${NAMESPACE:-default}"; \
	echo "Cleaning up smoke-test resources in namespace $$NS..."; \
	kubectl delete swiftguest sample -n "$$NS" --ignore-not-found --wait --timeout=60s 2>/dev/null || true; \
	kubectl delete swiftimage ubuntu-cloud -n "$$NS" --ignore-not-found --wait --timeout=60s 2>/dev/null || true; \
	kubectl delete swiftseedprofile minimal -n "$$NS" --ignore-not-found --wait --timeout=30s 2>/dev/null || true; \
	kubectl delete swiftguestclass default -n "$$NS" --ignore-not-found --wait --timeout=30s 2>/dev/null || true; \
	kubectl delete job swiftimage-import-ubuntu-cloud -n "$$NS" --ignore-not-found --wait --timeout=30s 2>/dev/null || true; \
	kubectl delete pvc swiftimage-import-ubuntu-cloud -n "$$NS" --ignore-not-found --wait --timeout=30s 2>/dev/null || true; \
	kubectl delete configmap sample-seed sample-runtime-intent -n "$$NS" --ignore-not-found --timeout=10s 2>/dev/null || true; \
	echo "Smoke-test cleanup done"

preflight:
	@./scripts/kubeswift-preflight.sh
