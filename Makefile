# KubeSwift Makefile

IMAGE_TAG ?= latest
CONTROLLER_IMAGE ?= ghcr.io/projectbeskar/kubeswift/controller-manager:$(IMAGE_TAG)
WEBHOOK_IMAGE ?= ghcr.io/projectbeskar/kubeswift/webhook-server:$(IMAGE_TAG)
SWIFTLETD_IMAGE ?= ghcr.io/projectbeskar/kubeswift/swiftletd:$(IMAGE_TAG)

.PHONY: build build-go build-rust build-images build-controller-image build-webhook-image build-swiftletd-image generate deploy undeploy load-images smoke-test preflight help

help:
	@echo "Targets:"
	@echo "  build                 Build Go and Rust"
	@echo "  build-go              Build Go binaries"
	@echo "  build-rust            Build Rust crates"
	@echo "  build-images          Build all container images"
	@echo "  build-controller-image  Build controller-manager image"
	@echo "  build-webhook-image   Build webhook-server image"
	@echo "  build-swiftletd-image Build swiftletd image"
	@echo "  generate              Generate CRDs and deepcopy"
	@echo "  deploy                Apply CRDs and KubeSwift to cluster"
	@echo "  undeploy              Remove KubeSwift from cluster, then CRDs"
	@echo "  load-images           Load built images into kind/minikube (local clusters)"
	@echo "  smoke-test            Run boot smoke test (requires KubeSwift cluster)"
	@echo "  preflight             Run worker-node readiness preflight (host checks only)"

build: build-go build-rust

build-go:
	go build ./cmd/...

build-rust:
	cd rust && cargo build

build-images: build-controller-image build-swiftletd-image

build-controller-image:
	docker build -f images/controller-manager/Containerfile . -t $(CONTROLLER_IMAGE)

build-webhook-image:
	docker build -f images/webhook-server/Containerfile . -t $(WEBHOOK_IMAGE)

build-swiftletd-image:
	docker build -f images/swiftletd/Containerfile rust/ -t $(SWIFTLETD_IMAGE)

generate:
	$(shell go env GOPATH)/bin/controller-gen object crd paths="./api/..." output:crd:dir=config/crd/bases

deploy:
	kubectl apply -k config/crd
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
	@test/smoke/boot-test.sh

preflight:
	@./scripts/kubeswift-preflight.sh
