# KubeSwift faas-minimal kernel build
# Usage:
#   make setup      — clone buildroot into ./buildroot/
#   make build      — run buildroot, produce bzImage and initramfs.cpio.gz
#   make clean      — clean buildroot output
#   make distclean  — remove buildroot checkout entirely

BUILDROOT_VERSION := 2024.02.6
BUILDROOT_DIR     := $(CURDIR)/buildroot
OUTPUT_DIR        := $(CURDIR)/output
EXTERNAL          := $(CURDIR)

.PHONY: setup build clean distclean

setup:
	@if [ ! -d "$(BUILDROOT_DIR)" ]; then \
		echo "Cloning buildroot $(BUILDROOT_VERSION)..."; \
		git clone --depth=1 --branch $(BUILDROOT_VERSION) \
			https://github.com/buildroot/buildroot.git $(BUILDROOT_DIR); \
	else \
		echo "buildroot already present at $(BUILDROOT_DIR)"; \
	fi

build: setup
	$(MAKE) -C $(BUILDROOT_DIR) \
		BR2_EXTERNAL=$(EXTERNAL) \
		O=$(OUTPUT_DIR) \
		faas_minimal_defconfig
	$(MAKE) -C $(BUILDROOT_DIR) \
		BR2_EXTERNAL=$(EXTERNAL) \
		O=$(OUTPUT_DIR) \
		-j$(shell nproc)
	@echo ""
	@echo "Build complete:"
	@echo "  bzImage:           $(OUTPUT_DIR)/images/bzImage"
	@echo "  initramfs.cpio.gz: $(OUTPUT_DIR)/images/rootfs.cpio.gz"

clean:
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUTPUT_DIR) clean

distclean:
	rm -rf $(BUILDROOT_DIR) $(OUTPUT_DIR)