# KubeSwift sandbox kernel build
# Usage:
#   make setup      — clone buildroot into ./buildroot/
#   make build      — run buildroot, produce bzImage and rootfs.cpio.gz (bridge)
#   make verify     — boot the artifacts on cloud-hypervisor with an OCI rootfs
#   make clean      — clean buildroot output
#   make distclean  — remove buildroot checkout entirely
#
# The sandbox profile boots a direct kernel + this bridge-initramfs + an OCI
# image as the root filesystem (RO base + tmpfs overlay). See
# docs/design/swiftsandbox-design.md.
#
# PROFILE selects the kernel profile (default: sandbox). `gpu-sandbox` is the
# base sandbox config + loadable-module support (CONFIG_MODULES), so a GPU
# sandbox's OCI image can `insmod` the NVIDIA driver:
#   make build PROFILE=gpu-sandbox
# Each profile builds into its own OUTPUT_DIR (output / output-gpu-sandbox) so
# they don't clobber each other; override OUTPUT_DIR to reuse a built toolchain.

PROFILE           ?= sandbox
BUILDROOT_VERSION := 2024.02.6
BUILDROOT_DIR     := $(CURDIR)/buildroot
OUTPUT_DIR        ?= $(CURDIR)/output$(if $(filter-out sandbox,$(PROFILE)),-$(PROFILE))
EXTERNAL          := $(CURDIR)

.PHONY: setup build verify clean distclean

setup:
	@if [ ! -d "$(BUILDROOT_DIR)" ]; then \
		echo "Cloning buildroot $(BUILDROOT_VERSION)..."; \
		git clone --depth=1 --branch $(BUILDROOT_VERSION) \
			https://github.com/buildroot/buildroot.git $(BUILDROOT_DIR); \
	else \
		echo "buildroot already present at $(BUILDROOT_DIR)"; \
	fi

# The in-guest vsock control agent (swiftctl sandbox exec/attach) is baked into the
# initramfs via the buildroot rootfs-overlay. Built static from source here (a build
# artifact — gitignored, never committed). The bridge-init runs it in the background.
REPO_ROOT := $(CURDIR)/../../..
AGENT_BIN  := $(CURDIR)/rootfs-overlay/usr/local/bin/kubeswift-guest-agent

agent:
	@mkdir -p $(dir $(AGENT_BIN))
	cd $(REPO_ROOT) && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -ldflags="-s -w" -o $(AGENT_BIN) ./cmd/kubeswift-guest-agent
	@echo "  guest-agent: $(AGENT_BIN) ($$(du -h $(AGENT_BIN) | cut -f1))"

build: setup agent
	$(MAKE) -C $(BUILDROOT_DIR) \
		BR2_EXTERNAL=$(EXTERNAL) \
		O=$(OUTPUT_DIR) \
		$(PROFILE)_defconfig
	$(MAKE) -C $(BUILDROOT_DIR) \
		BR2_EXTERNAL=$(EXTERNAL) \
		O=$(OUTPUT_DIR) \
		-j$(shell nproc)
	@echo ""
	@echo "Build complete ($(PROFILE)):"
	@echo "  bzImage:           $(OUTPUT_DIR)/images/bzImage"
	@echo "  rootfs.cpio.gz:    $(OUTPUT_DIR)/images/rootfs.cpio.gz  (bridge-initramfs)"

verify:
	./verify-boot.sh

clean:
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUTPUT_DIR) clean

distclean:
	rm -rf $(BUILDROOT_DIR) $(OUTPUT_DIR)
