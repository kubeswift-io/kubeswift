# KubeSwift Makefile

.PHONY: build build-go build-rust build-swiftletd-image generate smoke-test preflight help

help:
	@echo "Targets:"
	@echo "  build              Build Go and Rust"
	@echo "  build-go           Build Go binaries"
	@echo "  build-rust         Build Rust crates"
	@echo "  build-swiftletd-image  Build swiftletd container image"
	@echo "  generate           Generate CRDs and deepcopy"
	@echo "  smoke-test         Run boot smoke test (requires KubeSwift cluster)"
	@echo "  preflight          Run worker-node readiness preflight (host checks only)"

build: build-go build-rust

build-go:
	go build ./cmd/...

build-rust:
	cd rust && cargo build

build-swiftletd-image:
	docker build -f images/swiftletd/Containerfile rust/ -t ghcr.io/projectbeskar/kubeswift/swiftletd:latest

generate:
	$(shell go env GOPATH)/bin/controller-gen object crd paths="./api/..." output:crd:dir=config/crd/bases

smoke-test:
	@test/smoke/boot-test.sh

preflight:
	@./scripts/kubeswift-preflight.sh
